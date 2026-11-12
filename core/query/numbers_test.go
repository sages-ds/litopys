package query_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestSearchByNumber: a query that is a number reads the entity first, then who mentions it, then
// the usual list; legacy forms of a record number meet; a task comes as the entity; another person's number is not
// this person's.
func TestSearchByNumber(t *testing.T) {
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
	must(t, "tenant", app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant))
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must(t, "human", err)
	other, _, err := v.RegisterHuman(ctx, tenant, "Other", "other@example")
	must(t, "other", err)

	write := func(who uuid.UUID, project, title string, mentions ...verbs.EntityRef) verbs.StatementAssertOut {
		t.Helper()
		me := verbs.Principal{TenantID: tenant, ActorID: who}
		open, err := v.ContextOpen(ctx, me, verbs.ContextOpenIn{Project: project, Attach: "none", AutoCreate: true})
		must(t, "open", err)
		me.RunID = open.RunID
		out, err := v.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: project, Type: "note", Title: title,
			Body: ptr("details of " + title), ContextID: &open.ContextID, Mentions: mentions})
		must(t, "assert "+title, err)
		return out
	}
	target := write(ada, "litopys", "Пул fastpool")
	mention := write(ada, "litopys", "Міграція спирається на пул", verbs.EntityRef{EntityType: "statement", EntityID: target.StatementID})
	write(ada, "litopys", "Інший запис про пул")
	foreign := write(other, "elsewhere", "Чужий запис")
	me := query.Principal{TenantID: tenant, ActorID: ada}
	search := func(qs, project string) query.SearchOut {
		t.Helper()
		out, err := q.Search(ctx, me, query.SearchIn{Query: qs, Project: project, Limit: 20})
		must(t, "search "+qs, err)
		return out
	}

	for _, form := range []string{target.Ref, strings.ToLower(target.Ref)} {
		r := search(form, "")
		if len(r.Items) < 2 || r.Items[0].StatementID != target.StatementID || r.Items[1].StatementID != mention.StatementID ||
			r.Items[0].Ref != target.Ref || r.Coverage != "full" {
			t.Errorf("%s: the record first, then who mentions it: %d items", form, len(r.Items))
		}
	}
	// numbers are per owner: the other person's R-1 is their own record, mine is mine
	if foreign.Ref != target.Ref {
		t.Errorf("numbers per owner: %s and %s", foreign.Ref, target.Ref)
	}
	for _, it := range search(foreign.Ref, "").Items {
		if it.StatementID == foreign.StatementID {
			t.Error("another person's record by number")
		}
	}

	// a task: the entity, then the records that mention it; the short form only within the project filter
	sp := verbs.Principal{TenantID: tenant, ActorID: ada}
	task, err := v.TaskDeclare(ctx, sp, verbs.TaskDeclareIn{Project: "litopys", Fields: map[string]any{"title": "Перевірити пул"}})
	must(t, "task", err)
	byTask := write(ada, "litopys", "Перевірку пулу заплановано", verbs.EntityRef{EntityType: "task", EntityID: task.TaskID})
	r := search(task.Ref, "")
	if r.Entity == nil || r.Entity.Ref != task.Ref || r.Entity.Type != "task" || len(r.Items) == 0 || r.Items[0].StatementID != byTask.StatementID {
		t.Errorf("%s: entity %+v, %d items", task.Ref, r.Entity, len(r.Items))
	}
	short := fmt.Sprintf("T-%d", task.Num)
	if r := search(short, ""); r.Entity != nil {
		t.Errorf("%s without a project names no task: %+v", short, r.Entity)
	}
	if r := search(short, "litopys"); r.Entity == nil || r.Entity.ID != task.TaskID {
		t.Errorf("%s in project litopys: %+v", short, r.Entity)
	}
	// a number nobody has: the usual search, no entity
	if r := search("R-999999", ""); r.Entity != nil || (len(r.Items) > 0 && r.Items[0].StatementID == target.StatementID) {
		t.Error("R-999999 names a record")
	}

	// a task that mentions a record comes in mentioned_by — it is not a record, so not among the items
	byRecord, err := v.TaskDeclare(ctx, sp, verbs.TaskDeclareIn{Project: "litopys", Fields: map[string]any{"title": "Розширити пул"},
		Mentions: []verbs.EntityRef{{EntityType: "statement", EntityID: target.StatementID}}})
	must(t, "task with refs", err)
	if r := search(target.Ref, ""); len(r.MentionedBy) != 1 || r.MentionedBy[0].Ref != byRecord.Ref || r.MentionedBy[0].Type != "task" ||
		r.Items[0].StatementID != target.StatementID {
		t.Errorf("%s: mentioned_by %+v", target.Ref, r.MentionedBy)
	}

	// the text fallback of task_resume.related knows T-N and <project>/T-N standing alone, not another project's
	task2, err := v.TaskDeclare(ctx, sp, verbs.TaskDeclareIn{Project: "litopys", Fields: map[string]any{"title": "Ще одна задача"}})
	must(t, "task 2", err)
	full := write(ada, "litopys", fmt.Sprintf("Згадка litopys/T-%d текстом", task2.Num))
	short2 := write(ada, "litopys", fmt.Sprintf("Згадка (T-%d) коротко", task2.Num))
	write(ada, "litopys", fmt.Sprintf("Чужа elsewhere/T-%d і слово ST-%d", task2.Num, task2.Num))
	rel, err := q.TaskMentions(ctx, me, "litopys", task2.Num, 20)
	must(t, "task mentions", err)
	found := map[uuid.UUID]bool{}
	for _, r := range rel {
		found[r.StatementID] = true
	}
	if len(rel) != 2 || !found[full.StatementID] || !found[short2.StatementID] {
		t.Errorf("text fallback for T-%d: %d rows", task2.Num, len(rel))
	}
}
