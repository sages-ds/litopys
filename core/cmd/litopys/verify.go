package main

import (
	"context"
	"fmt"

	"litopys.smart-gw.co.ua/internal/pg"

	"litopys.smart-gw.co.ua/core/projector"
)

// runVerify: `litopys verify` — integrity of a database (a live one, or a restore of a backup):
// the journal is gapless (count = max(seq) = head per tenant), the projector applied everything with the
// current code version, and every statement has its asserted event. Exit 1 on any finding (the
// verify step of backup + verify-restore). The checks live in the database (ops.verify_report, 0018),
// so the scheduled backup runs the same ones through psql. Read-only; run as the migrator or with the
// system scope.
func runVerify(ctx context.Context, _ []string) error {
	db, err := pg.Pool(pg.WithScope(ctx, pg.ScopeSystem))
	if err != nil {
		return err
	}
	defer db.Close()
	ctx = pg.WithScope(ctx, pg.ScopeSystem)
	rows, err := db.Query(ctx, `SELECT tenant, journal_head, journal_rows, journal_max, projector_seq, projector_version,
		orphan_statements, findings FROM ops.verify_report($1)`, projector.Version)
	if err != nil {
		return err
	}
	defer rows.Close()
	findings := 0
	for rows.Next() {
		var name, version string
		var head, count, max, applied, orphans int64
		var found []string
		if err := rows.Scan(&name, &head, &count, &max, &applied, &version, &orphans, &found); err != nil {
			return err
		}
		fmt.Printf("tenant %s: journal head %d, rows %d, max %d; projector %d (v%s); orphan statements %d\n",
			name, head, count, max, applied, version, orphans)
		for _, f := range found {
			findings++
			fmt.Println("  FINDING:", f)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if findings > 0 {
		return fmt.Errorf("verify: %d finding(s)", findings)
	}
	fmt.Println("verify: OK")
	return nil
}
