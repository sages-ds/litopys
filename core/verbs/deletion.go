package verbs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/privacy"
	"litopys.smart-gw.co.ua/internal/pg"
)

// Quarantine: any actor who sees a record or a task hides it at once from every read, search and the
// journal as consumers read it; nothing is erased. Only a human releases it, as only a human gives a GO.

// JournalQuarantine hides target; a repeated quarantine gives already. The reason is masked by privacy before it
// is written: it is shown to everyone who follows the ref.
func (s *Service) JournalQuarantine(ctx context.Context, p Principal, target EntityRef, reason string, secret bool) (Ack, error) {
	return s.quarantine(ctx, p, target, reason, secret, true)
}

// JournalRelease shows a quarantined target again — a human only (release_requires_human).
func (s *Service) JournalRelease(ctx context.Context, p Principal, target EntityRef, reason string) (Ack, error) {
	return s.quarantine(ctx, p, target, reason, false, false)
}

func (s *Service) quarantine(ctx context.Context, p Principal, target EntityRef, reason string, secret, on bool) (Ack, error) {
	if err := checkEnum("target.entity_type", target.EntityType, purgeable...); err != nil {
		return Ack{}, err
	}
	if err := checkLen("reason", reason, 1, 1000); err != nil {
		return Ack{}, err
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		if !on {
			kind, err := c.actorKind()
			if err != nil {
				return err
			}
			if kind != "human" {
				return fail("release_requires_human", "releasing from quarantine is the user's decision; an %s cannot do it — ask the user", kind)
			}
		}
		purged, quarantined, err := c.quarantineState(target)
		if err != nil {
			return err
		}
		if purged {
			return fail("already_purged", "%s %s is erased", target.EntityType, target.EntityID)
		}
		if quarantined == on {
			if !on {
				return fail("not_quarantined", "%s %s is not in quarantine", target.EntityType, target.EntityID)
			}
			a.Already = true
			return nil
		}
		live := false
		if on && target.EntityType == "task" {
			if live, err = c.taskLive(target.EntityID); err != nil {
				return err
			}
		}
		typ, payload := "journal.released", map[string]any{"target": target, "reason": privacy.MaskText(reason)}
		if on {
			typ = "journal.quarantined"
			if secret {
				payload["secret"] = true
			}
		}
		e, err := c.emit(typ, target.EntityType, target.EntityID, nil, nil, payload)
		if err != nil {
			return err
		}
		a = ack(e)
		if live {
			if err := c.askDependents(target.EntityID, "quarantined"); err != nil {
				return err
			}
		}
		if on && secret { // a secret is a candidate for erasure: the proposal waits for the user's GO
			_, err = c.propose(target, "purge", reason, true)
		}
		if !on { // the user released it: the erase the quarantine proposed is rejected with the release
			var open uuid.UUID
			err = c.tx.QueryRow(c.ctx, `SELECT deletion_id FROM projection.deletion
			        WHERE target_id = $1 AND action = 'purge' AND secret AND state = 'open'`, target.EntityID).Scan(&open)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			_, err = c.emit("deletion.rejected", "deletion", open, nil, nil, map[string]any{"reason": "released: " + privacy.MaskText(reason)})
		}
		return err
	})
	return a, err
}

// quarantineState locks a record or task of the principal's owner, live or not.
func (c *cmd) quarantineState(r EntityRef) (purged, quarantined bool, err error) {
	if r.EntityType == "statement" {
		if _, _, err := c.statement(r.EntityID); err != nil {
			return false, false, err
		}
		err = c.tx.QueryRow(c.ctx, `SELECT purged, quarantined FROM projection.statement WHERE statement_id = $1`,
			r.EntityID).Scan(&purged, &quarantined)
		return
	}
	err = c.tx.QueryRow(c.ctx, `SELECT t.purged, t.quarantined FROM projection.task t JOIN projection.project pr ON pr.project_id = t.project_id
	        WHERE t.task_id = $1 AND pr.owner_id = $2 FOR UPDATE OF t`, r.EntityID, c.me).Scan(&purged, &quarantined)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, fail("task_not_found", "task %s", r.EntityID)
	}
	return
}

