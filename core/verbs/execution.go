package verbs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/flow"
	"litopys.smart-gw.co.ua/core/policy"
)

// Executions of workflows: a run of one frozen version; steps are taken as attempts with a
// lease, human steps and approval points wait for a GO of a person. Step states come from core/flow.

var executionOutcomes = []string{"completed", "failed", "canceled"}
var attemptStates = []string{"completed", "failed", "canceled", "rejected"}

// execState is an execution with its definition and derived state, read inside the command's transaction.
type execState struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	Open      bool
	Params    map[string]any
	Def       flow.Definition
	Attempts  []flow.Attempt
	Approvals []flow.Approval
	State     flow.State
}

// loadExec locks the execution row (FOR UPDATE: step commands of one run serialize) and derives its state.
func (c *cmd) loadExec(id uuid.UUID, now time.Time) (*execState, error) {
	x := &execState{ID: id}
	var state string
	var params, def []byte
	err := c.tx.QueryRow(c.ctx, `SELECT e.project_id, e.state, e.params, v.definition FROM projection.execution e
	        JOIN projection.workflow_version v ON v.workflow_id = e.workflow_id AND v.version = e.version
	       WHERE e.execution_id = $1 FOR UPDATE OF e`, id).Scan(&x.ProjectID, &state, &params, &def)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail("execution_not_found", "execution %s", id)
	}
	if err != nil {
		return nil, err
	}
	x.Open = state == "working"
	if len(params) > 0 {
		if err := json.Unmarshal(params, &x.Params); err != nil {
			return nil, err
		}
	}
	if x.Def, err = flow.ParseDefinition(def); err != nil {
		return nil, err
	}
	rows, err := c.tx.Query(c.ctx, `SELECT step_key, n, state, lease_until, output FROM projection.step_attempt WHERE execution_id = $1`, id)
	if err != nil {
		return nil, err
	}
	if x.Attempts, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (flow.Attempt, error) {
		var a flow.Attempt
		var out []byte
		err := r.Scan(&a.Key, &a.N, &a.State, &a.LeaseUntil, &out)
		a.Output = out
		return a, err
	}); err != nil {
		return nil, err
	}
	rows, err = c.tx.Query(c.ctx, `SELECT step_key, used_by IS NOT NULL FROM projection.approval WHERE execution_id = $1 AND NOT revoked`, id)
	if err != nil {
		return nil, err
	}
	if x.Approvals, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (flow.Approval, error) {
		var a flow.Approval
		return a, r.Scan(&a.Key, &a.Used)
	}); err != nil {
		return nil, err
	}
	x.State, err = flow.Derive(x.Def, x.Params, x.Attempts, x.Approvals, now)
	return x, err
}

func (x *execState) step(key string) (flow.Step, bool) {
	for _, s := range x.Def.Steps {
		if s.Key == key {
			return s, true
		}
	}
	return flow.Step{}, false
}

func ready(st flow.State) []string {
	if st.ReadyFor == nil {
		return []string{}
	}
	return st.ReadyFor
}

// closeIfDone emits execution.closed (completed) when every step is completed or skipped.
func (c *cmd) closeIfDone(id uuid.UUID, now time.Time) (bool, []string, error) {
	x, err := c.loadExec(id, now)
	if err != nil {
		return false, nil, err
	}
	if !x.State.Done {
		return false, ready(x.State), nil
	}
	_, err = c.emit("execution.closed", "execution", id, nil, nil, map[string]any{"outcome": "completed", "summary": "every step completed or skipped"})
	return err == nil, []string{}, err
}

func (c *cmd) actorKind() (string, error) {
	var kind string
	err := c.tx.QueryRow(c.ctx, `SELECT kind FROM projection.actor WHERE actor_id = $1`, c.p.ActorID).Scan(&kind)
	return kind, err
}

// ---------------------------------------------------------------------------

