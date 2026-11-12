package query_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestRoastR2 (roast round 2): what misled the models — refs taken for a dependency (Р2-10), a blocked task that
// cannot name who waits (Р2-3), an answered question that reads as plain current (Р2-2).
func TestRoastR2(t *testing.T) {
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
	for _, name := range []string{"a", "b"} {
		_, _, err := v.ProjectDeclare(ctx, sp, verbs.ProjectDeclareIn{Name: name})
		must(t, "project "+name, err)
	}
	ta := verbs.EntityRef{EntityType: "task"}

	// Р2-10: a task whose refs name another task, without a link, is told so
	base, err := v.TaskDeclare(ctx, sp, verbs.TaskDeclareIn{Project: "a", Fields: map[string]any{"title": "Backup check"}})
	must(t, "base", err)
	ta.EntityID = base.TaskID
	mirror, err := v.TaskDeclare(ctx, sp, verbs.TaskDeclareIn{Project: "b", Fields: map[string]any{"title": "Waits for the backup",
		"status": "waiting", "refs": []any{base.Ref}}, Mentions: []verbs.EntityRef{ta}})
	must(t, "mirror", err)
	if len(mirror.Warnings) != 1 || !strings.HasPrefix(mirror.Warnings[0], "refs_not_dependency") {
		t.Fatalf("refs without a link, warnings %q", mirror.Warnings)
	}
	_, _, err = v.LinkAssert(ctx, sp, verbs.EntityRef{EntityType: "task", EntityID: mirror.TaskID}, ta, "depends_on", nil)
	must(t, "depends_on", err)
	a, err := v.TaskAmend(ctx, sp, verbs.TaskAmendIn{TaskID: mirror.TaskID, Fields: map[string]any{"refs": []any{base.Ref}},
		Mentions: []verbs.EntityRef{ta}})
	must(t, "amend refs", err)
	if len(a.Warnings) != 0 {
		t.Errorf("refs with a link still warn: %q", a.Warnings)
	}

	// Р2-3: both ends name each other
	row := func(project string, id uuid.UUID) query.TaskRow {
		t.Helper()
		tl, err := q.TaskList(ctx, me, query.TaskListIn{Project: project})
		must(t, "task list", err)
		for _, x := range tl.Items {
			if x.TaskID == id {
				return x
			}
		}
		t.Fatalf("task %s not listed", id)
		return query.TaskRow{}
	}
	if r := row("a", base.TaskID); r.Blocking != 1 || !slices.Equal(r.BlockingRefs, []string{mirror.Ref}) || r.WaitingOnRefs != nil {
		t.Errorf("blocked side: blocking %d %v, waiting_on_refs %v", r.Blocking, r.BlockingRefs, r.WaitingOnRefs)
	}
	if r := row("b", mirror.TaskID); r.WaitingOn != 1 || !slices.Equal(r.WaitingOnRefs, []string{base.Ref}) || r.BlockingRefs != nil {
		t.Errorf("waiting side: waiting_on %d %v, blocking_refs %v", r.WaitingOn, r.WaitingOnRefs, r.BlockingRefs)
	}

	// Р2-2: an answered question names its answer in search and in the start pack
	question, err := v.StatementAssert(ctx, sp, verbs.StatementAssertIn{Project: "a", Type: "question", Title: "Move the socket by the sink?"})
	must(t, "question", err)
	answer, err := v.StatementAssert(ctx, sp, verbs.StatementAssertIn{Project: "a", Type: "decision", Title: "The socket stays"})
	must(t, "answer", err)
	_, _, err = v.LinkAssert(ctx, sp, verbs.EntityRef{EntityType: "statement", EntityID: answer.StatementID},
		verbs.EntityRef{EntityType: "statement", EntityID: question.StatementID}, "resolves", nil)
	must(t, "resolves", err)
	sr, err := q.Search(ctx, me, query.SearchIn{Project: "a"})
	must(t, "search", err)
	for _, r := range sr.Items {
		switch r.StatementID {
		case question.StatementID:
			if !slices.Equal(r.ResolvedBy, []string{answer.Ref}) {
				t.Errorf("answered question: resolved_by %v, want [%s]", r.ResolvedBy, answer.Ref)
			}
		case answer.StatementID:
			if r.ResolvedBy != nil {
				t.Errorf("the answer itself: resolved_by %v", r.ResolvedBy)
			}
		}
	}

	// Р2-6: a media type with parameters is taken; a metadata-only amend keeps when the thing happened
	att, err := v.AttachmentAdd(ctx, sp, "a", strings.Repeat("ab", 32), "estimate.txt", "text/plain; charset=utf-8", 12)
	must(t, "attachment with charset", err)
	past := time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Second)
	exp, err := v.StatementAssert(ctx, sp, verbs.StatementAssertIn{Project: "a", Type: "note", Title: "Worktop bought", EffectiveAt: &past})
	must(t, "dated note", err)
	_, err = v.StatementAmend(ctx, sp, verbs.StatementAmendIn{StatementID: exp.StatementID, Files: []string{att.Ref}})
	must(t, "attach file", err)
	var at time.Time
	must(t, "effective", app.QueryRow(sys, `SELECT effective_at FROM projection.statement WHERE statement_id = $1`, exp.StatementID).Scan(&at))
	if !at.Equal(past) {
		t.Errorf("attaching a file moved effective_at %s -> %s", past, at)
	}
}
