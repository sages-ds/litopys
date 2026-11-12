package verbs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/privacy"
	"litopys.smart-gw.co.ua/internal/pg"
)

// ---------------------------------------------------------------------------
// actors
// ---------------------------------------------------------------------------

// RegisterHuman is the system path "людину реєструє система при першому вході" (actor.register notes).
// Idempotent by external_identity (email / principal). The event is self-authored: actor_id = the new actor.
func (s *Service) RegisterHuman(ctx context.Context, tenant uuid.UUID, displayName, externalIdentity string) (uuid.UUID, bool, error) {
	if err := checkLen("display_name", displayName, 1, 200); err != nil {
		return uuid.Nil, false, err
	}
	if err := checkLen("external_identity", externalIdentity, 1, 320); err != nil {
		return uuid.Nil, false, err
	}
	var existing uuid.UUID
	// the directory of humans spans owners: system scope (RLS of 0008)
	err := s.DB.QueryRow(pg.WithScope(ctx, pg.ScopeSystem), `SELECT actor_id FROM projection.actor
	        WHERE tenant_id = $1 AND kind = 'human' AND external_identity = $2`, tenant, externalIdentity).Scan(&existing)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, err
	}
	id := uuid.New()
	err = s.exec(ctx, s.DB, Principal{TenantID: tenant, ActorID: id, Owner: id}, func(c *cmd) error {
		_, err := c.emit("actor.registered", "actor", id, nil, nil,
			map[string]any{"kind": "human", "display_name": displayName, "external_identity": externalIdentity})
		return err
	})
	return id, err == nil, err
}

type ActorRegisterIn struct {
	Kind             string           `json:"kind"`
	DisplayName      string           `json:"display_name"`
	Client           json.RawMessage  `json:"client,omitempty"`
	ExternalIdentity *string          `json:"external_identity,omitempty"`
	Executor         *ExecutorProfile `json:"executor,omitempty"` // agent / service only (0024)
}

