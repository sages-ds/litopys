package gates

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
)

// projectionTables are the owned read models of 0008.
var projectionTables = []string{"actor", "run", "context", "project", "task", "fact", "statement",
	"statement_version", "statement_lexical", "statement_search", "link", "search_vocab", "stem_vocab", "consumer"}

type person struct {
	id      uuid.UUID
	p       verbs.Principal
	q       query.Principal
	stmt    uuid.UUID
	context uuid.UUID
}

// TestGateIsolation is the acceptance of the isolation rules: two people with a project of the
// same name, written through the real commands, never see each other — neither through the query
// services nor through direct SQL of litopys_app (RLS); without an owner nothing is visible; only
// the projector writes projections.
func TestGateIsolation(t *testing.T) {
	ctx := context.Background() // the services resolve the owner from the principal themselves
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db, tenant := stand(t)
	app := db.RolePool(t, "litopys_app", 4)
	v := &verbs.Service{DB: app}
	q := &query.Service{DB: app}
	str := func(s string) *string { return &s }
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	people := map[string]*person{}
	for _, name := range []string{"Alice", "Bob"} {
		id, _, err := v.RegisterHuman(ctx, tenant, name, name+"@example")
		must("register "+name, err)
		x := &person{id: id, p: verbs.Principal{TenantID: tenant, ActorID: id}}
		open, err := v.ContextOpen(ctx, x.p, verbs.ContextOpenIn{Project: "workshop", Attach: "none", AutoCreate: true})
		must(name+" opens workshop", err)
		x.p.RunID, x.context = open.RunID, open.ContextID
		x.q = query.Principal{TenantID: tenant, ActorID: id}
		_, _, err = v.ActorRegister(ctx, x.p, verbs.ActorRegisterIn{Kind: "agent", DisplayName: "sub of " + name, ExternalIdentity: str("agent:sub")})
		must(name+" registers an agent", err)
		st, err := v.StatementAssert(ctx, x.p, verbs.StatementAssertIn{Project: "workshop", Type: "note",
			Title: "secret of " + name, Body: str("private words of " + name), ContextID: &open.ContextID})
		must(name+" asserts", err)
		x.stmt = st.StatementID
		st2, err := v.StatementAssert(ctx, x.p, verbs.StatementAssertIn{Project: "workshop", Type: "note", Title: "second of " + name})
		must(name+" asserts again", err)
		_, _, err = v.LinkAssert(ctx, x.p, verbs.EntityRef{EntityType: "statement", EntityID: st2.StatementID},
			verbs.EntityRef{EntityType: "statement", EntityID: st.StatementID}, "related", nil)
		must(name+" links", err)
		_, err = v.StatementAmend(ctx, x.p, verbs.StatementAmendIn{StatementID: st.StatementID, Body: str("amended words of " + name)})
		must(name+" amends", err)
		_, err = v.TaskDeclare(ctx, x.p, verbs.TaskDeclareIn{Project: "workshop", Fields: map[string]any{"title": "task of " + name}})
		must(name+" declares a task", err)
		_, err = v.FactRecord(ctx, x.p, verbs.FactRecordIn{Project: "workshop", Kind: "note", Key: "stand", Value: str("value of " + name)})
		must(name+" records a fact", err)
		_, err = v.ContextCheckpoint(ctx, x.p, open.ContextID, "checkpoint of "+name, str("current of "+name))
		must(name+" checkpoints", err)
		_, err = v.ConsumerCheckpoint(ctx, x.p, "vault-harvest", 1)
		must(name+" moves a cursor", err)
		people[name] = x
	}
	alice, bob := people["Alice"], people["Bob"]

	// (a) the services give each person only their own data
	for _, pair := range [][2]*person{{alice, bob}, {bob, alice}} {
		me, them := pair[0], pair[1]
		projects, err := q.ProjectList(ctx, me.q, "")
		must("project list", err)
		if len(projects) != 1 || projects[0].Name != "workshop" || projects[0].Current == nil || *projects[0].Current != "current of "+nameOf(people, me) {
			t.Errorf("project list of %s: %+v", nameOf(people, me), projects)
		}
		found, err := q.Search(ctx, me.q, query.SearchIn{Query: "words"})
		must("search", err)
		for _, r := range found.Items {
			if r.StatementID == them.stmt {
				t.Errorf("%s finds a statement of %s", nameOf(people, me), nameOf(people, them))
			}
		}
		if len(found.Items) == 0 {
			t.Errorf("%s does not find own statement", nameOf(people, me))
		}
		read, err := q.Read(ctx, me.q, []uuid.UUID{them.stmt}, true)
		must("read", err)
		if len(read.Items) != 0 {
			t.Errorf("%s reads a statement of %s", nameOf(people, me), nameOf(people, them))
		}
		tasks, err := q.TaskList(ctx, me.q, query.TaskListIn{Project: "workshop", State: "all"})
		must("task list", err)
		if tasks.Total != 1 {
			t.Errorf("%s sees %d tasks in workshop, want 1", nameOf(people, me), tasks.Total)
		}
		facts, err := q.FactList(ctx, me.q, "workshop", "", "")
		must("fact list", err)
		if len(facts) != 1 || facts[0].Value == nil || *facts[0].Value != "value of "+nameOf(people, me) {
			t.Errorf("facts of %s: %+v", nameOf(people, me), facts)
		}
		cursors, err := q.ConsumerList(ctx, me.q, "")
		must("consumer list", err)
		if len(cursors.Items) != 1 {
			t.Errorf("%s sees %d consumer cursors, want 1", nameOf(people, me), len(cursors.Items))
		}
		changes, err := q.Changes(ctx, me.q, query.ChangesIn{SinceSeq: 0, Limit: 500})
		must("changes", err)
		for _, e := range changes.Events {
			if e.ActorID == them.id {
				t.Errorf("%s sees event seq %d of %s in the changefeed", nameOf(people, me), e.Seq, nameOf(people, them))
			}
		}
		_, err = v.StatementRetract(ctx, me.p, them.stmt, nil)
		if verbs.Code(err) != "statement_not_found" {
			t.Errorf("%s retracts a statement of %s: %v", nameOf(people, me), nameOf(people, them), err)
		}
		_, err = v.ContextCheckpoint(ctx, me.p, them.context, "hijack", nil)
		if err == nil {
			t.Errorf("%s checkpoints a context of %s", nameOf(people, me), nameOf(people, them))
		}
	}

	// (b) direct SQL of litopys_app: Alice sees her rows only, in every owned table and the journal
	asAlice := pg.WithOwner(ctx, alice.id)
	count := func(c context.Context, sql string, args ...any) int {
		t.Helper()
		var n int
		must(sql, app.QueryRow(c, sql, args...).Scan(&n))
		return n
	}
	for _, tbl := range projectionTables {
		mine := count(sys, fmt.Sprintf(`SELECT count(*) FROM projection.%s WHERE owner_id = $1`, tbl), alice.id)
		if mine == 0 {
			t.Errorf("%s: Alice owns no rows — the scenario does not exercise it", tbl)
		}
		if got := count(asAlice, `SELECT count(*) FROM projection.`+tbl); got != mine {
			t.Errorf("%s: Alice sees %d rows, owns %d", tbl, got, mine)
		}
		if got := count(ctx, `SELECT count(*) FROM projection.`+tbl); got != 0 { // (c) no owner
			t.Errorf("%s: %d rows visible without an owner", tbl, got)
		}
	}
	all := count(sys, `SELECT count(*) FROM journal`)
	minej := count(sys, `SELECT count(*) FROM journal WHERE COALESCE(litopys_root(actor_id), actor_id) = $1`, alice.id)
	if got := count(asAlice, `SELECT count(*) FROM journal`); got != minej || minej == 0 || minej == all {
		t.Errorf("journal: Alice sees %d events, owns %d of %d", got, minej, all)
	}
	if got := count(ctx, `SELECT count(*) FROM journal`); got != 0 {
		t.Errorf("journal: %d events visible without an owner", got)
	}

	// (d) only the projector writes projections
	_, err := app.Exec(asAlice, `UPDATE projection.statement SET title = 'forged' WHERE owner_id = $1`, alice.id)
	if got := sqlstate(err); got != "42501" {
		t.Errorf("UPDATE outside the projector: sqlstate %q (%v), want 42501", got, err)
	}
	_, err = app.Exec(asAlice, `INSERT INTO projection.consumer (tenant_id, owner_id, consumer, seq, updated_at)
	                            VALUES ($1, $2, 'forged', 1, now())`, tenant, alice.id)
	if got := sqlstate(err); got != "42501" {
		t.Errorf("INSERT outside the projector: sqlstate %q (%v), want 42501", got, err)
	}
}

func nameOf(people map[string]*person, x *person) string {
	for n, p := range people {
		if p == x {
			return n
		}
	}
	return "?"
}
