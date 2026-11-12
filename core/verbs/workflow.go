package verbs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"litopys.smart-gw.co.ua/core/cron"
	"regexp"
	"slices"
	"time"

	"cel.dev/cel-go/cel"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Workflows: a process of a project with immutable versions. Commands here cover the
// definitions; executions, step attempts and approvals come with the next slice.

var (
	workflowKeyRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	stepKeyRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
	verbNameRe    = regexp.MustCompile(`^[a-z_]+\.[a-z_]+$`)
	stepKinds     = []string{"script", "model", "human", "external", "subworkflow"}
	executorClass = []string{"cheap", "main"}
	whoMayRun     = []string{"assistant", "executor", "human_only"}
	writeModes    = []string{"direct", "candidate"}
	dataClasses   = []string{"public", "internal", "local"}
	triggerKinds  = []string{"manual", "schedule", "journal", "webhook"}
)

// StepDefinition is types.yaml StepDefinition; unknown fields are refused (decoded strictly).
type StepDefinition struct {
	Key              string          `json:"key"`
	Title            string          `json:"title"`
	Kind             string          `json:"kind"`
	ExecutorClass    *string         `json:"executor_class,omitempty"`
	Delegable        *bool           `json:"delegable,omitempty"`
	WhoMayRun        *string         `json:"who_may_run,omitempty"`
	Input            json.RawMessage `json:"input,omitempty"`
	Output           json.RawMessage `json:"output,omitempty"`
	ContextPack      *ContextPack    `json:"context_pack,omitempty"`
	Verbs            []string        `json:"verbs,omitempty"`
	WriteMode        *string         `json:"write_mode,omitempty"`
	MaxDataClass     *string         `json:"max_data_class,omitempty"`
	RequiresApproval *bool           `json:"requires_approval,omitempty"`
	Condition        *string         `json:"condition,omitempty"`
	Next             []string        `json:"next,omitempty"`
	Idempotency      *string         `json:"idempotency,omitempty"`
	TimeoutS         *int            `json:"timeout_s,omitempty"`
	HeartbeatS       *int            `json:"heartbeat_s,omitempty"`
	Retry            *StepRetry      `json:"retry,omitempty"`
	External         *StepExternal   `json:"external,omitempty"`
	Subworkflow      *string         `json:"subworkflow,omitempty"`
	Acceptance       *string         `json:"acceptance,omitempty"`
}

type ContextPack struct {
	Selectors    []string `json:"selectors,omitempty"`
	BudgetTokens *int     `json:"budget_tokens,omitempty"`
}

type StepRetry struct {
	Max      *int `json:"max,omitempty"`
	BackoffS *int `json:"backoff_s,omitempty"`
}

type StepExternal struct {
	System *string `json:"system,omitempty"`
	Ref    *string `json:"ref,omitempty"`
}

type WorkflowTrigger struct {
	Kind string  `json:"kind"`
	Spec *string `json:"spec,omitempty"`
}

// WorkflowDefinition is types.yaml WorkflowDefinition: one frozen version.
type WorkflowDefinition struct {
	Params     json.RawMessage   `json:"params,omitempty"`
	Triggers   []WorkflowTrigger `json:"triggers,omitempty"`
	Touches    []string          `json:"touches,omitempty"`
	VerifiedAt *string           `json:"verified_at,omitempty"`
	Steps      []StepDefinition  `json:"steps"`
}

