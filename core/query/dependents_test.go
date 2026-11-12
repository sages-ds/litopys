package query_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestDependents: a task others wait on goes — retracted, quarantined or erased — and nothing unblocks
// silently: each open dependent keeps waiting, and a question record by the owner's automation names both by ref
// (never the gone task's title) and asks what next. A task that is already gone asks nobody again.
func TestDependents(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	v := &verbs.Service{DB: app, Admin: db.RolePool(t, "litopys_admin", 1)}
	q := &query.Service{DB: app}
	var tenant uuid.UUID
	must(t, "tenant", app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant))
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must(t, "human", err)
	sp := verbs.Principal{TenantID: tenant, ActorID: ada}
	me := query.Principal{TenantID: tenant, ActorID: ada}

	task := func(project, title string) verbs.TaskDeclareOut {
		t.Helper()
		_, _, _ = v.ProjectDeclare(ctx, sp, verbs.ProjectDeclareIn{Name: project})
		out, err := v.TaskDeclare(ctx, sp, verbs.TaskDeclareIn{Project: project, Fields: map[string]any{"title": title}})
		must(t, "task "+title, err)
		return out
	}
	waitsOn := func(from, to verbs.TaskDeclareOut) {
		t.Helper()
		_, _, err := v.LinkAssert(ctx, sp, verbs.EntityRef{EntityType: "task", EntityID: from.TaskID},
			verbs.EntityRef{EntityType: "task", EntityID: to.TaskID}, "depends_on", nil)
		must(t, "depends_on", err)
	}
	questions := func(project string) []query.IndexRow {
		t.Helper()
		open, err := q.WorkOpenItems(ctx, me, project)
		must(t, "open items "+project, err)
		return open.Questions
	}
	waiting := func(project string, id uuid.UUID) int {
		t.Helper()
		tl, err := q.TaskList(ctx, me, query.TaskListIn{Project: project})
		must(t, "task list", err)
		for _, x := range tl.Items {
			if x.TaskID == id {
				return x.WaitingOn
			}
		}
		t.Fatalf("task %s not listed", id)
		return 0
	}

	// a dependency in another project: the question goes to the dependent's project
	base := task("infra", "Ключ Wrenstone для бекапу")
	app1 := task("apps", "Розгорнути сервіс")
	waitsOn(app1, base)
	_, err = v.TaskRetract(ctx, sp, base.TaskID, "не потрібна")
	must(t, "retract", err)
	qs := questions("apps")
	if len(qs) != 1 || !strings.Contains(qs[0].Title, base.Ref) || !strings.Contains(qs[0].Title, app1.Ref) ||
		!strings.Contains(qs[0].Title, "retracted") || strings.Contains(qs[0].Title, "Wrenstone") || qs[0].Authority != "owner" {
		t.Fatalf("question after a retract: %+v", qs)
	}
	if len(questions("infra")) != 0 {
		t.Error("the question went to the gone task's project")
	}
	if n := waiting("apps", app1.TaskID); n != 1 {
		t.Errorf("the dependent is unblocked by a retract: waiting_on %d", n)
	}
	var kind string
	must(t, "author", app.QueryRow(sys, `SELECT a.kind FROM projection.statement s JOIN projection.actor a ON a.actor_id = s.actor_id
	        WHERE s.statement_id = $1`, qs[0].StatementID).Scan(&kind))
	if kind != "automation" {
		t.Errorf("the question is written by %s, not the owner's automation", kind)
	}
	// the gone task erased afterwards: nobody is asked a second time
	_, err = v.JournalPurge(ctx, sp, "task", base.TaskID, "секрет у назві", nil)
	must(t, "purge retracted", err)
	if n := len(questions("apps")); n != 1 {
		t.Errorf("questions after erasing an already retracted task: %d", n)
	}

	// quarantine and erase of a live task ask too; a closed dependent is not asked
	dep := task("apps", "Оновити конфіг")
	done := task("apps", "Стара робота")
	quar := task("apps", "Токен у назві")
	waitsOn(dep, quar)
	waitsOn(done, quar)
	_, err = v.TaskClose(ctx, sp, done.TaskID, "done", nil)
	must(t, "close", err)
	_, err = v.JournalQuarantine(ctx, sp, verbs.EntityRef{EntityType: "task", EntityID: quar.TaskID}, "секрет", false)
	must(t, "quarantine", err)
	qs = questions("apps")
	if len(qs) != 2 || !strings.Contains(qs[0].Title+qs[1].Title, "quarantined") || strings.Contains(qs[0].Title+qs[1].Title, done.Ref+"?") {
		t.Errorf("questions after a quarantine: %+v", qs)
	}
	gone := task("apps", "Стерти")
	waitsOn(dep, gone)
	_, err = v.JournalPurge(ctx, sp, "task", gone.TaskID, "дані", nil)
	must(t, "purge live", err)
	if n := len(questions("apps")); n != 3 {
		t.Errorf("questions after erasing a live task: %d", n)
	}
	if n := waiting("apps", dep.TaskID); n != 2 {
		t.Errorf("the dependent of a quarantined and an erased task: waiting_on %d", n)
	}
}
