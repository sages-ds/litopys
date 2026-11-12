package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestGateStartBundleDecisions (roast r1): payments dated next weeks and the outcomes of closed sessions
// pushed every decision out of the start bundle, and the project looked updated in the future. Now the current
// decisions have their own block that outlives the recent statements, outcomes stay out of recent_statements (they
// are open_outcomes), the order is by creation and updated_at never runs ahead of the recording time.
func TestGateStartBundleDecisions(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	v := &verbs.Service{DB: app}
	q := &query.Service{DB: app}
	var tenant uuid.UUID
	if err := app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	qme := query.Principal{TenantID: tenant, ActorID: ada}
	if _, _, err := v.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "kitchen"}); err != nil {
		t.Fatal(err)
	}
	assert := func(typ, title string, at *time.Time) {
		t.Helper()
		body := title + ": body"
		if _, err := v.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "kitchen", Type: typ, Title: title,
			Body: &body, EffectiveAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	assert("decision", "Budget is 180k", nil)
	// a session that leaves outcomes behind
	open, err := v.ContextOpen(ctx, me, verbs.ContextOpenIn{Project: "kitchen", Attach: "none"})
	if err != nil {
		t.Fatal(err)
	}
	var outs []verbs.OutcomeIn
	for i := range 6 {
		outs = append(outs, verbs.OutcomeIn{Topic: fmt.Sprintf("topic %d", i), Result: "checked", State: "done"})
	}
	if _, err := v.ContextCloseWith(ctx, me, open.ContextID, verbs.CloseIn{Summary: "a session", Outcomes: outs}); err != nil {
		t.Fatal(err)
	}
	// payments dated weeks ahead, written after the decision
	for i := range 40 {
		at := time.Now().AddDate(0, 0, 7+i)
		assert("note", fmt.Sprintf("Payment %d due", i), &at)
	}

	b, err := q.ContextPack(ctx, qme, "kitchen", 1500)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Decisions) != 1 || b.Decisions[0].Title != "Budget is 180k" {
		t.Fatalf("decisions block: %+v (truncated %v)", b.Decisions, b.Budget.Truncated)
	}
	for _, r := range b.RecentStatements {
		if r.Type == "outcome" || r.Type == "decision" {
			t.Errorf("recent_statements carry %s %q: outcomes are open_outcomes, decisions have their block", r.Type, r.Title)
		}
	}
	if len(b.RecentStatements) == 0 || b.RecentStatements[0].Title != "Payment 39 due" {
		t.Errorf("recent_statements must be newest by creation first: %+v", b.RecentStatements)
	}
	if b.Project.UpdatedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("project updated_at in the future: %v", b.Project.UpdatedAt)
	}
}