// ---------------------------------------------------------------------------
// deletion proposals D-N
// ---------------------------------------------------------------------------

// retractOwn: an agent or a service retracts only what it wrote itself; someone else's record goes through a
// proposal the user decides. A human retracts any record of their own.
func (c *cmd) retractOwn(id uuid.UUID) error {
	kind, err := c.actorKind()
	if err != nil || kind == "human" {
		return err
	}
	var author uuid.UUID
	if err := c.tx.QueryRow(c.ctx, `SELECT actor_id FROM projection.statement WHERE statement_id = $1`, id).Scan(&author); err != nil {
		return err
	}
	if author != c.p.ActorID {
		return fail("retract_not_own", "an %s retracts only its own records; propose it to the user instead (deletion_propose, action retract)", kind)
	}
	return nil
}

// DeletionOut answers propose, approve and reject: the proposal and its state.
type DeletionOut struct {
	DeletionID uuid.UUID      `json:"deletion_id"`
	Ref        string         `json:"ref"` // <project>/D-N
	State      string         `json:"state"`
	Impact     map[string]int `json:"impact,omitempty"` // propose: the counts the GO will be given on
	EventID    uuid.UUID      `json:"event_id,omitempty"`
	Seq        int64          `json:"seq,omitempty"`
	Already    bool           `json:"already,omitempty"`
}

// DeletionPropose: anyone proposes to retract or erase a record or a task; nothing changes until a human's GO.
// An open proposal for the same target and action answers already.
func (s *Service) DeletionPropose(ctx context.Context, p Principal, target EntityRef, action, reason string, secret bool) (DeletionOut, error) {
	if err := checkEnum("target.entity_type", target.EntityType, purgeable...); err != nil {
		return DeletionOut{}, err
	}
	if err := checkEnum("action", action, "retract", "purge"); err != nil {
		return DeletionOut{}, err
	}
	if err := checkLen("reason", reason, 1, 1000); err != nil {
		return DeletionOut{}, err
	}
	var out DeletionOut
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		var err error
		out, err = c.propose(target, action, reason, secret)
		return err
	})
	return out, err
}

// propose writes deletion.proposed (or finds the open one) — also called by a quarantine with secret.
func (c *cmd) propose(target EntityRef, action, reason string, secret bool) (DeletionOut, error) {
	var out DeletionOut
	purged, _, err := c.quarantineState(target)
	if err != nil {
		return out, err
	}
	if purged {
		return out, fail("already_purged", "%s %s is erased", target.EntityType, target.EntityID)
	}
	var project uuid.UUID
	var retracted bool
	if target.EntityType == "statement" {
		err = c.tx.QueryRow(c.ctx, `SELECT project_id, status = 'retracted' FROM projection.statement WHERE statement_id = $1`,
			target.EntityID).Scan(&project, &retracted)
	} else {
		err = c.tx.QueryRow(c.ctx, `SELECT project_id, retracted FROM projection.task WHERE task_id = $1`, target.EntityID).Scan(&project, &retracted)
	}
	if err != nil {
		return out, err
	}
	if action == "retract" && retracted {
		return out, fail("already_retracted", "%s %s is retracted already", target.EntityType, target.EntityID)
	}
	err = c.tx.QueryRow(c.ctx, `SELECT d.deletion_id, p.name || '/D-' || d.num, d.state FROM projection.deletion d
	        JOIN projection.project p ON p.project_id = d.project_id
	       WHERE d.target_id = $1 AND d.action = $2 AND d.state = 'open'`, target.EntityID, action).Scan(&out.DeletionID, &out.Ref, &out.State)
	if err == nil {
		out.Already = true
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	impact, err := c.impactCounts(target)
	if err != nil {
		return out, err
	}
	var num int
	var name string
	if err := c.tx.QueryRow(c.ctx, `SELECT COALESCE((SELECT max(num) FROM projection.deletion WHERE project_id = $1), 0) + 1, name
	        FROM projection.project WHERE project_id = $1`, project).Scan(&num, &name); err != nil {
		return out, err
	}
	payload := map[string]any{"project_id": project, "num": num, "target": target, "action": action,
		"reason": privacy.MaskText(reason), "impact": impact}
	if secret {
		payload["secret"] = true
	}
	id := uuid.New()
	e, err := c.emit("deletion.proposed", "deletion", id, nil, nil, payload)
	if err != nil {
		return out, err
	}
	return DeletionOut{DeletionID: id, Ref: name + "/D-" + strconv.Itoa(num), State: "open", Impact: impact, EventID: e.EventID, Seq: e.Seq}, nil
}

// impactCounts: the live incoming links of the target by relation and the outcomes about it — what a GO is given on
// (approve compares them again).
func (c *cmd) impactCounts(target EntityRef) (map[string]int, error) {
	out := map[string]int{}
	rows, err := c.tx.Query(c.ctx, `SELECT relation, count(*)::int FROM projection.link
	        WHERE to_type = $1 AND to_id = $2 AND NOT retired GROUP BY relation`, target.EntityType, target.EntityID)
	if err != nil {
		return nil, err
	}
	var rel string
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&rel, &n}, func() error { out[rel] = n; return nil }); err != nil {
		return nil, err
	}
	if err := c.tx.QueryRow(c.ctx, `SELECT count(*)::int FROM projection.statement
	        WHERE type = 'outcome' AND outcome->>'subject_id' = $1 AND NOT hidden AND status <> 'retracted'`,
		target.EntityID.String()).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		out["outcomes"] = n
	}
	return out, nil
}

