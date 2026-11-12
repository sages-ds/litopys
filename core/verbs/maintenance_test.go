package verbs_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestMaintenanceReadOnly (0042): while ops.maintenance is read-only every command refuses with code maintenance
// and writes nothing; reads go on; switched off, the same command goes through.
func TestMaintenanceReadOnly(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 2)
	s := &verbs.Service{DB: app}
	var tenant uuid.UUID
	must(t, "tenant", app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant))
	ada, _, err := s.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must(t, "human", err)
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	_, _, err = s.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "litopys-test"})
	must(t, "project", err)

	op := db.MigratorConn(t) // the operator's role owns the schema ops
	if _, err := op.Exec(ctx, `UPDATE ops.maintenance SET read_only = true, reason = 'update to v1', since = now()`); err != nil {
		t.Fatal(err)
	}
	var before int
	must(t, "journal", app.QueryRow(sys, `SELECT count(*) FROM journal`).Scan(&before))
	_, _, err = s.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "another"})
	if verbs.Code(err) != "maintenance" {
		t.Fatalf("a command while read-only: %v, want code maintenance", err)
	}
	var after, projects int
	must(t, "journal", app.QueryRow(sys, `SELECT count(*) FROM journal`).Scan(&after))
	must(t, "read", app.QueryRow(sys, `SELECT count(*) FROM projection.project`).Scan(&projects)) // reads go on
	if after != before || projects != 1 {
		t.Fatalf("read-only wrote: journal %d -> %d, projects %d", before, after, projects)
	}
	if _, err := op.Exec(ctx, `UPDATE ops.maintenance SET read_only = false, reason = NULL, since = NULL`); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "another"})
	must(t, "after the mode is off", err)
}
