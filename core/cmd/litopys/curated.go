package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/importer"
	"litopys.smart-gw.co.ua/internal/pg"
)

// runCurated: `litopys migrate-from vault-curated-v1 --dir <vault> --owner <e-mail> [--dry-run] [--report <file>]`.
// The notes of <vault>/curated become documents of the owner; --dry-run runs the
// import in one transaction that is rolled back.
func runCurated(ctx context.Context, desc, dir, owner, mode, reportFile string) error {
	if dir == "" || owner == "" {
		return errors.New("migrate-from vault-curated-v1: --dir <vault> and --owner <e-mail> are required")
	}
	if mode == "verify" {
		return errors.New("migrate-from vault-curated-v1: --verify is not available (a repeated run reports unchanged notes)")
	}
	notes, skipped, err := importer.ParseCurated(dir)
	if err != nil {
		return err
	}
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db, err := pg.Pool(sys)
	if err != nil {
		return err
	}
	defer db.Close()
	var tenant uuid.UUID
	if err := db.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		return fmt.Errorf("target tenant: %w", err)
	}
	var rep importer.CuratedReport
	if mode == "dry-run" {
		tx, err := db.Begin(sys)
		if err != nil {
			return err
		}
		rep, err = importer.Curated(ctx, tx, tenant, owner, notes)
		if rerr := tx.Rollback(sys); err == nil {
			err = rerr
		}
		if err != nil {
			return err
		}
	} else if rep, err = importer.Curated(ctx, db, tenant, owner, notes); err != nil {
		return err
	}
	rep.Skipped = append(skipped, rep.Skipped...)
	out := map[string]any{"profile": "vault-curated-v1", "description": desc, "mode": mode, "started_at": time.Now().UTC(), "report": rep}
	b, _ := json.MarshalIndent(out, "", "  ")
	if reportFile != "" {
		if err := os.WriteFile(reportFile, append(b, '\n'), 0o600); err != nil {
			return err
		}
	}
	fmt.Println(string(b))
	return nil
}