type ExecutionStartIn struct {
	Project           string          `json:"project"`
	Workflow          string          `json:"workflow"`
	Version           *int            `json:"version,omitempty"`
	Params            json.RawMessage `json:"params,omitempty"`
	TaskNum           *int            `json:"task_num,omitempty"`
	ParentExecutionID *uuid.UUID      `json:"parent_execution_id,omitempty"`
	ParentStepKey     *string         `json:"parent_step_key,omitempty"`
}

type ExecutionStartOut struct {
	ExecutionID uuid.UUID `json:"execution_id"`
	Num         int       `json:"num"` // X-<num> within the project
	Ref         string    `json:"ref"`
	Version     int       `json:"version"`
	Ready       []string  `json:"ready"`
	Ack
}

// ExecutionStart starts a run of a process on its current version (or a given one).
func (s *Service) ExecutionStart(ctx context.Context, p Principal, in ExecutionStartIn) (ExecutionStartOut, error) {
	var out ExecutionStartOut
	if err := checkWorkflowKey(in.Workflow); err != nil {
		return out, err
	}
	var params map[string]any
	if len(in.Params) > 0 {
		if err := json.Unmarshal(in.Params, &params); err != nil || params == nil {
			return out, invalid("params", "must be an object")
		}
	}
	if in.ParentStepKey != nil && !stepKeyRe.MatchString(*in.ParentStepKey) {
		return out, invalid("parent_step_key", "%q does not match %s", *in.ParentStepKey, stepKeyRe)
	}
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		projectID, err := c.projectID(in.Project)
		if err != nil {
			return err
		}
		var wid uuid.UUID
		var current int
		var retired bool
		err = c.tx.QueryRow(ctx, `SELECT workflow_id, version, retired FROM projection.workflow WHERE project_id = $1 AND key = $2`,
			projectID, in.Workflow).Scan(&wid, &current, &retired)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail("workflow_not_found", "workflow %q in project %q", in.Workflow, in.Project)
		}
		if err != nil {
			return err
		}
		if retired {
			return fail("workflow_retired", "workflow %q is retired", in.Workflow)
		}
		out.Version = current
		if in.Version != nil {
			if *in.Version < 1 || *in.Version > current {
				return fail("version_not_found", "workflow %q has versions 1..%d", in.Workflow, current)
			}
			out.Version = *in.Version
		}
		payload := map[string]any{"workflow_id": wid, "version": out.Version, "project_id": projectID}
		if params != nil {
			payload["params"] = params
		}
		if in.TaskNum != nil {
			var tid uuid.UUID
			err := c.tx.QueryRow(ctx, `SELECT task_id FROM projection.task WHERE project_id = $1 AND num = $2 AND NOT hidden`,
				projectID, *in.TaskNum).Scan(&tid)
			if errors.Is(err, pgx.ErrNoRows) {
				return fail("task_not_found", "task #%d in project %q", *in.TaskNum, in.Project)
			}
			if err != nil {
				return err
			}
			payload["task_id"] = tid
		}
		if in.ParentExecutionID != nil {
			if _, err := c.loadExec(*in.ParentExecutionID, time.Now()); err != nil {
				return err
			}
			payload["parent_execution_id"] = *in.ParentExecutionID
			if in.ParentStepKey != nil {
				payload["parent_step_key"] = *in.ParentStepKey
			}
		}
		if out.Num, err = c.nextProjectNum("execution", projectID); err != nil {
			return err
		}
		slug, err := c.slug(projectID)
		if err != nil {
			return err
		}
		payload["num"], out.Ref = out.Num, ProjectRef(slug, "X", out.Num)
		out.ExecutionID = uuid.New()
		e, err := c.emit("execution.started", "execution", out.ExecutionID, nil, nil, payload)
		if err != nil {
			return err
		}
		out.Ack = ack(e)
		x, err := c.loadExec(out.ExecutionID, time.Now())
		if err != nil {
			return err
		}
		out.Ready = ready(x.State)
		return nil
	})
	return out, err
}

