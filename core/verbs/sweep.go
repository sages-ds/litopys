package verbs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/internal/pg"
)

// sessionsIdentity: the owner's automation that closes idle sessions (owner decision 2026-10-30).
const sessionsIdentity = "litopys:sessions"

// SessionIdle is how long a session may stay without events before the worker closes it (a day).
const SessionIdle = 24 * time.Hour

// sweepBatch bounds one pass; the rest waits for the next one.
const sweepBatch = 500

// AutoClosedSummary is the summary of an auto-closed session that had none of its own.
const AutoClosedSummary = "auto-closed: no activity for a day"

// SweepIdleSessions closes the open sessions whose last event is older than idle (context.closed with auto: true,
// written by the owner's automation). A summary from a checkpoint is kept; a session without one gets
// AutoClosedSummary. The run finishes with outcome timeout when no other session of it is open. Each session is its
// own transaction and is re-checked under a row lock, so a session that woke up meanwhile stays open.
func (s *Service) SweepIdleSessions(ctx context.Context, tenant uuid.UUID, now time.Time, idle time.Duration) (int, error) {
	cut := now.Add(-idle)
	rows, err := s.DB.Query(pg.WithScope(ctx, pg.ScopeSystem), `SELECT context_id, actor_id FROM projection.context
	         WHERE tenant_id = $1 AND closed_at IS NULL AND last_event_at < $2
	         ORDER BY last_event_at LIMIT $3`, tenant, cut, sweepBatch)
	if err != nil {
		return 0, err
	}
	type idleSession struct{ id, actor uuid.UUID }
	sessions, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (idleSession, error) {
		var x idleSession
		return x, r.Scan(&x.id, &x.actor)
	})
	if err != nil {
		return 0, err
	}
	done := 0
	for _, x := range sessions {
		closed := false
		err := s.exec(ctx, s.DB, Principal{TenantID: tenant, ActorID: x.actor}, func(c *cmd) error {
			_, run, closedAt, err := c.context(x.id)
			if err != nil || closedAt != nil {
				return err
			}
			var last time.Time
			var summary *string
			if err := c.tx.QueryRow(c.ctx, `SELECT last_event_at, summary FROM projection.context WHERE context_id = $1 FOR UPDATE`,
				x.id).Scan(&last, &summary); err != nil {
				return err
			}
			if !last.Before(cut) {
				return nil // woke up since the scan
			}
			auto, err := c.automation(sessionsIdentity)
			if err != nil {
				return err
			}
			c.p.ActorID, c.run, c.cause = auto, run, uuid.Nil
			payload := map[string]any{"auto": true}
			if summary == nil {
				payload["summary"] = AutoClosedSummary
			}
			if _, err := c.emit("context.closed", "context", x.id, nil, nil, payload); err != nil {
				return err
			}
			closed = true
			var others bool
			var finished *time.Time
			if err := c.tx.QueryRow(c.ctx, `SELECT EXISTS (SELECT 1 FROM projection.context WHERE run_id = $1 AND closed_at IS NULL),
			        (SELECT finished_at FROM projection.run WHERE run_id = $1)`, run).Scan(&others, &finished); err != nil {
				return err
			}
			if !others && finished == nil {
				_, err = c.emit("run.finished", "run", run, nil, nil, map[string]any{"outcome": "timeout"})
			}
			return err
		})
		if err != nil {
			return done, fmt.Errorf("sweep session %s: %w", x.id, err)
		}
		if closed {
			done++
		}
	}
	return done, nil
}