// conditionEnv: a step condition sees the run parameters and the outputs of earlier steps, nothing else.
var conditionEnv = func() *cel.Env {
	env, err := cel.NewEnv(
		cel.Variable("params", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("steps", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		panic("verbs: CEL environment: " + err.Error())
	}
	return env
}()

func defErr(format string, args ...any) *Error {
	return fail("invalid_definition", format, args...)
}

func jsonObject(field string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return defErr("%s: must be a JSON Schema object", field)
	}
	return nil
}

func optEnum(field string, v *string, allowed []string) error {
	if v == nil {
		return nil
	}
	if !slices.Contains(allowed, *v) {
		return defErr("%s: %q not in %v", field, *v, allowed)
	}
	return nil
}

func optRange(field string, v *int, lo, hi int) error {
	if v != nil && (*v < lo || *v > hi) {
		return defErr("%s: %d not in %d..%d", field, *v, lo, hi)
	}
	return nil
}

// ParseWorkflowDefinition decodes a definition strictly and checks what the schema cannot: unique step keys,
// next pointing at existing steps without cycles, fields that belong to the step kind, CEL conditions that
// compile to bool. It returns the canonical JSON stored in the event.
func ParseWorkflowDefinition(raw json.RawMessage) (WorkflowDefinition, json.RawMessage, error) {
	var d WorkflowDefinition
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, nil, defErr("%v", err)
	}
	if n := len(d.Steps); n < 1 || n > 50 {
		return d, nil, defErr("steps: %d, need 1..50", n)
	}
	if err := jsonObject("params", d.Params); err != nil {
		return d, nil, err
	}
	if len(d.Triggers) > 10 {
		return d, nil, defErr("triggers: at most 10")
	}
	for i, t := range d.Triggers {
		if !slices.Contains(triggerKinds, t.Kind) {
			return d, nil, defErr("triggers[%d].kind: %q not in %v", i, t.Kind, triggerKinds)
		}
		if t.Kind != "manual" && (t.Spec == nil || *t.Spec == "") {
			return d, nil, defErr("triggers[%d]: %s needs spec (cron, event type or webhook name)", i, t.Kind)
		}
		if t.Spec != nil && len(*t.Spec) > 500 {
			return d, nil, defErr("triggers[%d].spec: longer than 500", i)
		}
		if t.Kind == "schedule" { // the worker starts it at those minutes (Europe/Kyiv)
			if _, err := cron.Parse(*t.Spec); err != nil {
				return d, nil, defErr("triggers[%d].spec: %v", i, err)
			}
		}
	}
	if len(d.Touches) > 30 {
		return d, nil, defErr("touches: at most 30")
	}
	for i, s := range d.Touches {
		if s == "" || len([]rune(s)) > 200 {
			return d, nil, defErr("touches[%d]: 1..200 characters", i)
		}
	}
	if d.VerifiedAt != nil {
		if _, err := time.Parse("2006-01-02", *d.VerifiedAt); err != nil {
			return d, nil, defErr("verified_at: YYYY-MM-DD")
		}
	}
	index := map[string]int{}
	for i, s := range d.Steps {
		f := fmt.Sprintf("steps[%d]", i)
		if !stepKeyRe.MatchString(s.Key) {
			return d, nil, defErr("%s.key: %q does not match %s", f, s.Key, stepKeyRe)
		}
		if _, dup := index[s.Key]; dup {
			return d, nil, defErr("%s.key: %q used twice", f, s.Key)
		}
		index[s.Key] = i
		if n := len([]rune(s.Title)); n < 1 || n > 500 {
			return d, nil, defErr("%s.title: 1..500 characters", f)
		}
		if err := checkStep(f, s); err != nil {
			return d, nil, err
		}
	}
	// next: existing keys; the graph (explicit next, or the following step) must be acyclic
	for i, s := range d.Steps {
		for _, n := range s.Next {
			if _, ok := index[n]; !ok {
				return d, nil, defErr("steps[%d].next: no step %q", i, n)
			}
			if n == s.Key {
				return d, nil, defErr("steps[%d].next: a step cannot follow itself (retry repeats a step)", i)
			}
		}
	}
	if cyc := stepCycle(d.Steps, index); cyc != "" {
		return d, nil, defErr("next forms a cycle through %q; repeats are retry, not a loop", cyc)
	}
	canon, err := json.Marshal(d)
	return d, canon, err
}

