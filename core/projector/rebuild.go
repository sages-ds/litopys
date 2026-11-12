package projector

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/journal"
	"litopys.smart-gw.co.ua/internal/pg"
)

// BatchSize of a rebuild transaction.
const BatchSize = 1000

// derived lists every table the projector feeds (search_vocab too since projector v2).
const derived = `projection.attachment, projection.num_floor, projection.deletion, projection.approval, projection.step_attempt, projection.execution, projection.workflow_version, projection.workflow, projection.search_vocab, projection.stem_vocab, projection.statement_search, projection.statement_lexical, projection.statement_version,
	projection.link, projection.statement, projection.task, projection.fact, projection.context,
	projection.run, projection.actor, projection.consumer, projection.policy, projection.project, projection.projector_state`

// DB is what Rebuild needs: transactions and reads (*pgx.Conn, *pgxpool.Pool).
type DB interface {
	pg.Beginner
	journal.Querier
}

// Rebuilding marks projector_state.version while a rebuild runs: it is never a code version, so an
// interrupted rebuild stays stale for `projector status`, verify and the metrics.
const Rebuilding = "rebuilding:" + Version

// Rebuild truncates the derived tables and replays the tenant's journal in seq order,
// one transaction per batch. The version becomes Version in the transaction of the
// last batch only; a failure on the way leaves Rebuilding.
//
// Slice 1 has a single tenant ("default"), so TRUNCATE is global; a multi-tenant rebuild
// must switch to DELETE ... WHERE tenant_id. Stopping writers during the rebuild is the
// caller's job (read-only API mode or paused worker).
func Rebuild(ctx context.Context, db DB, tenantID uuid.UUID) (int64, error) {
	err := pg.InTx(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `TRUNCATE `+derived); err != nil { // TRUNCATE is outside RLS (0008)
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO projection.projector_state (tenant_id, last_seq, rebuilt_at, version)
		                        VALUES ($1, 0, now(), $2)`, tenantID, Rebuilding)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("rebuild reset: %w", err)
	}
	var applied, last int64
	for {
		var n int
		err := pg.InTx(ctx, db, func(tx pgx.Tx) error {
			// replay reads every owner's events (RLS of 0008); Apply switches to the projector scope per event
			if _, err := tx.Exec(ctx, `SELECT set_config('litopys.scope', 'system', true)`); err != nil {
				return err
			}
			events, err := journal.Read(ctx, tx, tenantID, last, BatchSize)
			if err != nil {
				return err
			}
			n = len(events)
			for i := range events {
				if err := Apply(ctx, tx, &events[i]); err != nil {
					return err
				}
				last = events[i].Seq
			}
			if n < BatchSize { // the last batch: the projections now match this code
				_, err = tx.Exec(ctx, `UPDATE projection.projector_state SET version = $2 WHERE tenant_id = $1`, tenantID, Version)
			}
			return err
		})
		if err != nil {
			return applied, fmt.Errorf("rebuild after seq %d: %w", last, err)
		}
		applied += int64(n)
		if n < BatchSize {
			return applied, nil
		}
	}
}