type ExecutionCloseIn struct {
	ExecutionID uuid.UUID `json:"execution_id"`
	Outcome     string    `json:"outcome"`
	Summary     *string   `json:"summary,omitempty"`
}

// ExecutionClose ends a run; a closed one gives already.
func (s *Service) ExecutionClose(ctx context.Context, p Principal, in ExecutionCloseIn) (Ack, error) {
	var a Ack
	if err := checkEnum("outcome", in.Outcome, executionOutcomes...); err != nil {
		return a, err
	}
	cleanOpt(in.Summary)
	if err := optLen("summary", in.Summary, 2000); err != nil {
		return a, err
	}
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		x, err := c.loadExec(in.ExecutionID, time.Now())
		if err != nil {
			return err
		}
		if !x.Open {
			a.Already = true
			return nil
		}
		payload := map[string]any{"outcome": in.Outcome}
		if in.Summary != nil && *in.Summary != "" {
			payload["summary"] = *in.Summary
		}
		e, err := c.emit("execution.closed", "execution", in.ExecutionID, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

type StepStartIn struct {
	ExecutionID uuid.UUID `json:"execution_id"`
	StepKey     string    `json:"step_key"`
}

type StepStartOut struct {
	AttemptID  uuid.UUID `json:"attempt_id"`
	Attempt    int       `json:"attempt"`
	LeaseUntil time.Time `json:"lease_until"`
	Ack
}

// StepStart takes a ready step: one attempt holds it until its lease runs out.
func (s *Service) StepStart(ctx context.Context, p Principal, in StepStartIn) (StepStartOut, error) {
	var out StepStartOut
	if err := p.boundTo(in.ExecutionID, in.StepKey); err != nil {
		return out, err
	}
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		now := time.Now().UTC()
		x, err := c.loadExec(in.ExecutionID, now)
		if err != nil {
			return err
		}
		if !x.Open {
			return fail("execution_closed", "execution %s is closed", in.ExecutionID)
		}
		step, ok := x.step(in.StepKey)
		if !ok {
			return fail("step_not_found", "no step %q in this version", in.StepKey)
		}
		ss, _ := x.State.Of(in.StepKey)
		switch {
		case ss.State == flow.Working:
			return fail("step_busy", "step %q is held by another attempt", in.StepKey)
		case ss.State == flow.InputRequired && ss.Waiting == "person":
			return fail("person_required", "step %q is done by a person: approval_grant records it", in.StepKey)
		case ss.State == flow.InputRequired:
			return fail("approval_required", "step %q needs the user's GO first (approval_grant)", in.StepKey)
		case ss.State != flow.Ready:
			return fail("step_not_ready", "step %q is %s", in.StepKey, ss.State)
		}
		if step.WhoMayRun != nil && *step.WhoMayRun == "human_only" {
			kind, err := c.actorKind()
			if err != nil {
				return err
			}
			if kind != "human" {
				return fail("person_required", "step %q is human_only: the user runs it", in.StepKey)
			}
		}
		// an attempt whose lease ran out is closed first, by the server
		if ss.Expired {
			var stale uuid.UUID
			if err := c.tx.QueryRow(ctx, `SELECT attempt_id FROM projection.step_attempt
			        WHERE execution_id = $1 AND step_key = $2 AND state = 'working' ORDER BY n DESC LIMIT 1`, in.ExecutionID, in.StepKey).Scan(&stale); err != nil {
				return err
			}
			if _, err := c.emit("step.finished", "attempt", stale, nil, nil, map[string]any{"state": "failed", "error": "lease_expired"}); err != nil {
				return err
			}
		}
		payload := map[string]any{"execution_id": in.ExecutionID, "step_key": in.StepKey}
		if step.RequiresApproval && ss.Attempts == 0 {
			var aid uuid.UUID
			if err := c.tx.QueryRow(ctx, `SELECT approval_id FROM projection.approval WHERE execution_id = $1 AND step_key = $2
			        AND NOT revoked AND used_by IS NULL ORDER BY granted_at LIMIT 1`, in.ExecutionID, in.StepKey).Scan(&aid); err != nil {
				return err
			}
			payload["approval_id"] = aid
		}
		out.Attempt = ss.Attempts + 1
		out.LeaseUntil = now.Add(step.Lease()).Truncate(time.Millisecond)
		out.AttemptID = uuid.New()
		payload["attempt"], payload["lease_until"] = out.Attempt, out.LeaseUntil
		e, err := c.emit("step.started", "attempt", out.AttemptID, nil, nil, payload)
		if err == nil {
			out.Ack = ack(e)
		}
		return err
	})
	return out, err
}