func checkStep(f string, s StepDefinition) error {
	if !slices.Contains(stepKinds, s.Kind) {
		return defErr("%s.kind: %q not in %v", f, s.Kind, stepKinds)
	}
	if err := optEnum(f+".executor_class", s.ExecutorClass, executorClass); err != nil {
		return err
	}
	if s.ExecutorClass != nil && s.Kind != "model" {
		return defErr("%s.executor_class: only for kind model", f)
	}
	if s.Delegable != nil && s.Kind != "model" {
		return defErr("%s.delegable: only for kind model", f)
	}
	if err := optEnum(f+".who_may_run", s.WhoMayRun, whoMayRun); err != nil {
		return err
	}
	if s.Kind == "human" && s.WhoMayRun != nil && *s.WhoMayRun != "human_only" {
		return defErr("%s.who_may_run: a human step is human_only", f)
	}
	if err := optEnum(f+".write_mode", s.WriteMode, writeModes); err != nil {
		return err
	}
	if err := optEnum(f+".max_data_class", s.MaxDataClass, dataClasses); err != nil {
		return err
	}
	if err := jsonObject(f+".input", s.Input); err != nil {
		return err
	}
	if err := jsonObject(f+".output", s.Output); err != nil {
		return err
	}
	if cp := s.ContextPack; cp != nil {
		if len(cp.Selectors) > 20 {
			return defErr("%s.context_pack.selectors: at most 20", f)
		}
		for i, sel := range cp.Selectors {
			if sel == "" || len(sel) > 300 {
				return defErr("%s.context_pack.selectors[%d]: 1..300 characters", f, i)
			}
		}
		if err := optRange(f+".context_pack.budget_tokens", cp.BudgetTokens, 500, 32000); err != nil {
			return err
		}
	}
	if len(s.Verbs) > 30 {
		return defErr("%s.verbs: at most 30", f)
	}
	for i, v := range s.Verbs {
		if !verbNameRe.MatchString(v) {
			return defErr("%s.verbs[%d]: %q is not domain.verb", f, i, v)
		}
	}
	if len(s.Next) > 10 {
		return defErr("%s.next: at most 10", f)
	}
	if s.Condition != nil {
		if len(*s.Condition) > 1000 {
			return defErr("%s.condition: longer than 1000", f)
		}
		ast, iss := conditionEnv.Compile(*s.Condition)
		if iss != nil && iss.Err() != nil {
			return defErr("%s.condition: %v", f, iss.Err())
		}
		if t := ast.OutputType(); !t.IsExactType(cel.BoolType) && !t.IsExactType(cel.DynType) {
			return defErr("%s.condition: must be a bool expression, got %s", f, t)
		}
	}
	if s.Idempotency != nil && len(*s.Idempotency) > 300 {
		return defErr("%s.idempotency: longer than 300", f)
	}
	if err := optRange(f+".timeout_s", s.TimeoutS, 10, 604800); err != nil {
		return err
	}
	if err := optRange(f+".heartbeat_s", s.HeartbeatS, 5, 3600); err != nil {
		return err
	}
	if s.TimeoutS != nil && s.HeartbeatS != nil && *s.HeartbeatS >= *s.TimeoutS {
		return defErr("%s.heartbeat_s: must be shorter than timeout_s", f)
	}
	if r := s.Retry; r != nil {
		if err := optRange(f+".retry.max", r.Max, 0, 10); err != nil {
			return err
		}
		if err := optRange(f+".retry.backoff_s", r.BackoffS, 0, 86400); err != nil {
			return err
		}
	}
	switch s.Kind {
	case "external":
		if s.External == nil || s.External.System == nil || *s.External.System == "" {
			return defErr("%s.external.system: required for kind external", f)
		}
	case "subworkflow":
		if s.Subworkflow == nil || !workflowKeyRe.MatchString(*s.Subworkflow) {
			return defErr("%s.subworkflow: the key of the nested workflow is required", f)
		}
	}
	if s.External != nil && s.Kind != "external" {
		return defErr("%s.external: only for kind external", f)
	}
	if s.Subworkflow != nil && s.Kind != "subworkflow" {
		return defErr("%s.subworkflow: only for kind subworkflow", f)
	}
	if e := s.External; e != nil {
		if e.System != nil && len(*e.System) > 60 || e.Ref != nil && len(*e.Ref) > 300 {
			return defErr("%s.external: system ≤ 60, ref ≤ 300", f)
		}
	}
	if s.Acceptance != nil && len([]rune(*s.Acceptance)) > 4000 {
		return defErr("%s.acceptance: longer than 4000", f)
	}
	return nil
}

