package query_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestQuarantine: an agent hides a record or a task at once; no read, search, typo suggestion or
// journal page gives its content back; only a human releases it, and then everything comes back.
func TestQuarantine(t *testing.T) {
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
	agent, _, err := v.ActorRegister(ctx, sp, verbs.ActorRegisterIn{Kind: "agent", DisplayName: "helper", ExternalIdentity: ptr("agent:helper")})
	must(t, "agent", err)
	bot := verbs.Principal{TenantID: tenant, ActorID: agent}

	open, err := v.ContextOpen(ctx, sp, verbs.ContextOpenIn{Project: "vault", Attach: "none", AutoCreate: true})
	must(t, "open", err)
	p := sp
	p.RunID = open.RunID
	const word = "Kestrelvault"
	leak, err := v.StatementAssert(ctx, p, verbs.StatementAssertIn{Project: "vault", Type: "note", Title: "Доступ до сховища",
		Body: ptr("токен " + word + " для бекапу"), ContextID: &open.ContextID})
	must(t, "assert", err)
	_, err = v.StatementAssert(ctx, p, verbs.StatementAssertIn{Project: "vault", Type: "note", Title: "Бекап сховища щодня",
		Body: ptr("розклад"), ContextID: &open.ContextID})
	must(t, "assert other", err)
	leakRef := verbs.EntityRef{EntityType: "statement", EntityID: leak.StatementID}
	found := func(text string) bool {
		t.Helper()
		sr, err := q.Search(ctx, me, query.SearchIn{Query: text, Limit: 20})
		must(t, "search "+text, err)
		for _, x := range sr.Items {
			if x.StatementID == leak.StatementID {
				return true
			}
		}
		return false
	}
	if !found(word) {
		t.Fatal("the record is not found before the quarantine — the test proves nothing")
	}
	var since int64 // the whole journal of this throw-away database fits a page

	// --- an agent quarantines; a repeat is already ---------------------------------------------------------------
	_, err = v.JournalQuarantine(ctx, bot, leakRef, "секрет у тілі", true)
	must(t, "quarantine by an agent", err)
	again, err := v.JournalQuarantine(ctx, bot, leakRef, "секрет у тілі", true)
	must(t, "quarantine again", err)
	if !again.Already {
		t.Errorf("a repeated quarantine: %+v", again)
	}

	// read: the tombstone of the quarantine, no content, no history
	rd, err := q.Read(ctx, me, []uuid.UUID{leak.StatementID}, true)
	must(t, "read", err)
	if len(rd.Items) != 1 || !rd.Items[0].Quarantined || rd.Items[0].Tombstone == nil || rd.Items[0].Tombstone.State != "quarantined" ||
		rd.Items[0].Tombstone.By.ActorID != agent || rd.Items[0].Tombstone.Reason != "секрет у тілі" || rd.Items[0].Body != nil ||
		rd.Items[0].Title != "[quarantined]" || len(rd.Items[0].History) != 0 {
		t.Fatalf("read of a quarantined record: %+v", rd.Items)
	}
	// search: neither the word nor the number gives the content; the dictionaries lost its words
	if found(word) || found("токен бекапу") {
		t.Error("search finds a quarantined record")
	}
	sr, err := q.Search(ctx, me, query.SearchIn{Query: leak.Ref, Limit: 20})
	must(t, "search number", err)
	if sr.Entity == nil || sr.Entity.State != "quarantined" || sr.Entity.Title != "" || sr.Entity.Tombstone == nil {
		t.Errorf("number of a quarantined record: %+v", sr.Entity)
	}
	var vocab int
	must(t, "vocab", app.QueryRow(sys, `SELECT (SELECT count(*) FROM projection.search_vocab WHERE word = lower($1))
	        + (SELECT count(*) FROM projection.stem_vocab WHERE stem LIKE 'kestrel%')`, word).Scan(&vocab))
	if vocab != 0 {
		t.Errorf("the dictionaries keep %d words of a quarantined record (typo channel)", vocab)
	}
	// journal.changes: the record's events are masked, the quarantine itself comes through
	changes := func() string {
		t.Helper()
		ch, err := q.Changes(ctx, me, query.ChangesIn{SinceSeq: since, Limit: 500})
		must(t, "changes", err)
		var b strings.Builder
		for _, e := range ch.Events {
			if e.EntityID == leak.StatementID {
				b.WriteString(e.Type + string(e.Payload) + "\n")
			}
		}
		return b.String()
	}
	if c := changes(); strings.Contains(c, word) || !strings.Contains(c, `"quarantined": true`) || !strings.Contains(c, "journal.quarantined") {
		t.Errorf("journal.changes of a quarantined record:\n%s", c)
	}
	// nothing above carries the hidden word
	for name, out := range map[string]any{"read": rd, "number": sr} {
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), word) || strings.Contains(string(raw), "Доступ до сховища") {
			t.Errorf("%s leaks the quarantined content: %s", name, raw)
		}
	}

	// --- only a human releases; then everything is back ------------------------------------------------------------
	_, err = v.JournalRelease(ctx, bot, leakRef, "помилка")
	if verbs.Code(err) != "release_requires_human" {
		t.Errorf("an agent releases: %v", err)
	}
	_, err = v.JournalRelease(ctx, sp, leakRef, "не секрет, тестовий токен")
	must(t, "release", err)
	_, err = v.JournalRelease(ctx, sp, leakRef, "ще раз")
	if verbs.Code(err) != "not_quarantined" {
		t.Errorf("a release of a released record: %v", err)
	}
	if !found(word) {
		t.Error("search does not find a released record")
	}
	if dl, err := q.DeletionList(ctx, me, query.DeletionListIn{Project: "vault", State: "rejected"}); err != nil || len(dl.Items) != 1 ||
		dl.Items[0].Action != "purge" || !dl.Items[0].Secret {
		t.Errorf("the release keeps the erase the quarantine proposed: %+v, %v", dl.Items, err)
	}
	if c := changes(); !strings.Contains(c, word) {
		t.Errorf("journal.changes after the release still masks:\n%s", c)
	}
	rd, err = q.Read(ctx, me, []uuid.UUID{leak.StatementID}, false)
	must(t, "read released", err)
	if rd.Items[0].Quarantined || rd.Items[0].Tombstone != nil || rd.Items[0].Body == nil {
		t.Errorf("read of a released record: %+v", rd.Items[0])
	}

	// --- a task: gone from the list, its number answers with the tombstone, amends are refused ---------------------
	task, err := v.TaskDeclare(ctx, sp, verbs.TaskDeclareIn{Project: "vault", Fields: map[string]any{"title": "Ключ " + word}})
	must(t, "task", err)
	_, err = v.JournalQuarantine(ctx, bot, verbs.EntityRef{EntityType: "task", EntityID: task.TaskID}, "секрет у назві", true)
	must(t, "quarantine task", err)
	tl, err := q.TaskList(ctx, me, query.TaskListIn{Project: "vault", State: "all"})
	must(t, "task list", err)
	if len(tl.Items) != 0 {
		t.Errorf("task list shows a quarantined task: %+v", tl.Items)
	}
	sr, err = q.Search(ctx, me, query.SearchIn{Query: task.Ref, Limit: 20})
	must(t, "search task number", err)
	if sr.Entity == nil || sr.Entity.State != "quarantined" || sr.Entity.Title != "" || sr.Entity.Tombstone == nil {
		t.Errorf("number of a quarantined task: %+v", sr.Entity)
	}
	if _, err := v.TaskAmend(ctx, bot, verbs.TaskAmendIn{TaskID: task.TaskID, Fields: map[string]any{"title": "x"}}); verbs.Code(err) != "task_not_found" {
		t.Errorf("amend of a quarantined task: %v", err)
	}

	// --- a purged record cannot be quarantined or released ----------------------------------------------------------
	_, err = v.JournalPurge(ctx, sp, "statement", leak.StatementID, "стерто", nil)
	must(t, "purge", err)
	if _, err := v.JournalQuarantine(ctx, bot, leakRef, "x", false); verbs.Code(err) != "already_purged" {
		t.Errorf("quarantine of a purged record: %v", err)
	}

	// --- a types filter always lets journal.* through; the project filter still holds -------------------
	_, err = v.ContextOpen(ctx, sp, verbs.ContextOpenIn{Project: "other", Attach: "none", AutoCreate: true})
	must(t, "other project", err)
	filtered := func(project string) map[string]bool {
		t.Helper()
		ch, err := q.Changes(ctx, me, query.ChangesIn{SinceSeq: since, Limit: 500, Types: []string{"statement.asserted"}, Project: project})
		must(t, "changes by type", err)
		got := map[string]bool{}
		for _, e := range ch.Events {
			if e.EntityID == leak.StatementID {
				got[e.Type] = true
			}
		}
		return got
	}
	want := map[string]bool{"statement.asserted": true, "journal.quarantined": true, "journal.released": true, "journal.purged": true}
	for _, project := range []string{"", "vault"} {
		if got := filtered(project); !reflect.DeepEqual(got, want) {
			t.Errorf("types [statement.asserted], project %q: %v", project, got)
		}
	}
	if got := filtered("other"); len(got) != 0 {
		t.Errorf("the project filter lets another project's journal.* through: %v", got)
	}
}
