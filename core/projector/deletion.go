package projector

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/journal"
)

// Deletion proposals D-N (0035): a proposal is a row of its own; the action it approves is an ordinary
// event of the target (statement.retracted, task.retracted, journal.purged), so this file only keeps the proposal.

func deletionProposed(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		ProjectID uuid.UUID       `json:"project_id"`
		Num       *int            `json:"num"`
		Target    entityRef       `json:"target"`
		Action    string          `json:"action"`
		Reason    string          `json:"reason"`
		Secret    bool            `json:"secret"`
		Impact    json.RawMessage `json:"impact"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	num, err := projectNum(ctx, tx, "deletion", p.ProjectID, p.Num)
	if err != nil {
		return err
	}
	impact := string(p.Impact)
	if impact == "" || impact == "null" {
		impact = "{}"
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO projection.deletion (deletion_id, tenant_id, owner_id, project_id, num, target_type, target_id, action,
		       reason, secret, impact, proposed_by, proposed_at, proposed_seq, last_seq, last_event_id)
		VALUES ($1, $2, (SELECT owner_id FROM projection.project WHERE project_id = $3), $3, $4, $5, $6, $7, $8, $9, $10::jsonb,
		        $11, $12, $13, $13, $14)`,
		e.EntityID, e.TenantID, p.ProjectID, num, p.Target.EntityType, p.Target.EntityID, p.Action, p.Reason, p.Secret,
		impact, e.ActorID, effective(e), e.Seq, e.EventID)
	return err
}

// deletionApproved: the GO. A retract is done by the same command (state done); an erase waits for the worker.
func deletionApproved(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Note *string `json:"note"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	return exec1(ctx, tx, "deletion "+e.EntityID.String(), `
		UPDATE projection.deletion
		   SET state = CASE WHEN action = 'retract' THEN 'done' ELSE 'approved' END, decided_by = $2, decided_at = $3,
		       note = $4, last_seq = $5, last_event_id = $6
		 WHERE deletion_id = $1`, e.EntityID, e.ActorID, effective(e), p.Note, e.Seq, e.EventID)
}

func deletionRejected(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Reason string `json:"reason"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	return exec1(ctx, tx, "deletion "+e.EntityID.String(), `
		UPDATE projection.deletion SET state = 'rejected', decided_by = $2, decided_at = $3, note = $4, last_seq = $5, last_event_id = $6
		 WHERE deletion_id = $1`, e.EntityID, e.ActorID, effective(e), p.Reason, e.Seq, e.EventID)
}

// deletionExecuted: the worker erased the target (journal.purged precedes it in the same transaction).
func deletionExecuted(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	return exec1(ctx, tx, "deletion "+e.EntityID.String(), `
		UPDATE projection.deletion SET state = 'done', last_seq = $2, last_event_id = $3 WHERE deletion_id = $1`,
		e.EntityID, e.Seq, e.EventID)
}
