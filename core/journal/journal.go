// Package journal is the single write path into the append-only journal.
package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Event is the envelope of domain/envelope.yaml (envelope_version 1).
// Seq and RecordedAt are assigned by the server inside journal_append and never by the client.
type Event struct {
	Seq           int64
	TenantID      uuid.UUID
	EventID       uuid.UUID
	SchemaVersion int16
	Type          string
	RecordedAt    time.Time
	EffectiveAt   *time.Time // nil = recorded_at
	ActorID       uuid.UUID
	RunID         uuid.UUID
	CausationID   uuid.UUID
	CorrelationID uuid.UUID
	EntityType    string
	EntityID      uuid.UUID
	ContextID     *uuid.UUID
	Payload       json.RawMessage
}

// Append writes e through journal_append in tx and fills Seq and RecordedAt.
// The caller runs the projector in the same tx before committing.
func Append(ctx context.Context, tx pgx.Tx, e *Event) error {
	if e.EventID == uuid.Nil {
		e.EventID = uuid.New()
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = 1
	}
	err := tx.QueryRow(ctx,
		`SELECT seq, recorded_at FROM journal_append($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		e.TenantID, e.EventID, e.SchemaVersion, e.Type, e.EffectiveAt,
		e.ActorID, e.RunID, e.CausationID, e.CorrelationID,
		e.EntityType, e.EntityID, e.ContextID, []byte(e.Payload),
	).Scan(&e.Seq, &e.RecordedAt)
	if err != nil {
		return fmt.Errorf("journal_append %s: %w", e.Type, err)
	}
	return nil
}