type deletionRow struct {
	id     uuid.UUID
	ref    string
	state  string
	action string
	target EntityRef
	impact map[string]int
}

// deletion locks a proposal of the principal's owner.
func (c *cmd) deletion(id uuid.UUID) (deletionRow, error) {
	var d deletionRow
	var raw []byte
	err := c.tx.QueryRow(c.ctx, `SELECT d.deletion_id, p.name || '/D-' || d.num, d.state, d.action, d.target_type, d.target_id, d.impact
	          FROM projection.deletion d JOIN projection.project p ON p.project_id = d.project_id
	         WHERE d.deletion_id = $1 AND d.owner_id = $2 FOR UPDATE OF d`, id, c.me).
		Scan(&d.id, &d.ref, &d.state, &d.action, &d.target.EntityType, &d.target.EntityID, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, fail("deletion_not_found", "deletion %s", id)
	}
	if err != nil {
		return d, err
	}
	return d, json.Unmarshal(raw, &d.impact)
}

// decide: the checks of approve and reject — a human, an open proposal.
func (c *cmd) decide(id uuid.UUID) (deletionRow, error) {
	if err := c.requireLiveActor(); err != nil {
		return deletionRow{}, err
	}
	kind, err := c.actorKind()
	if err != nil {
		return deletionRow{}, err
	}
	if kind != "human" {
		return deletionRow{}, fail("deletion_requires_human", "a GO on a deletion is the user's decision; an %s cannot give it — ask the user", kind)
	}
	d, err := c.deletion(id)
	if err != nil {
		return d, err
	}
	if d.state != "open" {
		return d, fail("deletion_closed", "%s is %s", d.ref, d.state)
	}
	return d, nil
}

