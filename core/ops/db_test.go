package ops

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/journal"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestGateOpsGatherDB: the database facts of doctor on a migrated throw-away database (run-gates.ps1 -Run Ops):
// the schema as embedded, a journal event the projector has not applied shows as lag with its age, the lag
// is not repeated as a verify finding, and the read stays in a READ ONLY transaction.
func TestGateOpsGatherDB(t *testing.T) {
	ctx := pg.WithScope(context.Background(), pg.ScopeSystem)
	db := pgtest.New(t)
	mig := db.MigratorConn(t)
	if _, err := migrate.Up(ctx, mig); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	app := db.RolePool(t, "litopys_app", 2)

	f, err := GatherDB(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Applied) != len(embedded()) {
		t.Errorf("applied %d migrations, embedded %d", len(f.Applied), len(embedded()))
	}
	if len(f.Tenants) == 0 || f.Tenants[0].Head != 0 || f.Tenants[0].OldestPendingAt != nil {
		t.Errorf("fresh tenants %+v", f.Tenants)
	}
	if len(f.Findings) != 0 {
		t.Errorf("fresh database findings %v", f.Findings)
	}

	var tenant uuid.UUID
	if err := mig.QueryRow(ctx, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	run, ent := uuid.New(), uuid.New()
	// the journal alone, without the projector: exactly the state of a stuck projection
	if err := pg.InTx(ctx, app, func(tx pgx.Tx) error {
		return journal.Append(ctx, tx, &journal.Event{TenantID: tenant, Type: "statement.asserted",
			ActorID: run, RunID: run, CausationID: run, CorrelationID: run,
			EntityType: "statement", EntityID: ent, Payload: json.RawMessage(`{"title":"ops gate"}`)})
	}); err != nil {
		t.Fatal(err)
	}
	f, err = GatherDB(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	var d *Tenant
	for i := range f.Tenants {
		if f.Tenants[i].Name == "default" {
			d = &f.Tenants[i]
		}
	}
	if d == nil || d.Head != 1 || d.ProjectorSeq != 0 || d.OldestPendingAt == nil {
		t.Fatalf("lag not seen: %+v", f.Tenants)
	}
	for _, s := range f.Findings {
		t.Errorf("lag repeated as a verify finding (doctor checks it with its age): %s", s)
	}
	if f.ClockSkew.Abs().Seconds() > 60 {
		t.Errorf("clock skew %s against the stand: the measurement is wrong", f.ClockSkew)
	}
}
