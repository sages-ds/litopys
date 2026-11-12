package projector

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/journal"
)

// Executions, step attempts, approvals (0021). Step states are derived by core/flow, not stored.

func executionStarted(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		WorkflowID        uuid.UUID       `json:"workflow_id"`
		Version           int             `json:"version"`
		ProjectID         uuid.UUID       `json:"project_id"`
		Params            json.RawMessage `json:"params"`
		TaskID            *uuid.UUID      `json:"task_id"`
		ParentExecutionID *uuid.UUID      `json:"parent_execution_id"`
		ParentStepKey     *string         `json:"parent_step_key"`
		Num               *int            `json:"num"` // 0030: X-<num> within the project
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	var params any
	if len(p.Params) > 0 {
		params = p.Params
	}
	num, err := projectNum(ctx, tx, "execution", p.ProjectID, p.Num)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO projection.execution (execution_id, tenant_id, owner_id, project_id, workflow_id, version, params, task_id,
		       parent_execution_id, parent_step_key, started_at, started_seq, last_seq, last_event_id, num)
		VALUES ($1, $2, (SELECT owner_id FROM projection.project WHERE project_id = $3), $3, $4, $5, $6, $7, $8, $9, $10, $11, $11, $12, $13)`,
		e.EntityID, e.TenantID, p.ProjectID, p.WorkflowID, p.Version, params, p.TaskID, p.ParentExecutionID, p.ParentStepKey,
		effective(e), e.Seq, e.EventID, num); err != nil {
		return err
	}
	return touchProject(ctx, tx, p.ProjectID, e)
}

func executionClosed(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Outcome string  `json:"outcome"`
		Summary *string `json:"summary"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	var projectID uuid.UUID
	if err := tx.QueryRow(ctx, `
		UPDATE projection.execution SET state = $2, summary = $3, closed_at = $4, revision = revision + 1, last_seq = $5, last_event_id = $6
		 WHERE execution_id = $1 RETURNING project_id`, e.EntityID, p.Outcome, p.Summary, effective(e), e.Seq, e.EventID).Scan(&projectID); err != nil {
		return fmt.Errorf("%w: execution %s: %v", ErrMissing, e.EntityID, err)
	}
	return touchProject(ctx, tx, projectID, e)
}

// touchExecution moves the execution's last_seq (a step or an approval changed it).
func touchExecution(ctx context.Context, tx pgx.Tx, executionID uuid.UUID, e *journal.Event) error {
	return exec1(ctx, tx, "execution "+executionID.String(),
		`UPDATE projection.execution SET revision = revision + 1, last_seq = $2, last_event_id = $3 WHERE execution_id = $1`,
		executionID, e.Seq, e.EventID)
}

func stepStarted(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		ExecutionID uuid.UUID  `json:"execution_id"`
		StepKey     string     `json:"step_key"`
		Attempt     int        `json:"attempt"`
		LeaseUntil  time.Time  `json:"lease_until"`
		ApprovalID  *uuid.UUID `json:"approval_id"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO projection.step_attempt (attempt_id, tenant_id, owner_id, execution_id, step_key, n, actor_id, run_id, lease_until, approval_id, started_at, last_seq)
		VALUES ($1, $2, (SELECT owner_id FROM projection.execution WHERE execution_id = $3), $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		e.EntityID, e.TenantID, p.ExecutionID, p.StepKey, p.Attempt, e.ActorID, e.RunID, p.LeaseUntil, p.ApprovalID, effective(e), e.Seq); err != nil {
		return err
	}
	if p.ApprovalID != nil {
		if err := exec1(ctx, tx, "approval "+p.ApprovalID.String(),
			`UPDATE projection.approval SET used_by = $2, last_seq = $3 WHERE approval_id = $1`, *p.ApprovalID, e.EntityID, e.Seq); err != nil {
			return err
		}
	}
	return touchExecution(ctx, tx, p.ExecutionID, e)
}

func attemptExecution(ctx context.Context, tx pgx.Tx, e *journal.Event, set string, args ...any) error {
	var executionID uuid.UUID
	all := append([]any{e.EntityID, e.Seq}, args...)
	if err := tx.QueryRow(ctx, `UPDATE projection.step_attempt SET `+set+`, last_seq = $2 WHERE attempt_id = $1 RETURNING execution_id`,
		all...).Scan(&executionID); err != nil {
		return fmt.Errorf("%w: attempt %s: %v", ErrMissing, e.EntityID, err)
	}
	return touchExecution(ctx, tx, executionID, e)
}

func stepCheckpointed(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		LeaseUntil time.Time       `json:"lease_until"`
		Output     json.RawMessage `json:"output"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	if len(p.Output) > 0 {
		return attemptExecution(ctx, tx, e, "lease_until = $3, output = $4", p.LeaseUntil, p.Output)
	}
	return attemptExecution(ctx, tx, e, "lease_until = $3", p.LeaseUntil)
}

func stepFinished(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		State  string          `json:"state"`
		Output json.RawMessage `json:"output"`
		Error  *string         `json:"error"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	var out any
	if len(p.Output) > 0 {
		out = p.Output
	}
	return attemptExecution(ctx, tx, e, "state = $3, output = COALESCE($4, output), error = $5, finished_at = $6",
		p.State, out, p.Error, effective(e))
}

func approvalGranted(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		ExecutionID uuid.UUID       `json:"execution_id"`
		StepKey     string          `json:"step_key"`
		Items       json.RawMessage `json:"items"`
		Note        *string         `json:"note"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	var items any
	if len(p.Items) > 0 {
		items = p.Items
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO projection.approval (approval_id, tenant_id, owner_id, execution_id, step_key, actor_id, items, note, granted_at, last_seq)
		VALUES ($1, $2, (SELECT owner_id FROM projection.execution WHERE execution_id = $3), $3, $4, $5, $6, $7, $8, $9)`,
		e.EntityID, e.TenantID, p.ExecutionID, p.StepKey, e.ActorID, items, p.Note, effective(e), e.Seq); err != nil {
		return err
	}
	return touchExecution(ctx, tx, p.ExecutionID, e)
}

func approvalRevoked(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var executionID uuid.UUID
	if err := tx.QueryRow(ctx, `UPDATE projection.approval SET revoked = true, last_seq = $2 WHERE approval_id = $1 RETURNING execution_id`,
		e.EntityID, e.Seq).Scan(&executionID); err != nil {
		return fmt.Errorf("%w: approval %s: %v", ErrMissing, e.EntityID, err)
	}
	return touchExecution(ctx, tx, executionID, e)
}

// stepDelegated: the delegation itself is edge data (auth.token); the run records that it changed.
func stepDelegated(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	return touchExecution(ctx, tx, e.EntityID, e)
}
