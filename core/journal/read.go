package journal

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Querier is satisfied by *pgx.Conn, *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Read returns up to limit events of the tenant with seq > afterSeq, ordered by seq (invariant 7).
// It is the replay source of the projector and the shape of journal.changes.
func Read(ctx context.Context, q Querier, tenantID uuid.UUID, afterSeq int64, limit int) ([]Event, error) {
	rows, err := q.Query(ctx, `
		SELECT seq, tenant_id, event_id, schema_version, type, recorded_at, effective_at,
		       actor_id, run_id, causation_id, correlation_id, entity_type, entity_id, context_id, payload
		  FROM journal
		 WHERE tenant_id = $1 AND seq > $2
		 ORDER BY seq
		 LIMIT $3`, tenantID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("journal read: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		var payload []byte
		err := r.Scan(&e.Seq, &e.TenantID, &e.EventID, &e.SchemaVersion, &e.Type, &e.RecordedAt, &e.EffectiveAt,
			&e.ActorID, &e.RunID, &e.CausationID, &e.CorrelationID, &e.EntityType, &e.EntityID, &e.ContextID, &payload)
		e.Payload = payload
		return e, err
	})
}
