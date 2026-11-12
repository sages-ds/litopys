package verbs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/internal/pg"
)

// Retention labels: a project fact of kind retention says what goes and when; without one nothing
// happens (no defaults). `litopys worker` applies them once a day as the owner's automation, on behalf of the human
// who set the label; every action is an ordinary event with the reason "retention: <project>/<key>".

// Retention is the value of a retention fact (JSON).
type Retention struct {
	AppliesTo   string  `json:"applies_to"`             // record | a record type | task (closed tasks)
	AfterDays   *int    `json:"after_days,omitempty"`   // older than N days: a record by effective_at, a task by closed_at
	RetainUntil *string `json:"retain_until,omitempty"` // YYYY-MM-DD (Europe/Kyiv): from that day, what was there before it
	Action      string  `json:"action"`                 // retract | purge
}

// RetentionZone is the clock of retain_until and of the worker's daily pass.
const RetentionZone = "Europe/Kyiv"

// retentionIdentity: the owner's automation that applies retention labels.
const retentionIdentity = "litopys:retention"

// ParseRetention validates the value of a retention fact.
func ParseRetention(value *string) (Retention, error) {
	var r Retention
	if value == nil {
		return r, invalid("value", "required for kind=retention: {applies_to, after_days | retain_until, action}")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(*value)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, invalid("value", "retention is a JSON object {applies_to, after_days | retain_until, action}: %v", err)
	}
	if r.AppliesTo != "record" && r.AppliesTo != "task" && !slices.Contains(statementTypes, r.AppliesTo) {
		return r, invalid("value.applies_to", "record, task or a record type, not %q", r.AppliesTo)
	}
	if err := checkEnum("value.action", r.Action, "retract", "purge"); err != nil {
		return r, err
	}
	if (r.AfterDays == nil) == (r.RetainUntil == nil) {
		return r, invalid("value", "exactly one of after_days and retain_until")
	}
	if r.AfterDays != nil && (*r.AfterDays < 1 || *r.AfterDays > 36500) {
		return r, invalid("value.after_days", "1..36500")
	}
	if r.RetainUntil != nil {
		if _, err := time.Parse(time.DateOnly, *r.RetainUntil); err != nil {
			return r, invalid("value.retain_until", "a date YYYY-MM-DD")
		}
	}
	return r, nil
}

// cutoff: what is older than it goes; ok = false when the label does not act yet (retain_until in the future).
func (r Retention) cutoff(now time.Time, zone *time.Location) (time.Time, bool) {
	if r.AfterDays != nil {
		return now.AddDate(0, 0, -*r.AfterDays), true
	}
	until, _ := time.ParseInLocation(time.DateOnly, *r.RetainUntil, zone)
	return until, !now.Before(until)
}

// checkRetention: the check of fact.record for kind retention — a valid value; an erase, and a label over every record
// of the project (applies_to: record), only from a human: an agent labels a record type.
func (c *cmd) checkRetention(value *string) error {
	r, err := ParseRetention(value)
	if err != nil || !r.humanOnly() {
		return err
	}
	kind, err := c.actorKind()
	if err != nil {
		return err
	}
	switch {
	case kind == "human":
		return nil
	case r.Action == "purge":
		return fail("retention_purge_requires_human", "a label that erases is the user's decision; an %s may set only action retract — ask the user", kind)
	default:
		return fail("retention_record_requires_human", "a label over every record of the project is the user's decision; an %s may label a record type (note, decision, …) — ask the user", kind)
	}
}

// humanOnly: a label only a human sets — one that erases, or one over every record of the project.
func (r Retention) humanOnly() bool { return r.Action == "purge" || r.AppliesTo == "record" }

// executeLabels applies every live retention label of the tenant at now: each entity in its own transaction
// (retract — the app pool, purge — the admin pool), as the owner's automation on behalf of the label's author.
// A label whose author is no longer a live human-rooted actor, or a human-only label (humanOnly) not written by a human,
// is skipped.
// Returns how many entities were retracted or erased; the first error stops the pass (the next one retries).
func (s *Service) executeLabels(ctx context.Context, tenant uuid.UUID, now time.Time, zone *time.Location) (int, error) {
	rows, err := s.DB.Query(pg.WithScope(ctx, pg.ScopeSystem), `SELECT f.project_id, p.name, f.key, f.value, j.actor_id, a.kind,
	                 f.confirmed_at IS NOT NULL
	          FROM projection.fact f JOIN projection.project p ON p.project_id = f.project_id
	          JOIN journal j ON j.event_id = f.last_event_id JOIN projection.actor a ON a.actor_id = j.actor_id
	         WHERE f.tenant_id = $1 AND f.kind = 'retention' AND NOT f.retired
	         ORDER BY p.name, f.key`, tenant)
	if err != nil {
		return 0, err
	}
	type label struct {
		project    uuid.UUID
		name, key  string
		value      *string
		author     uuid.UUID
		authorKind string
		confirmed  bool
	}
	labels, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (label, error) {
		var l label
		return l, r.Scan(&l.project, &l.name, &l.key, &l.value, &l.author, &l.authorKind, &l.confirmed)
	})
	if err != nil {
		return 0, err
	}
	done := 0
	for _, l := range labels {
		r, err := ParseRetention(l.value)
		if err != nil || (r.humanOnly() && l.authorKind != "human") {
			continue // refused by fact.record; kept out of the pass all the same
		}
		if r.humanOnly() && !l.confirmed {
			continue // proposed: the person has not confirmed this version in the viewer yet
		}
		if r.Action == "purge" && s.Admin == nil {
			return done, fail("forbidden", "an erasing retention label needs the admin role")
		}
		cut, ok := r.cutoff(now, zone)
		if !ok {
			continue
		}
		targets, err := s.retentionTargets(ctx, l.project, r, cut)
		if err != nil {
			return done, err
		}
		reason := "retention: " + l.name + "/" + l.key
		for _, t := range targets {
			n, err := s.applyRetention(ctx, Principal{TenantID: tenant, ActorID: l.author}, r.Action, t, reason)
			if err != nil {
				return done, fmt.Errorf("retention %s/%s, %s %s: %w", l.name, l.key, t.EntityType, t.EntityID, err)
			}
			done += n
		}
	}
	return done, nil
}