// DeletionApprove: the human's GO on an impact — seen, the counts of the impact_changed answer or of the list the
// user was shown; nil = the proposal's snapshot. A different impact now is refused (impact_changed) with the new
// counts, and the GO is given again on them. A retract is done here; an erase is left to the worker (ExecutePurges).
func (s *Service) DeletionApprove(ctx context.Context, p Principal, id uuid.UUID, seen map[string]int, note *string) (DeletionOut, error) {
	if err := optLen("note", note, 1000); err != nil {
		return DeletionOut{}, err
	}
	var out DeletionOut
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		d, err := c.decide(id)
		if err != nil {
			return err
		}
		now, err := c.impactCounts(d.target)
		if err != nil {
			return err
		}
		if seen == nil {
			seen = d.impact
		}
		if !maps.Equal(now, seen) {
			b, _ := json.Marshal(now)
			return fail("impact_changed", "%s: the impact is not what the user saw — now %s; show it to the user (deletion_impact) and, on a new GO, pass these counts as impact", d.ref, b)
		}
		payload := map[string]any{}
		if note != nil {
			payload["note"] = privacy.MaskText(*note)
		}
		e, err := c.emit("deletion.approved", "deletion", id, nil, nil, payload)
		if err != nil {
			return err
		}
		out = DeletionOut{DeletionID: id, Ref: d.ref, State: "approved", EventID: e.EventID, Seq: e.Seq}
		if d.action != "retract" {
			return nil
		}
		out.State = "done"
		why := map[string]any{"reason": "deletion " + d.ref}
		var retracted bool
		if d.target.EntityType == "task" {
			err = c.tx.QueryRow(c.ctx, `SELECT retracted FROM projection.task WHERE task_id = $1`, d.target.EntityID).Scan(&retracted)
		} else {
			err = c.tx.QueryRow(c.ctx, `SELECT status = 'retracted' FROM projection.statement WHERE statement_id = $1`, d.target.EntityID).Scan(&retracted)
		}
		if err != nil || retracted { // retracted meanwhile by its author: nothing left to do
			return err
		}
		live := false
		if d.target.EntityType == "task" {
			if live, err = c.taskLive(d.target.EntityID); err != nil {
				return err
			}
		}
		if _, err = c.emit(d.target.EntityType+".retracted", d.target.EntityType, d.target.EntityID, nil, nil, why); err != nil || !live {
			return err
		}
		return c.askDependents(d.target.EntityID, "retracted")
	})
	return out, err
}

// DeletionReject: the human keeps the target.
func (s *Service) DeletionReject(ctx context.Context, p Principal, id uuid.UUID, reason string) (DeletionOut, error) {
	if err := checkLen("reason", reason, 1, 1000); err != nil {
		return DeletionOut{}, err
	}
	var out DeletionOut
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		d, err := c.decide(id)
		if err != nil {
			return err
		}
		e, err := c.emit("deletion.rejected", "deletion", id, nil, nil, map[string]any{"reason": privacy.MaskText(reason)})
		if err == nil {
			out = DeletionOut{DeletionID: id, Ref: d.ref, State: "rejected", EventID: e.EventID, Seq: e.Seq}
		}
		return err
	})
	return out, err
}

// ExecutePurges erases the targets of approved purge proposals (owner's decision P1: `litopys worker` holds the admin
// pool, the public facade does not). Each one in its own transaction, as the human who gave the GO: journal.purged
// and deletion.executed together. Returns how many were done; the first error stops the batch.
func (s *Service) ExecutePurges(ctx context.Context, tenant uuid.UUID) (int, error) {
	if s.Admin == nil {
		return 0, fail("forbidden", "executing purges needs the admin role")
	}
	rows, err := s.DB.Query(pg.WithScope(ctx, pg.ScopeSystem), `SELECT deletion_id, decided_by FROM projection.deletion
	        WHERE tenant_id = $1 AND state = 'approved' AND action = 'purge' ORDER BY last_seq LIMIT 50`, tenant)
	if err != nil {
		return 0, err
	}
	type job struct{ id, by uuid.UUID }
	jobs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (job, error) {
		var j job
		return j, r.Scan(&j.id, &j.by)
	})
	if err != nil {
		return 0, err
	}
	done := 0
	for _, j := range jobs {
		err := s.exec(ctx, s.Admin, Principal{TenantID: tenant, ActorID: j.by}, func(c *cmd) error {
			d, err := c.deletion(j.id)
			if err != nil || d.state != "approved" { // taken by another worker meanwhile
				return err
			}
			live := false
			if d.target.EntityType == "task" {
				if live, err = c.taskLive(d.target.EntityID); err != nil {
					return err
				}
			}
			var seq int64
			if err := c.tx.QueryRow(c.ctx, `SELECT journal_purge($1,$2,$3,$4,NULL,$5,$5,$5,$5)`,
				tenant, d.target.EntityType, d.target.EntityID, "deletion "+d.ref, j.by).Scan(&seq); err != nil {
				return err
			}
			var a Ack
			if err := c.projectPurge(seq, &a); err != nil {
				return err
			}
			if live {
				if err := c.askDependents(d.target.EntityID, "purged"); err != nil {
					return err
				}
			}
			c.run, c.cause = j.by, a.EventID
			_, err = c.emit("deletion.executed", "deletion", j.id, nil, nil, map[string]any{"purge_seq": seq})
			return err
		})
		if err != nil {
			return done, fmt.Errorf("deletion %s: %w", j.id, err)
		}
		done++
	}
	return done, nil
}

