package projector

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/journal"
)

// Human numbers of entities (0030). The command issues the number and puts it into the creation event;
// the projector keeps it. Events written before 0030 carry none: a record takes its facade number (facade.legacy_id,
// outside the journal, so replay gives the numbers people already saw), a workflow or an execution the next one in
// its project — replay applies events in seq order, so the result is the same on every rebuild. A wanted number
// that is taken (an import after native writes) falls through to the next candidate; litopys verify reports it.

// recordNum: the number of statement id of the project's owner.
func recordNum(ctx context.Context, tx pgx.Tx, id, project uuid.UUID, want *int64) (int64, error) {
	var owner uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT owner_id FROM projection.project WHERE project_id = $1`, project).Scan(&owner); err != nil {
		return 0, err
	}
	candidates := []*int64{want}
	var legacy int64
	err := tx.QueryRow(ctx, `SELECT legacy_id FROM facade.legacy_id WHERE statement_id = $1`, id).Scan(&legacy)
	if err == nil {
		candidates = append(candidates, &legacy)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	for _, n := range candidates {
		if n == nil || *n < 1 {
			continue
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projection.statement WHERE owner_id = $1 AND num = $2)`,
			owner, *n).Scan(&taken); err != nil {
			return 0, err
		}
		if !taken {
			return *n, nil
		}
	}
	var n int64
	err = tx.QueryRow(ctx, `SELECT GREATEST(COALESCE(max(num), 0),
	        COALESCE((SELECT floor FROM projection.num_floor WHERE scope_id = $1 AND kind = 'R'), 0)) + 1
	          FROM projection.statement WHERE owner_id = $1`, owner).Scan(&n)
	return n, err
}

// projectNum: the number of a workflow, an execution or a deletion proposal in its project (table: workflow |
// execution | deletion).
func projectNum(ctx context.Context, tx pgx.Tx, table string, project uuid.UUID, want *int) (int, error) {
	if want != nil && *want >= 1 {
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projection.`+table+` WHERE project_id = $1 AND num = $2)`,
			project, *want).Scan(&taken); err != nil {
			return 0, err
		}
		if !taken {
			return *want, nil
		}
	}
	var n int
	err := tx.QueryRow(ctx, `SELECT COALESCE(max(num), 0) + 1 FROM projection.`+table+` WHERE project_id = $1`, project).Scan(&n)
	return n, err
}

// numberingReserved (0037): the floor of R numbers of the owner (the event's actor entity) or of T numbers of a
// project of that owner. A floor only rises: a lower one (a re-run against an older source) changes nothing.
func numberingReserved(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Kind      string     `json:"kind"`
		Floor     int64      `json:"floor"`
		ProjectID *uuid.UUID `json:"project_id"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	if p.Floor < 1 {
		return fmt.Errorf("numbering.reserved: floor %d", p.Floor)
	}
	scope := e.EntityID
	switch {
	case p.Kind == "R" && p.ProjectID == nil:
	case p.Kind == "T" && p.ProjectID != nil:
		var owner uuid.UUID
		err := tx.QueryRow(ctx, `SELECT owner_id FROM projection.project WHERE project_id = $1`, *p.ProjectID).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: project %s", ErrMissing, *p.ProjectID)
		}
		if err != nil {
			return err
		}
		if owner != e.EntityID {
			return fmt.Errorf("numbering.reserved: project %s is not of owner %s", *p.ProjectID, e.EntityID)
		}
		scope = *p.ProjectID
	default:
		return fmt.Errorf("numbering.reserved: kind %q with project_id %v", p.Kind, p.ProjectID)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO projection.num_floor (tenant_id, owner_id, scope_id, kind, floor, last_seq, last_event_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (scope_id, kind) DO UPDATE
		   SET floor = GREATEST(projection.num_floor.floor, EXCLUDED.floor), last_seq = EXCLUDED.last_seq,
		       last_event_id = EXCLUDED.last_event_id`,
		e.TenantID, e.EntityID, scope, p.Kind, p.Floor, e.Seq, e.EventID)
	return err
}