// ExecuteRetention applies the retention labels, then the retentions of single records — the second also when the
// first stopped on an error; the errors are joined.
func (s *Service) ExecuteRetention(ctx context.Context, tenant uuid.UUID, now time.Time) (int, error) {
	zone, err := time.LoadLocation(RetentionZone)
	if err != nil {
		return 0, err
	}
	done, labelErr := s.executeLabels(ctx, tenant, now, zone)
	n, recordErr := s.executeRecordRetention(ctx, tenant, now, zone)
	return done + n, errors.Join(labelErr, recordErr)
}

// retentionTargets: what a label covers in its project, oldest first (at most 500 a pass; the rest the next day).
func (s *Service) retentionTargets(ctx context.Context, project uuid.UUID, r Retention, cut time.Time) ([]EntityRef, error) {
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	var rows pgx.Rows
	var err error
	if r.AppliesTo == "task" {
		rows, err = s.DB.Query(sys, `SELECT 'task', task_id FROM projection.task
		         WHERE project_id = $1 AND NOT open AND closed_at < $2 AND NOT purged AND ($3 = 'purge' OR NOT hidden)
		         ORDER BY closed_at LIMIT 500`, project, cut, r.Action)
	} else {
		rows, err = s.DB.Query(sys, `SELECT 'statement', statement_id FROM projection.statement
		         WHERE project_id = $1 AND ($2 = 'record' OR type = $2) AND effective_at < $3 AND NOT purged
		           AND ($4 = 'purge' OR (status <> 'retracted' AND NOT hidden))
		         ORDER BY effective_at LIMIT 500`, project, r.AppliesTo, cut, r.Action)
	}
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (EntityRef, error) {
		var e EntityRef
		return e, row.Scan(&e.EntityType, &e.EntityID)
	})
}

// applyRetention retracts or erases one target; 0 when it changed meanwhile and there is nothing left to do.
func (s *Service) applyRetention(ctx context.Context, author Principal, action string, t EntityRef, reason string) (int, error) {
	db := s.DB
	if action == "purge" {
		db = s.Admin
	}
	n := 0
	err := s.exec(ctx, db, author, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		purged, _, err := c.quarantineState(t) // locks the row
		if err != nil || purged {
			return err
		}
		var retracted bool
		if t.EntityType == "task" {
			err = c.tx.QueryRow(c.ctx, `SELECT retracted FROM projection.task WHERE task_id = $1`, t.EntityID).Scan(&retracted)
		} else {
			err = c.tx.QueryRow(c.ctx, `SELECT status = 'retracted' FROM projection.statement WHERE statement_id = $1`, t.EntityID).Scan(&retracted)
		}
		if err != nil || (action == "retract" && retracted) {
			return err
		}
		live := false
		if t.EntityType == "task" {
			if live, err = c.taskLive(t.EntityID); err != nil {
				return err
			}
		}
		auto, err := c.automation(retentionIdentity)
		if err != nil {
			return err
		}
		c.p.ActorID, c.run, c.cause = auto, auto, uuid.Nil
		state := "retracted"
		if action == "retract" {
			if _, err := c.emit(t.EntityType+".retracted", t.EntityType, t.EntityID, nil, nil, map[string]any{"reason": reason}); err != nil {
				return err
			}
		} else {
			state = "purged"
			var seq int64
			if err := c.tx.QueryRow(c.ctx, `SELECT journal_purge($1,$2,$3,$4,NULL,$5,$5,$5,$5)`,
				c.p.TenantID, t.EntityType, t.EntityID, reason, auto).Scan(&seq); err != nil {
				return err
			}
			var a Ack
			if err := c.projectPurge(seq, &a); err != nil {
				return err
			}
		}
		n = 1
		if !live {
			return nil
		}
		return c.askDependents(t.EntityID, state)
	})
	return n, err
}