// ---------------------------------------------------------------------------
// dependents
// ---------------------------------------------------------------------------

// automationIdentity: the owner's automation that writes the consequences of a deletion.
const automationIdentity = "litopys:deletion"

// taskLive: the task is neither retracted, quarantined nor erased — asked before an event that may hide it, so the
// dependents are asked once, when the task goes.
func (c *cmd) taskLive(id uuid.UUID) (bool, error) {
	var hidden bool
	err := c.tx.QueryRow(c.ctx, `SELECT hidden FROM projection.task WHERE task_id = $1`, id).Scan(&hidden)
	return !hidden, err
}

// askDependents: a task that open tasks depend on is gone (retracted, quarantined, erased). Nothing unblocks silently:
// in the project of every dependent the owner's automation writes a question record that mentions both and asks what
// next — by refs only, the gone task's title may be the secret.
func (c *cmd) askDependents(target uuid.UUID, state string) error {
	var targetRef string
	if err := c.tx.QueryRow(c.ctx, `SELECT p.name || '/T-' || t.num FROM projection.task t JOIN projection.project p ON p.project_id = t.project_id
	        WHERE t.task_id = $1`, target).Scan(&targetRef); err != nil {
		return err
	}
	rows, err := c.tx.Query(c.ctx, `SELECT t.task_id, t.project_id, p.name || '/T-' || t.num FROM projection.link l
	          JOIN projection.task t ON t.task_id = l.from_id JOIN projection.project p ON p.project_id = t.project_id
	         WHERE l.to_type = 'task' AND l.to_id = $1 AND l.from_type = 'task' AND l.relation = 'depends_on' AND NOT l.retired
	           AND t.open AND NOT t.hidden
	         ORDER BY l.asserted_seq`, target)
	if err != nil {
		return err
	}
	type dependent struct {
		id, project uuid.UUID
		ref         string
	}
	deps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dependent, error) {
		var d dependent
		return d, r.Scan(&d.id, &d.project, &d.ref)
	})
	if err != nil || len(deps) == 0 {
		return err
	}
	auto, err := c.automation(automationIdentity)
	if err != nil {
		return err
	}
	me := c.p.ActorID
	c.p.ActorID = auto
	defer func() { c.p.ActorID = me }()
	for _, d := range deps {
		num, err := c.nextRecordNum(d.project)
		if err != nil {
			return err
		}
		id := uuid.New()
		if _, err := c.emit("statement.asserted", "statement", id, nil, nil, map[string]any{"project_id": d.project, "type": "question",
			"title": "Dependency " + targetRef + " is gone (" + state + ") — what next for " + d.ref + "?",
			"body":  d.ref + " waits on " + targetRef + ", which was " + stateWord[state] + ". The task is not unblocked: retire the dependency, close the task or pick another target.",
			"num":   num, "refs": []string{targetRef, d.ref}}); err != nil {
			return err
		}
		if err := c.setMentions(EntityRef{EntityType: "statement", EntityID: id},
			[]EntityRef{{EntityType: "task", EntityID: target}, {EntityType: "task", EntityID: d.id}}); err != nil {
			return err
		}
	}
	return nil
}

var stateWord = map[string]string{"retracted": "retracted", "quarantined": "quarantined", "purged": "erased"}

// automation: an automation actor of the owner by identity, registered on first use.
func (c *cmd) automation(identity string) (uuid.UUID, error) {
	var id uuid.UUID
	err := c.tx.QueryRow(c.ctx, `SELECT actor_id FROM projection.actor
	        WHERE tenant_id = $1 AND on_behalf_of = $2 AND kind = 'automation' AND external_identity = $3`,
		c.p.TenantID, c.me, identity).Scan(&id)
	if !errors.Is(err, pgx.ErrNoRows) {
		return id, err
	}
	id = uuid.New()
	_, err = c.emit("actor.registered", "actor", id, nil, nil, map[string]any{"kind": "automation", "display_name": "Litopys",
		"on_behalf_of": c.me, "external_identity": identity})
	return id, err
}
