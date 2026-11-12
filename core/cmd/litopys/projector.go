package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/projector"
	"litopys.smart-gw.co.ua/internal/pg"
)

// runProjector: `litopys projector rebuild` — truncate the derived tables and replay the journal.
// Needed after a projection migration or a projector version change.
// `litopys projector status` prints the stored and the code version ("stored=<v> code=<v> stale=<bool>"),
// so a deploy can rebuild before the new image serves (scripts/deploy-facade.ps1).
func runProjector(ctx context.Context, args []string) error {
	if len(args) != 1 || (args[0] != "rebuild" && args[0] != "status") {
		return fmt.Errorf("projector: usage: litopys projector rebuild | status")
	}
	db, err := pg.Pool(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	var tenant uuid.UUID
	if err := db.QueryRow(ctx, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		return err
	}
	if args[0] == "status" {
		var stored *string
		err := db.QueryRow(ctx, `SELECT version FROM projection.projector_state WHERE tenant_id = $1`, tenant).Scan(&stored)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		s := "-"
		if stored != nil {
			s = *stored
		}
		fmt.Printf("stored=%s code=%s stale=%v\n", s, projector.Version, s != projector.Version)
		return nil
	}
	n, err := projector.Rebuild(ctx, db, tenant)
	fmt.Printf("replayed %d events with projector version %s\n", n, projector.Version)
	return err
}
