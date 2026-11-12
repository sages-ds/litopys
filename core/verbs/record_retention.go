package verbs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/internal/pg"
)

// The retention of one record (0043): retain_until — the day (Europe/Kyiv) from which
// on_expiry applies. retract: the worker retracts the record on behalf of whoever set it — so setting it is allowed to
// whoever may retract the record now (an agent only its own). purge: the worker opens a deletion proposal (D-N) and
// the erasure waits for a human's GO, as every erasure — anyone may schedule that. A retention a human
// set is lifted or changed only by a human.

// StatementRetainIn: retain_until "" lifts the retention (on_expiry is then not given).
type StatementRetainIn struct {
	StatementID uuid.UUID `json:"statement_id"`
	RetainUntil string    `json:"retain_until"`
	OnExpiry    *string   `json:"on_expiry,omitempty"`
	Reason      *string   `json:"reason,omitempty"`
}

// checkRecordRetention: a date YYYY-MM-DD not before today (Europe/Kyiv) and an action, both given.
func checkRecordRetention(until, action *string, now time.Time) error {
	if until == nil || action == nil {
		return invalid("retain_until", "retain_until and on_expiry go together")
	}
	if err := checkEnum("on_expiry", *action, "retract", "purge"); err != nil {
		return err
	}
	zone, err := time.LoadLocation(RetentionZone)
	if err != nil {
		return err
	}
	if _, err := time.ParseInLocation(time.DateOnly, *until, zone); err != nil {
		return invalid("retain_until", "a date YYYY-MM-DD")
	}
	if today := now.In(zone).Format(time.DateOnly); *until < today {
		return fail("retain_until_past", "retain_until %s is before today (%s, %s)", *until, today, RetentionZone)
	}
	return nil
}