// attempt finds a live attempt of the caller's runs and its execution.
func (c *cmd) attempt(id uuid.UUID) (exec uuid.UUID, key string, state string, lease time.Time, err error) {
	err = c.tx.QueryRow(c.ctx, `SELECT execution_id, step_key, state, lease_until FROM projection.step_attempt WHERE attempt_id = $1`, id).
		Scan(&exec, &key, &state, &lease)
	if errors.Is(err, pgx.ErrNoRows) {
		err = fail("attempt_not_found", "attempt %s", id)
	}
	return
}

func outputObject(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, invalid("output", "must be an object")
	}
	return m, nil
}

type StepCheckpointIn struct {
	AttemptID uuid.UUID       `json:"attempt_id"`
	Output    json.RawMessage `json:"output,omitempty"`
	Note      *string         `json:"note,omitempty"`
}

type StepCheckpointOut struct {
	LeaseUntil time.Time `json:"lease_until"`
	Ack
}

// StepCheckpoint renews the lease of a working attempt (heartbeat) and may store a partial output.
func (s *Service) StepCheckpoint(ctx context.Context, p Principal, in StepCheckpointIn) (StepCheckpointOut, error) {
	var out StepCheckpointOut
	output, err := outputObject(in.Output)
	if err != nil {
		return out, err
	}
	cleanOpt(in.Note)
	if err := optLen("note", in.Note, 1000); err != nil {
		return out, err
	}
	err = s.exec(ctx, s.DB, p, func(c *cmd) error {
		execID, key, state, lease, err := c.attempt(in.AttemptID)
		if err != nil {
			return err
		}
		if err := p.boundTo(execID, key); err != nil {
			return err
		}
		if state != "working" {
			return fail("attempt_finished", "attempt is %s", state)
		}
		now := time.Now().UTC()
		if !now.Before(lease) {
			return fail("lease_expired", "the lease ran out at %s; take the step again with step_start", lease.Format(time.RFC3339))
		}
		x, err := c.loadExec(execID, now)
		if err != nil {
			return err
		}
		step, _ := x.step(key)
		out.LeaseUntil = now.Add(step.Lease()).Truncate(time.Millisecond)
		payload := map[string]any{"lease_until": out.LeaseUntil}
		if output != nil {
			payload["output"] = output
		}
		if in.Note != nil && *in.Note != "" {
			payload["note"] = *in.Note
		}
		e, err := c.emit("step.checkpointed", "attempt", in.AttemptID, nil, nil, payload)
		if err == nil {
			out.Ack = ack(e)
		}
		return err
	})
	return out, err
}

