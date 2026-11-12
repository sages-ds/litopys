package ops

import (
	"context"
	"testing"
	"time"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestGateOpsMaintenance (0042): the operator switches read-only on and off, and the drain waits for a command
// that started before the switch — it times out while one holds the lock and returns once it commits.
func TestGateOpsMaintenance(t *testing.T) {
	ctx := pg.WithScope(context.Background(), pg.ScopeSystem)
	db := pgtest.New(t)
	op := db.MigratorConn(t)
	if _, err := migrate.Up(ctx, op); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if err := SetMaintenance(ctx, op, true, "update to v1"); err != nil {
		t.Fatal(err)
	}
	m, err := GetMaintenance(ctx, op)
	if err != nil || !m.ReadOnly || m.Reason == nil || *m.Reason != "update to v1" || m.Since == nil {
		t.Fatalf("on: %+v %v", m, err)
	}
	since := *m.Since
	if err := SetMaintenance(ctx, op, true, "update to v1"); err != nil { // idempotent: keeps since
		t.Fatal(err)
	}
	if m, _ := GetMaintenance(ctx, op); !m.Since.Equal(since) {
		t.Errorf("a repeated on moved since: %v -> %v", since, m.Since)
	}

	// a command of another session that started before the drain
	other := db.RolePool(t, "litopys_app", 1) // no privilege beyond the app's
	tx, err := other.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// a command in flight: it holds the maintenance lock shared, as verbs.exec does (any role may)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared($1)`, verbs.MaintenanceLock); err != nil {
		t.Fatal(err)
	}
	if err := Drain(ctx, op, 600*time.Millisecond); err == nil {
		t.Fatal("drain returned while a write transaction was open")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := Drain(ctx, op, 5*time.Second); err != nil {
		t.Fatalf("drain after the commit: %v", err)
	}
	if err := SetMaintenance(ctx, op, false, ""); err != nil {
		t.Fatal(err)
	}
	if m, _ := GetMaintenance(ctx, op); m.ReadOnly || m.Since != nil || m.Reason != nil {
		t.Errorf("off: %+v", m)
	}
}
