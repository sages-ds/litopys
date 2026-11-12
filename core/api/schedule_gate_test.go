package api_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestGateScheduledRuns: a workflow with a schedule trigger is started by the worker's pass at a
// minute its cron names, once per minute however many passes; not at other minutes; a bad cron is refused on declare.
func TestGateScheduledRuns(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	v := &verbs.Service{DB: app}
	var tenant uuid.UUID
	if err := app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	if _, _, err := v.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "kitchen"}); err != nil {
		t.Fatal(err)
	}
	def := func(spec string) json.RawMessage {
		return json.RawMessage(`{"triggers": [{"kind": "schedule", "spec": "` + spec + `"}],
		  "steps": [{"key": "digest", "title": "Morning digest", "kind": "model", "verbs": ["work.open_items"]}]}`)
	}
	if _, err := v.WorkflowDeclare(ctx, me, verbs.WorkflowDeclareIn{Project: "kitchen", Key: "bad", Title: "Bad", Definition: def("61 7 * * *")}); err == nil {
		t.Error("a bad cron was accepted")
	}
	if _, err := v.WorkflowDeclare(ctx, me, verbs.WorkflowDeclareIn{Project: "kitchen", Key: "digest", Title: "Digest", Definition: def("0 7 * * *")}); err != nil {
		t.Fatal(err)
	}
	seven, _ := time.ParseInLocation("2006-01-02 15:04", "2026-11-02 07:00", time.FixedZone("EET", 2*3600))
	if got, err := v.StartScheduled(ctx, tenant, seven.Add(-time.Minute)); err != nil || len(got) != 0 {
		t.Fatalf("06:59: %v %v", got, err)
	}
	got, err := v.StartScheduled(ctx, tenant, seven)
	if err != nil || len(got) != 1 {
		t.Fatalf("07:00: %v %v", got, err)
	}
	if again, _ := v.StartScheduled(ctx, tenant, seven.Add(20*time.Second)); len(again) != 0 {
		t.Errorf("a second pass in the same minute started %v", again)
	}
}