// ActorRegister registers an agent / service / automation acting on behalf of the principal's human.
// Idempotent by (on_behalf_of, kind, external_identity).
func (s *Service) ActorRegister(ctx context.Context, p Principal, in ActorRegisterIn) (uuid.UUID, Ack, error) {
	if err := checkEnum("kind", in.Kind, "agent", "service", "automation"); err != nil {
		return uuid.Nil, Ack{}, err // humans are registered by the system (RegisterHuman)
	}
	if err := checkLen("display_name", in.DisplayName, 1, 200); err != nil {
		return uuid.Nil, Ack{}, err
	}
	if in.Executor != nil {
		if in.Kind == "automation" {
			return uuid.Nil, Ack{}, invalid("executor", "an executor profile belongs to an agent or a service")
		}
		if err := in.Executor.check(); err != nil {
			return uuid.Nil, Ack{}, err
		}
	}
	var id uuid.UUID
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		if err := c.requireHumanForAccess(in.Executor); err != nil {
			return err
		}
		root, err := c.owner(p.ActorID)
		if err != nil {
			return err
		}
		err = c.tx.QueryRow(ctx, `SELECT actor_id FROM projection.actor
		        WHERE tenant_id = $1 AND on_behalf_of = $2 AND kind = $3 AND external_identity IS NOT DISTINCT FROM $4`,
			p.TenantID, root, in.Kind, in.ExternalIdentity).Scan(&id)
		if err == nil {
			a.Already = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		id = uuid.New()
		payload := map[string]any{"kind": in.Kind, "display_name": in.DisplayName, "on_behalf_of": root}
		if len(in.Client) > 0 {
			payload["client"] = in.Client
		}
		if in.ExternalIdentity != nil {
			payload["external_identity"] = *in.ExternalIdentity
		}
		if in.Executor != nil {
			payload["executor"] = in.Executor
		}
		e, err := c.emit("actor.registered", "actor", id, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return id, a, err
}

type ActorAmendIn struct {
	ActorID     uuid.UUID       `json:"actor_id"`
	DisplayName *string         `json:"display_name,omitempty"`
	Executor    json.RawMessage `json:"executor,omitempty"` // the whole profile; JSON null removes it
}

// ActorAmend renames an agent / service of the same human or replaces its executor profile (0024).
func (s *Service) ActorAmend(ctx context.Context, p Principal, in ActorAmendIn) (Ack, error) {
	payload := map[string]any{}
	var widened *ExecutorProfile // a new profile; widening its access is a person's call
	if in.DisplayName != nil {
		if err := checkLen("display_name", *in.DisplayName, 1, 200); err != nil {
			return Ack{}, err
		}
		payload["display_name"] = *in.DisplayName
	}
	if len(in.Executor) > 0 {
		if string(in.Executor) == "null" {
			payload["executor"] = nil
		} else {
			var x ExecutorProfile
			dec := json.NewDecoder(bytes.NewReader(in.Executor))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&x); err != nil {
				return Ack{}, invalid("executor", "%v", err)
			}
			if err := x.check(); err != nil {
				return Ack{}, err
			}
			payload["executor"] = x
			widened = &x
		}
	}
	if len(payload) == 0 {
		return Ack{}, invalid("actor.amend", "nothing to change")
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireHumanForAccess(widened); err != nil {
			return err
		}
		if err := c.mine(in.ActorID); err != nil {
			return err
		}
		var kind string
		var revoked bool
		if err := c.tx.QueryRow(ctx, `SELECT kind, revoked_at IS NOT NULL FROM projection.actor WHERE actor_id = $1`, in.ActorID).Scan(&kind, &revoked); err != nil {
			return err
		}
		if kind == "human" || kind == "automation" {
			if _, ok := payload["executor"]; ok {
				return invalid("executor", "an executor profile belongs to an agent or a service")
			}
		}
		if kind == "human" {
			return fail("forbidden", "a person is renamed through their identity provider")
		}
		if revoked {
			return fail("forbidden", "actor %s is revoked", in.ActorID)
		}
		e, err := c.emit("actor.amended", "actor", in.ActorID, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

// ActorRevoke stops an actor of the same human from writing; history stays (invariant 1).
func (s *Service) ActorRevoke(ctx context.Context, p Principal, actorID uuid.UUID, reason string) (Ack, error) {
	if err := checkLen("reason", reason, 1, 500); err != nil {
		return Ack{}, err
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.mine(actorID); err != nil {
			return err
		}
		var revoked *time.Time
		if err := c.tx.QueryRow(ctx, `SELECT revoked_at FROM projection.actor WHERE actor_id = $1`, actorID).Scan(&revoked); err != nil {
			return err
		}
		if revoked != nil {
			a.Already = true
			return nil
		}
		e, err := c.emit("actor.revoked", "actor", actorID, nil, nil, map[string]any{"reason": reason})
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

// ---------------------------------------------------------------------------
// runs
// ---------------------------------------------------------------------------

type RunStartIn struct {
	ParentRunID *uuid.UUID      `json:"parent_run_id,omitempty"`
	Client      json.RawMessage `json:"client"`
	Purpose     *string         `json:"purpose,omitempty"`
}

func (c *cmd) startRun(in RunStartIn) (uuid.UUID, error) {
	if in.ParentRunID != nil {
		var author uuid.UUID
		err := c.tx.QueryRow(c.ctx, `SELECT actor_id FROM projection.run WHERE run_id = $1`, *in.ParentRunID).Scan(&author)
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, fail("run_not_found", "parent run %s", *in.ParentRunID)
		}
		if err != nil {
			return uuid.Nil, err
		}
		if err := c.mine(author); err != nil {
			return uuid.Nil, err
		}
	}
	client := in.Client
	if len(client) == 0 {
		client = json.RawMessage(`{}`)
	}
	payload := map[string]any{"client": client}
	if in.ParentRunID != nil {
		payload["parent_run_id"] = *in.ParentRunID
	}
	if in.Purpose != nil {
		if err := checkLen("purpose", *in.Purpose, 0, 500); err != nil {
			return uuid.Nil, err
		}
		payload["purpose"] = *in.Purpose
	}
	id := uuid.New()
	c.run = id // the run.started event belongs to the run it starts
	if _, err := c.emit("run.started", "run", id, nil, nil, payload); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// RunStart starts a run explicitly (sub-agents pass parent_run_id). context.open does it implicitly.
func (s *Service) RunStart(ctx context.Context, p Principal, in RunStartIn) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		var err error
		id, err = c.startRun(in)
		return err
	})
	return id, err
}

// RunFinish closes a run; a finished run gives already.
func (s *Service) RunFinish(ctx context.Context, p Principal, runID uuid.UUID, outcome string, note *string) (Ack, error) {
	if err := checkEnum("outcome", outcome, runOutcomes...); err != nil {
		return Ack{}, err
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		var author uuid.UUID
		var finished *time.Time
		err := c.tx.QueryRow(ctx, `SELECT actor_id, finished_at FROM projection.run WHERE run_id = $1`, runID).Scan(&author, &finished)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail("run_not_found", "run %s", runID)
		}
		if err != nil {
			return err
		}
		if err := c.mine(author); err != nil {
			return err
		}
		if finished != nil {
			a.Already = true
			return nil
		}
		payload := map[string]any{"outcome": outcome}
		if note != nil {
			payload["note"] = *note
		}
		c.run = runID
		e, err := c.emit("run.finished", "run", runID, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

// ---------------------------------------------------------------------------
// contexts
// ---------------------------------------------------------------------------

type ContextOpenIn struct {
	Project    string          `json:"project"`
	Attach     string          `json:"attach"` // context id of THIS terminal, or "none"
	AutoCreate bool            `json:"auto_create,omitempty"`
	RunID      *uuid.UUID      `json:"run_id,omitempty"`
	Client     json.RawMessage `json:"client,omitempty"`
}

type ContextOpenOut struct {
	ContextID      uuid.UUID `json:"context_id"`
	RunID          uuid.UUID `json:"run_id"`
	Attached       bool      `json:"attached"`
	AttachMiss     string    `json:"attach_miss,omitempty"` // closed | not_found | other_project
	ProjectCreated bool      `json:"project_created,omitempty"`
	// bundle (context_bundle read model) is filled by the query side.
}

// ContextOpen opens (or re-attaches) the principal's context in a project (verbs.yaml context.open).
func (s *Service) ContextOpen(ctx context.Context, p Principal, in ContextOpenIn) (ContextOpenOut, error) {
	var out ContextOpenOut
	if err := checkProjectName("project", in.Project); err != nil {
		return out, err
	}
	var attach uuid.UUID
	if in.Attach != "none" {
		id, err := uuid.Parse(in.Attach)
		if err != nil {
			return out, invalid("attach", "must be a context id or \"none\"")
		}
		attach = id
	}
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		// the run of the new context: an explicit open run, the caller's, else a new one with the client info
		runResolved := false
		resolveRun := func() error {
			if runResolved {
				return nil
			}
			runResolved = true
			switch {
			case in.RunID != nil:
				var author uuid.UUID
				var finished *time.Time
				err := c.tx.QueryRow(ctx, `SELECT actor_id, finished_at FROM projection.run WHERE run_id = $1`, *in.RunID).Scan(&author, &finished)
				if errors.Is(err, pgx.ErrNoRows) || (err == nil && finished != nil) {
					return fail("run_not_found", "open run %s", *in.RunID)
				}
				if err != nil {
					return err
				}
				if err := c.mine(author); err != nil {
					return err
				}
				c.run = *in.RunID
			case p.RunID != uuid.Nil:
				c.run = p.RunID
			default:
				if _, err := c.startRun(RunStartIn{Client: in.Client}); err != nil {
					return err
				}
			}
			return nil
		}

		projectID, err := c.projectID(in.Project)
		if Code(err) == "project_not_found" && in.AutoCreate {
			if err := c.allowNewProject(in.Project); err != nil {
				return err
			}
			// the run first: project.declared then carries the client of the run; a re-attach into a
			// project that did not exist cannot succeed, so the run is not wasted
			if err := resolveRun(); err != nil {
				return err
			}
			projectID = uuid.New()
			if _, err := c.emit("project.declared", "project", projectID, nil, nil,
				map[string]any{"name": in.Project, "auto_created": true}); err != nil {
				return err
			}
			out.ProjectCreated = true
		} else if err != nil {
			return err
		}

		if attach != uuid.Nil { // re-attach: no event, a miss falls through to a new context
			var proj, author, run uuid.UUID
			var closed *time.Time
			err := c.tx.QueryRow(ctx, `SELECT project_id, actor_id, run_id, closed_at FROM projection.context WHERE context_id = $1`,
				attach).Scan(&proj, &author, &run, &closed)
			switch {
			case errors.Is(err, pgx.ErrNoRows) || (err == nil && c.mine(author) != nil):
				out.AttachMiss = "not_found"
			case err != nil:
				return err
			case closed != nil:
				out.AttachMiss = "closed"
			case proj != projectID:
				out.AttachMiss = "other_project"
			default:
				out.ContextID, out.RunID, out.Attached = attach, run, true
				return nil
			}
		}

		if err := resolveRun(); err != nil {
			return err
		}
		out.RunID = c.run
		out.ContextID = uuid.New()
		// attached_to stays unset: a miss opens a NEW context, it does not continue the old one.
		_, err = c.emit("context.opened", "context", out.ContextID, nil, nil, map[string]any{"project_id": projectID})
		return err
	})
	return out, err
}

// context loads a context of the principal's human.
func (c *cmd) context(id uuid.UUID) (project uuid.UUID, run uuid.UUID, closed *time.Time, err error) {
	var author uuid.UUID
	err = c.tx.QueryRow(c.ctx, `SELECT project_id, run_id, actor_id, closed_at FROM projection.context WHERE context_id = $1`,
		id).Scan(&project, &run, &author, &closed)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, nil, fail("context_not_found", "context %s", id)
	}
	if err != nil {
		return
	}
	if c.mine(author) != nil {
		return uuid.Nil, uuid.Nil, nil, fail("context_not_found", "context %s", id)
	}
	return
}

func cleanSummary(s string) (string, error) {
	s = privacy.MaskText(privacy.StripPrivate(s))
	return s, checkLen("summary", s, 1, 8192)
}

// checkpoint writes context.checkpointed unless the same summary landed in the context within 24h.
func (c *cmd) checkpoint(contextID uuid.UUID, summary string, current *string) (Ack, error) {
	var dup bool
	err := c.tx.QueryRow(c.ctx, `SELECT EXISTS (SELECT 1 FROM journal
	        WHERE tenant_id = $1 AND entity_type = 'context' AND entity_id = $2 AND type = 'context.checkpointed'
	          AND payload->>'summary' = $3 AND recorded_at > now() - interval '24 hours')`,
		c.p.TenantID, contextID, summary).Scan(&dup)
	if err != nil {
		return Ack{}, err
	}
	if dup {
		return Ack{Deduped: true}, nil
	}
	payload := map[string]any{"summary": summary}
	if current != nil {
		payload["current"] = *current
	}
	e, err := c.emit("context.checkpointed", "context", contextID, nil, nil, payload)
	if err != nil {
		return Ack{}, err
	}
	return ack(e), nil
}

// ContextCheckpoint records an intermediate summary (and "where we are now").
func (s *Service) ContextCheckpoint(ctx context.Context, p Principal, contextID uuid.UUID, summary string, current *string) (Ack, error) {
	summary, err := cleanSummary(summary)
	if err != nil {
		return Ack{}, err
	}
	if current != nil {
		m := privacy.MaskText(privacy.StripPrivate(*current))
		if err := checkLen("current", m, 0, 2000); err != nil {
			return Ack{}, err
		}
		current = &m
	}
	var a Ack
	err = s.exec(ctx, s.DB, p, func(c *cmd) error {
		_, run, _, err := c.context(contextID)
		if err != nil {
			return err
		}
		c.run = run
		a, err = c.checkpoint(contextID, summary, current)
		return err
	})
	return a, err
}

// Checkpoint is the position for incremental readers after close (verbs.yaml $defs/Checkpoint).
type Checkpoint struct {
	ServerTime      time.Time   `json:"server_time"`
	Project         string      `json:"project"`
	LastSeq         int64       `json:"last_seq"`
	ContextEventIDs []uuid.UUID `json:"context_event_ids"`
}

type ContextCloseOut struct {
	Closed          bool         `json:"closed"`
	AlreadyClosed   bool         `json:"already_closed,omitempty"`
	ClosedAt        *time.Time   `json:"closed_at,omitempty"`
	Checkpoint      *Checkpoint  `json:"checkpoint,omitempty"`
	Outcomes        []OutcomeOut `json:"outcomes,omitempty"`
	MissingOutcomes []string     `json:"missing_outcomes,omitempty"`
	Hints           []string     `json:"hints,omitempty"`
}

// ContextClose closes the context. A closed one gives already_closed and its summary is kept as a
// checkpoint; the run finishes when finishRun (default true) and no other context of it is open.
func (s *Service) ContextClose(ctx context.Context, p Principal, contextID uuid.UUID, summary string, finishRun *bool) (ContextCloseOut, error) {
	return s.ContextCloseWith(ctx, p, contextID, CloseIn{Summary: summary, FinishRun: finishRun})
}

// ContextCloseWith is ContextClose with the outcomes of the session per subject and its next steps:
// the outcome statements are written in the context before it closes, in the same transaction. Closing a closed
// context writes no outcomes.
func (s *Service) ContextCloseWith(ctx context.Context, p Principal, contextID uuid.UUID, in CloseIn) (ContextCloseOut, error) {
	var out ContextCloseOut
	summary, err := cleanSummary(in.Summary)
	if err != nil {
		return out, err
	}
	if err := in.clean(); err != nil {
		return out, err
	}
	finishRun := in.FinishRun
	err = s.exec(ctx, s.DB, p, func(c *cmd) error {
		project, run, closed, err := c.context(contextID)
		if err != nil {
			return err
		}
		c.run = run
		if closed != nil {
			out.AlreadyClosed, out.ClosedAt = true, closed
			if len(in.Outcomes) > 0 || len(in.NextSteps) > 0 {
				out.Hints = append(out.Hints, "the session was already closed: outcomes and next_steps were not written")
			}
			_, err := c.checkpoint(contextID, summary, nil)
			return err
		}
		var projectName string
		if err := c.tx.QueryRow(ctx, `SELECT name FROM projection.project WHERE project_id = $1`, project).Scan(&projectName); err != nil {
			return err
		}
		for _, o := range in.Outcomes {
			w, err := c.writeOutcome(contextID, projectName, o)
			if err != nil {
				return err
			}
			out.Outcomes = append(out.Outcomes, w)
		}
		if out.MissingOutcomes, out.Hints, err = c.closeHints(run, projectName, in.Outcomes); err != nil {
			return err
		}
		c.run = run // writing into another project's subject keeps the session's run
		payload := map[string]any{"summary": summary}
		if len(in.NextSteps) > 0 {
			payload["next_steps"] = in.NextSteps
		}
		e, err := c.emit("context.closed", "context", contextID, nil, nil, payload)
		if err != nil {
			return err
		}
		out.Closed, out.ClosedAt = true, &e.RecordedAt
		if finishRun == nil || *finishRun {
			var others bool
			if err := c.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projection.context
			        WHERE run_id = $1 AND closed_at IS NULL)`, run).Scan(&others); err != nil {
				return err
			}
			var finished *time.Time
			if err := c.tx.QueryRow(ctx, `SELECT finished_at FROM projection.run WHERE run_id = $1`, run).Scan(&finished); err != nil {
				return err
			}
			if !others && finished == nil {
				if _, err := c.emit("run.finished", "run", run, nil, nil, map[string]any{"outcome": "ok"}); err != nil {
					return err
				}
			}
		}
		cp := &Checkpoint{ServerTime: e.RecordedAt, ContextEventIDs: []uuid.UUID{}}
		if err := c.tx.QueryRow(ctx, `SELECT p.name, h.last_seq FROM projection.project p, journal_head h
		        WHERE p.project_id = $1 AND h.tenant_id = $2`, project, p.TenantID).Scan(&cp.Project, &cp.LastSeq); err != nil {
			return err
		}
		rows, err := c.tx.Query(ctx, `SELECT asserted_event_id FROM projection.statement WHERE context_id = $1 ORDER BY asserted_seq`, contextID)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		cp.ContextEventIDs = append(cp.ContextEventIDs, ids...)
		out.Checkpoint = cp
		return nil
	})
	return out, err
}
