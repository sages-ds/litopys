package query_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/projector"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

func ptr[T any](v T) *T { return &v }

func must(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func ids(rows []query.IndexRow) map[uuid.UUID]bool {
	m := map[uuid.UUID]bool{}
	for _, r := range rows {
		m[r.StatementID] = true
	}
	return m
}

func TestQueries(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem) // direct checks read every owner (RLS of 0008)
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
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	qme := query.Principal{TenantID: tenant, ActorID: ada}
	qother := query.Principal{TenantID: tenant, ActorID: other}

	open, err := v.ContextOpen(ctx, me, verbs.ContextOpenIn{Project: "litopys", Attach: "none", AutoCreate: true,
		Client: []byte(`{"runtime":"claude-code"}`)})
	must(t, "open", err)
	me.RunID = open.RunID
	cx := &open.ContextID
	assert := func(title, typ string, body string, concepts ...string) uuid.UUID {
		t.Helper()
		out, err := v.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "litopys", Type: typ, Title: title,
			Body: ptr(body), Concepts: concepts, ContextID: cx})
		must(t, "assert "+title, err)
		return out.StatementID
	}
	mig := assert("Перенесли міграцію на новий пул", "decision", "Міграцію пулу виконано без простою сервісів.", "zfs")
	dep := assert("Деплой сервера через Semaphore", "config", "Деплой запускає раннер.", "deploy")
	qst := assert("Чи потрібен окремий PG для гейтів?", "question", "Відкрите питання до DBA.")
	old := assert("Журнал — перша версія", "decision", "Чернетка рішення.")
	gone := assert("Помилковий запис", "note", "Буде відкликано.")
	_, err = v.StatementRetract(ctx, me, gone, nil)
	must(t, "retract", err)
	amendAck, err := v.StatementAmend(ctx, me, verbs.StatementAmendIn{StatementID: old, Title: ptr("Журнал — друга версія"), Reason: ptr("уточнення")})
	must(t, "amend", err)
	repl, err := v.StatementSupersede(ctx, me, old, verbs.StatementAssertIn{Project: "litopys", Type: "decision",
		Title: "Журнал — остаточне рішення", ContextID: cx}, nil)
	must(t, "supersede", err)
	_, _, err = v.LinkAssert(ctx, me, verbs.EntityRef{EntityType: "statement", EntityID: dep},
		verbs.EntityRef{EntityType: "statement", EntityID: mig}, "related", nil)
	must(t, "link", err)
	_, err = v.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys", Fields: map[string]any{"title": "E17 differential test", "priority": "medium"}})
	must(t, "task", err)
	_, err = v.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys", Fields: map[string]any{"title": "Go-проєктор", "status": "in_progress", "priority": "low"}})
	must(t, "task 2", err)
	back, err := v.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys", Fields: map[string]any{"title": "Сайт", "status": "backlog", "body": "подумати"}})
	must(t, "task 3", err)
	_, err = v.TaskClose(ctx, me, back.TaskID, "dropped", ptr("не зараз"))
	must(t, "task 3 close", err)
	_, err = v.FactRecord(ctx, me, verbs.FactRecordIn{Project: "litopys", Kind: "note", Key: "stand", Value: ptr("db.example:5433")})
	must(t, "fact", err)
	_, err = v.ContextCheckpoint(ctx, me, open.ContextID, "Запити зрізу 1", ptr("пишу query_test"))
	must(t, "checkpoint", err)

	// another human's statement in a project of the same name must stay invisible to me
	// (since 0008 my project does not exist for them: they get their own "litopys")
	oopen, err := v.ContextOpen(ctx, verbs.Principal{TenantID: tenant, ActorID: other}, verbs.ContextOpenIn{Project: "litopys", Attach: "none", AutoCreate: true})
	must(t, "other open", err)
	foreign, err := v.StatementAssert(ctx, verbs.Principal{TenantID: tenant, ActorID: other, RunID: oopen.RunID},
		verbs.StatementAssertIn{Project: "litopys", Type: "note", Title: "Чужа міграція", ContextID: &oopen.ContextID})
	must(t, "foreign", err)

	// --- recall.search --------------------------------------------------------------------
	res, err := q.Search(ctx, qme, query.SearchIn{Query: "міграція"}) // word form: «міграцію» is in the corpus
	must(t, "search forms", err)
	if !ids(res.Items)[mig] || res.Relaxed {
		t.Fatalf("word-form expansion did not find «міграцію»: %+v", res)
	}
	if ids(res.Items)[foreign.StatementID] {
		t.Fatal("another human's statement leaked into search")
	}
	res, err = q.Search(ctx, qme, query.SearchIn{Query: "деплой неіснуючеслово"})
	must(t, "search relaxed", err)
	if !res.Relaxed || !ids(res.Items)[dep] {
		t.Fatalf("relaxed phase: %+v", res)
	}
	res, err = q.Search(ctx, qme, query.SearchIn{Query: "деплой", IncludeSnippet: true})
	must(t, "snippet", err)
	if len(res.Items) == 0 || res.Items[0].Snippet == nil || !strings.Contains(*res.Items[0].Snippet, "«") || res.Items[0].Score == nil {
		t.Fatalf("snippet/score: %+v", res.Items)
	}
	res, err = q.Search(ctx, qme, query.SearchIn{Project: "litopys"})
	must(t, "search all", err)
	got := ids(res.Items)
	if got[gone] || !got[old] || !got[repl.StatementID] {
		t.Fatalf("no-query search: retracted must be out, superseded in by default: %v", got)
	}
	for _, r := range res.Items {
		if r.Provenance.Seq == 0 || r.Provenance.EventID == uuid.Nil {
			t.Fatalf("row without provenance: %+v", r)
		}
	}
	res, err = q.Search(ctx, qme, query.SearchIn{Project: "litopys", ExcludeSuperseded: true, Concepts: []string{"zfs"}})
	must(t, "search filtered", err)
	if len(res.Items) != 1 || res.Items[0].StatementID != mig {
		t.Fatalf("concept filter: %+v", res.Items)
	}
	if res, _ = q.Search(ctx, qother, query.SearchIn{Query: "міграція"}); ids(res.Items)[mig] {
		t.Fatal("my statement visible to another human")
	}

	// --- recall.read / as_of / timeline ---------------------------------------------------
	rd, err := q.Read(ctx, qme, []uuid.UUID{old, gone, foreign.StatementID}, true)
	must(t, "read", err)
	if len(rd.Items) != 2 || rd.Items[0].Status != "superseded" || rd.Items[1].Status != "retracted" {
		t.Fatalf("read: statuses / visibility: %+v", rd.Items)
	}
	if len(rd.Items[0].History) != 3 || rd.Items[0].History[0].Title != "Журнал — перша версія" {
		t.Fatalf("history: %+v", rd.Items[0].History)
	}
	var amendedAt time.Time
	must(t, "amend time", app.QueryRow(sys, `SELECT recorded_at FROM journal WHERE seq = $1`, amendAck.Seq).Scan(&amendedAt))
	before := amendedAt.Add(-time.Microsecond)
	ao, err := q.AsOf(ctx, qme, query.AsOfIn{Project: "litopys", RecordedAt: &before})
	must(t, "as_of", err)
	var sawFirst bool
	for _, r := range ao.Items {
		sawFirst = sawFirst || (r.StatementID == old && r.Title == "Журнал — перша версія")
		if r.StatementID == old && r.SupersededBy != nil {
			t.Fatal("as_of before the supersede names the successor")
		}
		if r.StatementID == repl.StatementID {
			t.Fatal("as_of shows a statement recorded later")
		}
	}
	if !sawFirst || ao.Axes["recorded_at"] == "" {
		t.Fatalf("as_of transaction axis: %+v", ao)
	}
	now, err := q.AsOf(ctx, qme, query.AsOfIn{Project: "litopys"})
	must(t, "as_of now", err)
	if ids(now.Items)[old] {
		t.Fatal("as_of now shows the superseded statement as current")
	}
	tl, err := q.Timeline(ctx, qme, query.TimelineIn{Project: "litopys"})
	must(t, "timeline", err)
	var edges int
	for _, it := range tl.Items {
		for _, l := range it.Links {
			if l.Relation == "superseded_by" || l.Relation == "supersedes" || l.Relation == "related" || l.Relation == "related_by" {
				edges++
			}
		}
	}
	if edges < 4 {
		t.Fatalf("timeline edges: %d", edges)
	}

	// --- journal.changes: cursor advances past filtered events ----------------------------
	var all []query.Change
	cursor := int64(0)
	for {
		ch, err := q.Changes(ctx, qme, query.ChangesIn{SinceSeq: cursor, Limit: 5, Types: []string{"statement.asserted"}})
		must(t, "changes", err)
		all = append(all, ch.Events...)
		if ch.NextSeq == cursor {
			break
		}
		cursor = ch.NextSeq
	}
	if len(all) != 6 || cursor == 0 { // 5 mine + the replacement; the foreign one is not mine
		t.Fatalf("changes paging: %d events, cursor %d", len(all), cursor)
	}

	// --- lists · pack · system -------------------------------------------------------------
	oi, err := q.WorkOpenItems(ctx, qme, "litopys")
	must(t, "open items", err)
	tl2, err := q.TaskList(ctx, qme, query.TaskListIn{Project: "litopys"})
	must(t, "task list", err)
	if len(tl2.Items) != 2 || tl2.Items[0].Title != "Go-проєктор" || tl2.Counts["dropped"] != 1 || tl2.Items[0].Body != nil {
		t.Fatalf("task list order / counts / body: %+v %v", tl2.Items, tl2.Counts)
	}
	byNum, err := q.TaskList(ctx, qme, query.TaskListIn{Project: "litopys", Nums: []int{3}})
	must(t, "task by num", err)
	if len(byNum.Items) != 1 || byNum.Items[0].Status != "dropped" || byNum.Items[0].Body == nil || !byNum.Items[0].HasDetails {
		t.Fatalf("task by num (closed, with body): %+v", byNum.Items)
	}
	if len(oi.Tasks) != 2 || len(oi.Questions) != 1 || oi.Questions[0].StatementID != qst {
		t.Fatalf("open items: %+v", oi)
	}
	facts, err := q.FactList(ctx, qme, "", "", "5433")
	must(t, "facts", err)
	if len(facts) != 1 {
		t.Fatalf("fact query: %+v", facts)
	}
	cl, err := q.ContextList(ctx, qme, "litopys")
	must(t, "contexts", err)
	if len(cl) != 1 || cl[0].ContextID != open.ContextID {
		t.Fatalf("open contexts: %+v", cl)
	}
	_, err = v.ContextClose(ctx, me, open.ContextID, "Кінець дня", nil)
	must(t, "close", err)

	pack, err := q.ContextPack(ctx, qme, "litopys", 0)
	must(t, "pack", err)
	if pack.Current == nil || *pack.Current != "пишу query_test" || pack.LastSummary == nil || len(pack.RecentStatements) == 0 || len(pack.Budget.Truncated) != 0 {
		t.Fatalf("pack: %+v", pack)
	}
	small, err := q.ContextPack(ctx, qme, "litopys", 500)
	must(t, "pack small", err)
	if len(small.Budget.Truncated) == 0 || small.Current == nil || small.LastSummary == nil {
		t.Fatalf("pack under budget must cut statements but keep current/last_summary: %+v", small.Budget)
	}

	who, err := q.Whoami(ctx, qme)
	must(t, "whoami", err)
	if who.Email != "ada@example" || who.Role != "owner" {
		t.Fatalf("whoami: %+v", who)
	}
	h, err := q.Health(ctx, qme, projector.Version)
	must(t, "health", err)
	if !h.OK || h.ProjectorLagSeq != 0 || h.LastSeq == 0 {
		t.Fatalf("health: %+v", h)
	}
}