// StatementRetain sets, replaces or lifts the retention of a record (event statement.retention_set; no new version).
func (s *Service) StatementRetain(ctx context.Context, p Principal, in StatementRetainIn) (Ack, error) {
	if err := optLen("reason", in.Reason, 500); err != nil {
		return Ack{}, err
	}
	lift := in.RetainUntil == ""
	if lift && in.OnExpiry != nil {
		return Ack{}, invalid("on_expiry", `not given when retain_until is "" (lifting the retention)`)
	}
	if !lift {
		if err := checkRecordRetention(&in.RetainUntil, in.OnExpiry, time.Now()); err != nil {
			return Ack{}, err
		}
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		status, _, err := c.statement(in.StatementID) // locks the row; not the principal's — not found
		if err != nil {
			return err
		}
		if status == "retracted" && !lift {
			return fail("statement_retracted", "statement %s is retracted", in.StatementID)
		}
		var until, action *string
		var purged bool
		if err := c.tx.QueryRow(c.ctx, `SELECT to_char(retain_until, 'YYYY-MM-DD'), on_expiry, purged FROM projection.statement WHERE statement_id = $1`,
			in.StatementID).Scan(&until, &action, &purged); err != nil {
			return err
		}
		if purged {
			return fail("already_purged", "statement %s is erased", in.StatementID)
		}
		if err := c.retentionGuard(in.StatementID); err != nil {
			return err
		}
		if !lift && *in.OnExpiry == "retract" {
			if err := c.retractOwn(in.StatementID); err != nil {
				return err
			}
		}
		if (lift && until == nil) || (!lift && until != nil && *until == in.RetainUntil && action != nil && *action == *in.OnExpiry) {
			a.Already = true
			return nil
		}
		payload := map[string]any{"retain_until": in.RetainUntil}
		if !lift {
			payload["on_expiry"] = *in.OnExpiry
		}
		if in.Reason != nil {
			payload["reason"] = *in.Reason
		}
		e, err := c.emit("statement.retention_set", "statement", in.StatementID, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

// retentionGuard: a human changes any retention of their records; anyone else only one they set themselves — a
// retention a human set stays as the human set it, and an agent does not re-date another's (each new setting may open
// a new purge proposal).
func (c *cmd) retentionGuard(id uuid.UUID) error {
	var setBy *uuid.UUID
	if err := c.tx.QueryRow(c.ctx, `SELECT retain_by FROM projection.statement WHERE statement_id = $1`, id).Scan(&setBy); err != nil {
		return err
	}
	if setBy == nil || *setBy == c.p.ActorID {
		return nil
	}
	kind, err := c.actorKind()
	if err != nil || kind == "human" {
		return err
	}
	var setKind string
	if err := c.tx.QueryRow(c.ctx, `SELECT kind FROM projection.actor WHERE actor_id = $1`, *setBy).Scan(&setKind); err != nil {
		return err
	}
	if setKind == "human" {
		return fail("retention_set_by_human", "a human set the retention of this record; only a human changes or lifts it")
	}
	return fail("retention_set_by_another", "another actor set the retention of this record; ask the user")
}

// executeRecordRetention: the records whose retain_until has come (today in Europe/Kyiv): retract — retracted on
// behalf of whoever set it; purge — a deletion proposal opened once per setting (a rejected one is not reopened).
// Part of the daily retention pass; at most 500 records a pass, earliest date first.
func (s *Service) executeRecordRetention(ctx context.Context, tenant uuid.UUID, now time.Time, zone *time.Location) (int, error) {
	today := now.In(zone).Format(time.DateOnly)
	rows, err := s.DB.Query(pg.WithScope(ctx, pg.ScopeSystem), `
		SELECT s.statement_id, 'R-' || s.num, s.on_expiry, s.retain_by, to_char(s.retain_until, 'YYYY-MM-DD')
		  FROM projection.statement s
		 WHERE s.tenant_id = $1 AND s.retain_until <= $2::date AND NOT s.purged
		   AND CASE s.on_expiry
		         WHEN 'retract' THEN s.status <> 'retracted' AND NOT s.hidden
		              -- used once: a record retracted after the setting and reinstated since stays
		              AND NOT EXISTS (SELECT 1 FROM journal j WHERE j.tenant_id = s.tenant_id AND j.entity_type = 'statement'
		                                 AND j.entity_id = s.statement_id AND j.type = 'statement.retracted' AND j.seq > s.retain_seq)
		         ELSE NOT EXISTS (SELECT 1 FROM projection.deletion d
		                           WHERE d.target_id = s.statement_id AND d.action = 'purge' AND d.proposed_seq > s.retain_seq)
		       END
		 ORDER BY s.retain_until, s.num LIMIT 500`, tenant, today)
	if err != nil {
		return 0, err
	}
	type due struct {
		id          uuid.UUID
		ref, action string
		setBy       uuid.UUID
		until       string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var d due
		return d, r.Scan(&d.id, &d.ref, &d.action, &d.setBy, &d.until)
	})
	if err != nil {
		return 0, err
	}
	done := 0
	var skipped []error
	for _, d := range list {
		reason := "retention: " + d.ref + " retain_until " + d.until
		target := EntityRef{EntityType: "statement", EntityID: d.id}
		author := Principal{TenantID: tenant, ActorID: d.setBy}
		if d.action == "retract" {
			n, err := s.applyRetention(ctx, author, "retract", target, reason)
			if c := Code(err); c != "" && c != "maintenance" { // read-only stops the pass, the worker holds it
				skipped = append(skipped, fmt.Errorf("retention of %s skipped: %w", d.ref, err))
				continue
			}
			if err != nil {
				return done, fmt.Errorf("retention of %s: %w", d.ref, err)
			}
			done += n
			continue
		}
		opened := false
		err := s.exec(ctx, s.DB, author, func(c *cmd) error {
			if err := c.requireLiveActor(); err != nil {
				return err
			}
			auto, err := c.automation(retentionIdentity)
			if err != nil {
				return err
			}
			c.p.ActorID, c.run, c.cause = auto, auto, uuid.Nil
			out, err := c.propose(target, "purge", reason, false)
			opened = err == nil && !out.Already // an older open proposal for the same purge answers already
			return err
		})
		if Code(err) == "already_purged" {
			continue
		}
		if c := Code(err); c != "" && c != "maintenance" { // read-only stops the pass, the worker holds it
			skipped = append(skipped, fmt.Errorf("retention of %s skipped: %w", d.ref, err))
			continue
		}
		if err != nil {
			return done, fmt.Errorf("retention of %s: %w", d.ref, err)
		}
		if opened {
			done++
		}
	}
	// the other records went on; the skipped ones are reported (the worker logs them and retries an hour later)
	return done, errors.Join(skipped...)
}