// stepCycle returns a step on a cycle of the step graph, or "".
func stepCycle(steps []StepDefinition, index map[string]int) string {
	succ := func(i int) []int {
		if len(steps[i].Next) == 0 {
			if i+1 < len(steps) {
				return []int{i + 1}
			}
			return nil
		}
		out := make([]int, 0, len(steps[i].Next))
		for _, n := range steps[i].Next {
			out = append(out, index[n])
		}
		return out
	}
	state := make([]int, len(steps)) // 0 new, 1 on stack, 2 done
	var visit func(int) string
	visit = func(i int) string {
		state[i] = 1
		for _, j := range succ(i) {
			if state[j] == 1 {
				return steps[j].Key
			}
			if state[j] == 0 {
				if c := visit(j); c != "" {
					return c
				}
			}
		}
		state[i] = 2
		return ""
	}
	for i := range steps {
		if state[i] == 0 {
			if c := visit(i); c != "" {
				return c
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// commands
// ---------------------------------------------------------------------------

type WorkflowDeclareIn struct {
	Project    string          `json:"project"`
	Key        string          `json:"key"`
	Title      string          `json:"title"`
	Purpose    *string         `json:"purpose,omitempty"`
	Definition json.RawMessage `json:"definition"`
}

type WorkflowDeclareOut struct {
	WorkflowID uuid.UUID `json:"workflow_id"`
	Num        int       `json:"num"` // W-<num> within the project
	Ref        string    `json:"ref"`
	Version    int       `json:"version"`
	Ack
}

func checkWorkflowKey(key string) error {
	if !workflowKeyRe.MatchString(key) {
		return invalid("key", "%q does not match %s", key, workflowKeyRe)
	}
	return nil
}

// workflow finds a workflow of a visible project by key and locks it for the command.
func (c *cmd) workflow(project, key string) (id uuid.UUID, version int, retired bool, err error) {
	projectID, err := c.projectID(project)
	if err != nil {
		return
	}
	err = c.tx.QueryRow(c.ctx, `SELECT workflow_id, version, retired FROM projection.workflow
	        WHERE project_id = $1 AND key = $2 FOR UPDATE`, projectID, key).Scan(&id, &version, &retired)
	if errors.Is(err, pgx.ErrNoRows) {
		err = fail("workflow_not_found", "workflow %q in project %q", key, project)
	}
	return
}

// WorkflowDeclare saves a new process with version 1.
func (s *Service) WorkflowDeclare(ctx context.Context, p Principal, in WorkflowDeclareIn) (WorkflowDeclareOut, error) {
	var out WorkflowDeclareOut
	if err := checkWorkflowKey(in.Key); err != nil {
		return out, err
	}
	if err := checkLen("title", in.Title, 1, 500); err != nil {
		return out, err
	}
	if err := optLen("purpose", in.Purpose, 2000); err != nil {
		return out, err
	}
	_, canon, err := ParseWorkflowDefinition(in.Definition)
	if err != nil {
		return out, err
	}
	err = s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		projectID, err := c.projectID(in.Project)
		if err != nil {
			return err
		}
		var exists bool
		if err := c.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projection.workflow WHERE project_id = $1 AND key = $2)`,
			projectID, in.Key).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return fail("workflow_exists", "workflow %q already exists in project %q; a changed process is workflow.supersede", in.Key, in.Project)
		}
		out.WorkflowID, out.Version = uuid.New(), 1
		if out.Num, err = c.nextProjectNum("workflow", projectID); err != nil {
			return err
		}
		slug, err := c.slug(projectID)
		if err != nil {
			return err
		}
		out.Ref = ProjectRef(slug, "W", out.Num)
		payload := map[string]any{"project_id": projectID, "key": in.Key, "title": in.Title, "definition": json.RawMessage(canon),
			"num": out.Num}
		if in.Purpose != nil && *in.Purpose != "" {
			payload["purpose"] = *in.Purpose
		}
		e, err := c.emit("workflow.declared", "workflow", out.WorkflowID, nil, nil, payload)
		if err == nil {
			out.Ack = ack(e)
		}
		return err
	})
	return out, err
}

type WorkflowAmendIn struct {
	Project string         `json:"project"`
	Key     string         `json:"key"`
	Fields  map[string]any `json:"fields"` // title, purpose (null clears)
}

// WorkflowAmend changes the metadata of a process; the same values give unchanged.
func (s *Service) WorkflowAmend(ctx context.Context, p Principal, in WorkflowAmendIn) (Ack, error) {
	var a Ack
	payload := map[string]any{}
	for k, v := range in.Fields {
		switch k {
		case "title":
			t, ok := v.(string)
			if !ok {
				return a, invalid("title", "must be a string")
			}
			if err := checkLen("title", t, 1, 500); err != nil {
				return a, err
			}
			payload[k] = t
		case "purpose":
			if v == nil {
				payload[k] = nil
				continue
			}
			t, ok := v.(string)
			if !ok {
				return a, invalid("purpose", "must be a string or null")
			}
			if err := checkLen("purpose", t, 0, 2000); err != nil {
				return a, err
			}
			payload[k] = t
		default:
			return a, invalid(k, "not a workflow field; steps change through workflow.supersede")
		}
	}
	if len(payload) == 0 {
		return a, invalid("fields", "nothing to change: title or purpose")
	}
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		id, _, _, err := c.workflow(in.Project, in.Key)
		if err != nil {
			return err
		}
		var title string
		var purpose *string
		if err := c.tx.QueryRow(ctx, `SELECT title, purpose FROM projection.workflow WHERE workflow_id = $1`, id).Scan(&title, &purpose); err != nil {
			return err
		}
		if t, ok := payload["title"]; ok && t == title {
			delete(payload, "title")
		}
		if v, ok := payload["purpose"]; ok {
			np, _ := v.(string)
			if (v == nil && purpose == nil) || (v != nil && purpose != nil && np == *purpose) {
				delete(payload, "purpose")
			}
		}
		if len(payload) == 0 {
			a.Unchanged = true
			return nil
		}
		e, err := c.emit("workflow.amended", "workflow", id, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

type WorkflowSupersedeIn struct {
	Project    string          `json:"project"`
	Key        string          `json:"key"`
	IfVersion  *int            `json:"if_version,omitempty"`
	Definition json.RawMessage `json:"definition"`
	Reason     *string         `json:"reason,omitempty"`
}

type WorkflowSupersedeOut struct {
	Version int `json:"version"`
	Ack
}

// WorkflowSupersede publishes the next immutable version; the definition of the current one gives unchanged.
func (s *Service) WorkflowSupersede(ctx context.Context, p Principal, in WorkflowSupersedeIn) (WorkflowSupersedeOut, error) {
	var out WorkflowSupersedeOut
	if err := optLen("reason", in.Reason, 500); err != nil {
		return out, err
	}
	_, canon, err := ParseWorkflowDefinition(in.Definition)
	if err != nil {
		return out, err
	}
	err = s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		id, version, retired, err := c.workflow(in.Project, in.Key)
		if err != nil {
			return err
		}
		if retired {
			return fail("workflow_retired", "workflow %q is retired", in.Key)
		}
		if in.IfVersion != nil && *in.IfVersion != version {
			return fail("version_conflict", "current version is %d, not %d", version, *in.IfVersion)
		}
		var same bool
		if err := c.tx.QueryRow(ctx, `SELECT definition = $3::jsonb FROM projection.workflow_version WHERE workflow_id = $1 AND version = $2`,
			id, version, string(canon)).Scan(&same); err != nil {
			return err
		}
		if same {
			out.Version, out.Unchanged = version, true
			return nil
		}
		out.Version = version + 1
		payload := map[string]any{"version": out.Version, "definition": json.RawMessage(canon)}
		if in.Reason != nil && *in.Reason != "" {
			payload["reason"] = *in.Reason
		}
		e, err := c.emit("workflow.superseded", "workflow", id, nil, nil, payload)
		if err == nil {
			out.Ack = ack(e)
		}
		return err
	})
	return out, err
}

// WorkflowRetire stops a process from starting; a retired one gives already.
func (s *Service) WorkflowRetire(ctx context.Context, p Principal, project, key string, reason *string) (Ack, error) {
	var a Ack
	if err := optLen("reason", reason, 500); err != nil {
		return a, err
	}
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		id, _, retired, err := c.workflow(project, key)
		if err != nil {
			return err
		}
		if retired {
			a.Already = true
			return nil
		}
		payload := map[string]any{}
		if reason != nil && *reason != "" {
			payload["reason"] = *reason
		}
		e, err := c.emit("workflow.retired", "workflow", id, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}
