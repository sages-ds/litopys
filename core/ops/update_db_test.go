package ops

import (
	"context"
	"testing"
	"time"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestGateOpsUpdateDatabase: the rehearsal upgrades a copy and leaves the live database as it was
// (here: not migrated at all); the upgrade then migrates, builds the projections and verifies the live one; the
// read-only mode and the drain go through the same DSN.
func TestGateOpsUpdateDatabase(t *testing.T) {
	ctx := context.Background()
	db := pgtest.New(t)
	// a database one migration behind the code: the update has something to do
	mig := db.MigratorConn(t)
	if _, err := migrate.Up(ctx, mig); err != nil {
		t.Fatal(err)
	}
	// undo 0042 by hand (a table the update recreates); the update has one migration to apply
	if _, err := mig.Exec(ctx, `DELETE FROM schema_migrations WHERE version = 42;
		DROP TABLE ops.maintenance`); err != nil {
		t.Fatal(err)
	}
	mig.Close(ctx)
	count := func() int {
		var n int
		c := db.SuConn(t, db.Name)
		defer c.Close(ctx)
		if err := c.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	var logs []string
	d := LiveDatabase{Operator: db.SuDSN(), Migrator: db.MigratorDSN(), AppRole: "litopys_app",
		Log: func(f string, a ...any) { logs = append(logs, f) }}
	if err := d.Rehearse(ctx); err != nil {
		t.Fatalf("rehearsal: %v (%v)", err, logs)
	}
	if got := count(); got != before {
		t.Fatalf("the rehearsal changed the live database: %d -> %d migrations", before, got)
	}
	if err := d.Maintenance(ctx, true, "update test"); err != nil { // before 0042: skipped, not an error
		t.Fatal(err)
	}
	if err := d.Drain(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.Upgrade(ctx); err != nil {
		t.Fatalf("upgrade: %v (%v)", err, logs)
	}
	if got := count(); got != before+1 {
		t.Fatalf("the upgrade applied %d migrations, want 1", got-before)
	}
	// what a rollback reads: the migration head and the token changes since a moment
	last := 0
	for v := range embedded() {
		last = max(last, v)
	}
	if head, err := d.MigrationHead(ctx); err != nil || head != last {
		t.Fatalf("migration head %d %v, want the newest embedded %d", head, err, last)
	}
	if n, err := d.AuthSince(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("token changes %d %v on a fresh database", n, err)
	}
	if n, err := d.EventsSince(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("events %d %v on a fresh database", n, err)
	}
	// after the upgrade the mode exists: on, then off
	if err := d.Maintenance(ctx, true, "update test"); err != nil {
		t.Fatal(err)
	}
	if err := d.Maintenance(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	c := db.SuConn(t, db.Name)
	if m, err := GetMaintenance(pg.WithScope(ctx, pg.ScopeSystem), c); err != nil || m.ReadOnly {
		t.Fatalf("maintenance after: %+v %v", m, err)
	}
}
