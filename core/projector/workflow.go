package projector

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/journal"
)

// Workflows (0020): projection.workflow is the identity and metadata, projection.workflow_version one
// frozen definition per version.

type workflowDefinition struct {
	Steps []json.RawMessage `json:"steps"`
}

func stepsOf(def json.RawMessage) (int, error) {
	var d workflowDefinition
	if err := json.Unmarshal(def, &d); err != nil {
		return 0, err
	}
	return len(d.Steps), nil
}

func workflowVersion(ctx context.Context, tx pgx.Tx, e *journal.Event, version int, def json.RawMessage, reason *string) error {
	steps, err := stepsOf(def)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO projection.workflow_version (workflow_id, version, tenant_id, owner_id, definition, steps, reason, created_at, created_seq, event_id)
		VALUES ($1, $2, $3, (SELECT owner_id FROM projection.workflow WHERE workflow_id = $1), $4, $5, $6, $7, $8, $9)`,
		e.EntityID, version, e.TenantID, def, steps, reason, effective(e), e.Seq, e.EventID)
	return err
}

func workflowDeclared(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		ProjectID  uuid.UUID       `json:"project_id"`
		Key        string          `json:"key"`
		Title      string          `json:"title"`
		Purpose    *string         `json:"purpose"`
		Definition json.RawMessage `json:"definition"`
		Num        *int            `json:"num"` // 0030: W-<num> within the project
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	num, err := projectNum(ctx, tx, "workflow", p.ProjectID, p.Num)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO projection.workflow (workflow_id, tenant_id, owner_id, project_id, key, title, purpose, declared_seq, last_seq, last_event_id, num)
		VALUES ($1, $2, (SELECT owner_id FROM projection.project WHERE project_id = $3), $3, $4, $5, $6, $7, $7, $8, $9)`,
		e.EntityID, e.TenantID, p.ProjectID, p.Key, p.Title, p.Purpose, e.Seq, e.EventID, num); err != nil {
		return err
	}
	if err := workflowVersion(ctx, tx, e, 1, p.Definition, nil); err != nil {
		return err
	}
	return touchProject(ctx, tx, p.ProjectID, e)
}

// workflowUpdate bumps revision and returns the project for touchProject; set uses $4 onwards.
func workflowUpdate(ctx context.Context, tx pgx.Tx, e *journal.Event, set string, args ...any) error {
	var projectID uuid.UUID
	all := append([]any{e.EntityID, e.Seq, e.EventID}, args...)
	if err := tx.QueryRow(ctx, `
		UPDATE projection.workflow SET `+set+`, revision = revision + 1, last_seq = $2, last_event_id = $3
		 WHERE workflow_id = $1 RETURNING project_id`, all...).Scan(&projectID); err != nil {
		return fmt.Errorf("%w: workflow %s: %v", ErrMissing, e.EntityID, err)
	}
	return touchProject(ctx, tx, projectID, e)
}

func workflowAmended(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p map[string]json.RawMessage
	if err := decode(e, &p); err != nil {
		return err
	}
	set, args := "workflow_id = workflow_id", []any{}
	if raw, ok := p["title"]; ok {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		args = append(args, v)
		set += ", title = $" + strconv.Itoa(3+len(args))
	}
	if raw, ok := p["purpose"]; ok {
		var v *string
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		args = append(args, v)
		set += ", purpose = $" + strconv.Itoa(3+len(args))
	}
	return workflowUpdate(ctx, tx, e, set, args...)
}

func workflowSuperseded(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Version    int             `json:"version"`
		Definition json.RawMessage `json:"definition"`
		Reason     *string         `json:"reason"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	if err := workflowUpdate(ctx, tx, e, "version = $4", p.Version); err != nil {
		return err
	}
	return workflowVersion(ctx, tx, e, p.Version, p.Definition, p.Reason)
}

func workflowRetired(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	return workflowUpdate(ctx, tx, e, "retired = true")
}