type StepFinishIn struct {
	AttemptID uuid.UUID       `json:"attempt_id"`
	State     string          `json:"state"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     *string         `json:"error,omitempty"`
}

type StepFinishOut struct {
	Ready           []string `json:"ready"`
	ExecutionClosed bool     `json:"execution_closed,omitempty"`
	Ack
}

// StepFinish ends an attempt; the last step closes the run.
func (s *Service) StepFinish(ctx context.Context, p Principal, in StepFinishIn) (StepFinishOut, error) {
	var out StepFinishOut
	if err := checkEnum("state", in.State, attemptStates...); err != nil {
		return out, err
	}
	output, err := outputObject(in.Output)
	if err != nil {
		return out, err
	}
	cleanOpt(in.Error)
	if err := optLen("error", in.Error, 2000); err != nil {
		return out, err
	}
	err = s.exec(ctx, s.DB, p, func(c *cmd) error {
		execID, key, state, _, err := c.attempt(in.AttemptID)
		if err != nil {
			return err
		}
		if err := p.boundTo(execID, key); err != nil {
			return err
		}
		if state != "working" {
			return fail("attempt_finished", "attempt is %s", state)
		}
		if _, err := c.loadExec(execID, time.Now()); err != nil { // lock the run
			return err
		}
		payload := map[string]any{"state": in.State}
		if output != nil {
			payload["output"] = output
		}
		if in.Error != nil && *in.Error != "" {
			payload["error"] = *in.Error
		}
		e, err := c.emit("step.finished", "attempt", in.AttemptID, nil, nil, payload)
		if err != nil {
			return err
		}
		out.Ack = ack(e)
		out.ExecutionClosed, out.Ready, err = c.closeIfDone(execID, time.Now().UTC())
		return err
	})
	return out, err
}

type ApprovalGrantIn struct {
	ExecutionID uuid.UUID `json:"execution_id"`
	StepKey     string    `json:"step_key"`
	Items       []string  `json:"items,omitempty"`
	Note        *string   `json:"note,omitempty"`
}

type ApprovalGrantOut struct {
	ApprovalID      uuid.UUID `json:"approval_id"`
	Ready           []string  `json:"ready"`
	ExecutionClosed bool      `json:"execution_closed,omitempty"`
	Ack
}

// ApprovalGrant records a GO of a person (Д7): only a human actor gives it; a live unused GO on the step is returned.
func (s *Service) ApprovalGrant(ctx context.Context, p Principal, in ApprovalGrantIn) (ApprovalGrantOut, error) {
	var out ApprovalGrantOut
	if err := checkList("items", in.Items, 100, 200); err != nil {
		return out, err
	}
	cleanOpt(in.Note)
	if err := optLen("note", in.Note, 1000); err != nil {
		return out, err
	}
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		kind, err := c.actorKind()
		if err != nil {
			return err
		}
		if kind != "human" {
			return fail("approval_requires_human", "a GO is the user's decision; an %s cannot give it — ask the user", kind)
		}
		now := time.Now().UTC()
		x, err := c.loadExec(in.ExecutionID, now)
		if err != nil {
			return err
		}
		if !x.Open {
			return fail("execution_closed", "execution %s is closed", in.ExecutionID)
		}
		step, ok := x.step(in.StepKey)
		if !ok {
			return fail("step_not_found", "no step %q in this version", in.StepKey)
		}
		if step.Kind != "human" && !step.RequiresApproval {
			return fail("approval_not_needed", "step %q needs no GO", in.StepKey)
		}
		err = c.tx.QueryRow(ctx, `SELECT approval_id FROM projection.approval WHERE execution_id = $1 AND step_key = $2
		        AND NOT revoked AND used_by IS NULL LIMIT 1`, in.ExecutionID, in.StepKey).Scan(&out.ApprovalID)
		if err == nil {
			out.Already, out.Ready = true, ready(x.State)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		payload := map[string]any{"execution_id": in.ExecutionID, "step_key": in.StepKey}
		if len(in.Items) > 0 {
			payload["items"] = slices.Clone(in.Items)
		}
		if in.Note != nil && *in.Note != "" {
			payload["note"] = *in.Note
		}
		out.ApprovalID = uuid.New()
		e, err := c.emit("approval.granted", "approval", out.ApprovalID, nil, nil, payload)
		if err != nil {
			return err
		}
		out.Ack = ack(e)
		out.ExecutionClosed, out.Ready, err = c.closeIfDone(in.ExecutionID, now)
		return err
	})
	return out, err
}

// ApprovalRevoke takes back a GO no attempt has used.
func (s *Service) ApprovalRevoke(ctx context.Context, p Principal, id uuid.UUID, reason *string) (Ack, error) {
	var a Ack
	cleanOpt(reason)
	if err := optLen("reason", reason, 500); err != nil {
		return a, err
	}
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		var revoked, used bool
		err := c.tx.QueryRow(ctx, `SELECT revoked, used_by IS NOT NULL FROM projection.approval WHERE approval_id = $1`, id).Scan(&revoked, &used)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail("approval_not_found", "approval %s", id)
		}
		if err != nil {
			return err
		}
		if revoked {
			a.Already = true
			return nil
		}
		if used {
			return fail("approval_used", "a step already started with this GO")
		}
		payload := map[string]any{}
		if reason != nil && *reason != "" {
			payload["reason"] = *reason
		}
		e, err := c.emit("approval.revoked", "approval", id, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

// boundTo: a derived grant acts only on its own run and step.
func (p Principal) boundTo(execution uuid.UUID, step string) error {
	if !p.Grant.Derived {
		return nil
	}
	if p.Grant.ExecutionID == nil || *p.Grant.ExecutionID != execution || p.Grant.StepKey != step {
		return fail("outside_grant", "this grant covers step %q of run %v only", p.Grant.StepKey, p.Grant.ExecutionID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// step.delegate
// ---------------------------------------------------------------------------

type StepDelegateIn struct {
	ExecutionID uuid.UUID  `json:"execution_id"`
	StepKey     string     `json:"step_key"`
	ActorID     *uuid.UUID `json:"actor_id,omitempty"` // an existing agent / service of the same person
	Agent       *string    `json:"agent,omitempty"`    // or a role: the agent actor is registered on demand
	TTLSeconds  *int       `json:"ttl_s,omitempty"`
}

type StepDelegateOut struct {
	Token           string    `json:"token"` // shown once
	TokenID         uuid.UUID `json:"token_id"`
	ExecutorActorID uuid.UUID `json:"executor_actor_id"`
	ExpiresAt       time.Time `json:"expires_at"`
	Verbs           []string  `json:"verbs"`
	WriteMode       string    `json:"write_mode"`
	Depth           int       `json:"depth"`
	Ack
}

// StepDelegate issues a derived grant for one step of a run to an executor and records the delegation in the
// journal (step.delegated, without the secret). parent is the caller's own token principal (the edge knows it).
func (s *Service) StepDelegate(ctx context.Context, p Principal, parent auth.Principal, in StepDelegateIn) (StepDelegateOut, error) {
	var out StepDelegateOut
	if (in.ActorID == nil) == (in.Agent == nil) {
		return out, invalid("executor", "pass exactly one of actor_id or agent")
	}
	if err := p.boundTo(in.ExecutionID, in.StepKey); err != nil {
		return out, err // a derived grant delegates further only its own step
	}
	ttl := time.Duration(0)
	if in.TTLSeconds != nil {
		if *in.TTLSeconds < 60 || *in.TTLSeconds > int(auth.MaxDerivedTTL/time.Second) {
			return out, invalid("ttl_s", "60..%d", int(auth.MaxDerivedTTL/time.Second))
		}
		ttl = time.Duration(*in.TTLSeconds) * time.Second
	}
	var executor uuid.UUID
	if in.Agent != nil {
		if !agentRoleRe.MatchString(*in.Agent) {
			return out, invalid("agent", "lower-case letters, digits and '-', at most 63")
		}
		ext := "agent:" + *in.Agent
		id, _, err := s.ActorRegister(ctx, p, ActorRegisterIn{Kind: "agent", DisplayName: *in.Agent, ExternalIdentity: &ext})
		if err != nil {
			return out, err
		}
		executor = id
	} else {
		executor = *in.ActorID
	}
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		var kind string
		var root *uuid.UUID
		var profile []byte // the executor profile (0024); nil — a subagent of the session, no profile
		err := c.tx.QueryRow(ctx, `SELECT kind, litopys_root(actor_id), executor FROM projection.actor
		        WHERE actor_id = $1 AND revoked_at IS NULL`, executor).Scan(&kind, &root, &profile)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (kind == "human" || root == nil || *root != c.me)) {
			return invalid("actor_id", "the executor must be a live agent or service of the same person")
		}
		if err != nil {
			return err
		}
		x, err := c.loadExec(in.ExecutionID, time.Now())
		if err != nil {
			return err
		}
		if !x.Open {
			return fail("execution_closed", "execution %s is closed", in.ExecutionID)
		}
		step, ok := x.step(in.StepKey)
		if !ok {
			return fail("step_not_found", "no step %q in this version", in.StepKey)
		}
		switch {
		case step.Kind == "human":
			return fail("person_required", "step %q is done by a person; it cannot be delegated", in.StepKey)
		case step.WhoMayRun != nil && *step.WhoMayRun != "executor":
			return fail("step_not_delegable", "step %q is run by the %s", in.StepKey, *step.WhoMayRun)
		case step.Delegable != nil && !*step.Delegable:
			return fail("step_not_delegable", "step %q is marked not delegable", in.StepKey)
		}
		var project, projectClass string
		if err := c.tx.QueryRow(ctx, `SELECT name, data_class FROM projection.project WHERE project_id = $1`, x.ProjectID).Scan(&project, &projectClass); err != nil {
			return err
		}
		di := auth.DerivedIn{ExecutorActorID: executor, ExecutionID: in.ExecutionID, Project: project, StepKey: in.StepKey,
			Verbs: append([]string{}, step.Verbs...), WriteMode: "direct", TTL: ttl}
		if step.WriteMode != nil {
			di.WriteMode = *step.WriteMode
		}
		// data class of the grant: the narrowest of the executor's profile, the step and the delegating
		// grant; an executor that would not see its own run's project is refused here, not left with an empty view
		// the executor's class for this project: a person may have widened its access
		execClass := policy.ExecutorClass(profile, project)
		di.MaxDataClass = narrowestClass(execClass, deref(step.MaxDataClass), parent.Grant.MaxDataClass)
		if di.MaxDataClass != "" && policy.ClassRank(di.MaxDataClass) < policy.ClassRank(projectClass) {
			why := "the step"
			switch {
			case execClass == di.MaxDataClass:
				why = "the executor's profile"
			case parent.Grant.MaxDataClass == di.MaxDataClass:
				why = "the delegating grant"
			}
			return fail("data_class_exceeded", "project %s is %s; %s allows at most %s", project, projectClass, why, di.MaxDataClass)
		}
		raw, tokenID, expires, err := auth.IssueDerived(ctx, c.tx, parent, di)
		var refusal *auth.ErrDelegation
		if errors.As(err, &refusal) {
			return fail("delegation_refused", "%s", refusal.Reason)
		}
		if err != nil {
			return err
		}
		out.Token, out.TokenID, out.ExecutorActorID, out.ExpiresAt = raw, tokenID, executor, expires
		out.Verbs, out.WriteMode, out.Depth = di.Verbs, di.WriteMode, parent.Grant.Depth+1
		payload := map[string]any{"execution_id": in.ExecutionID, "step_key": in.StepKey, "executor_actor_id": executor,
			"token_id": tokenID, "depth": out.Depth, "expires_at": expires, "verbs": out.Verbs, "write_mode": out.WriteMode}
		if parent.Grant.Derived {
			payload["parent_token_id"] = parent.TokenID
		}
		if di.MaxDataClass != "" {
			payload["max_data_class"] = di.MaxDataClass
		}
		e, err := c.emit("step.delegated", "execution", in.ExecutionID, nil, nil, payload)
		if err == nil {
			out.Ack = ack(e)
		}
		return err
	})
	if err != nil {
		out.Token = ""
	}
	return out, err
}
