package query_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestTombstones: a reference to an erased record still resolves — to its tombstone, with
// when, who and why, never its title or content; a retracted task answers its number with its state; links name the
// state of an end that is no longer live; the impact lists who depends on an entity, before and after its erasure.
func TestTombstones(t *testing.T) {
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

	write := func(title string, mentions ...verbs.EntityRef) verbs.StatementAssertOut {
		t.Helper()
		p := sp
		open, err := v.ContextOpen(ctx, p, verbs.ContextOpenIn{Project: "vault", Attach: "none", AutoCreate: true})
		must(t, "open", err)
		p.RunID = open.RunID
		out, err := v.StatementAssert(ctx, p, verbs.StatementAssertIn{Project: "vault", Type: "note", Title: title,
			Body: ptr("body of " + title), ContextID: &open.ContextID, Mentions: mentions})
		must(t, "assert "+title, err)
		return out
	}
	const secret = "Пароль сховища Zebra-Quartz"
	target := write(secret)
	targetRef := verbs.EntityRef{EntityType: "statement", EntityID: target.StatementID}
	mention := write("Ротація пароля сховища", targetRef)
	task, err := v.TaskDeclare(ctx, sp, verbs.TaskDeclareIn{Project: "vault", Fields: map[string]any{"title": "Змінити пароль"},
		Mentions: []verbs.EntityRef{targetRef}})
	must(t, "task", err)

	// --- impact of a live record: who mentions it, nothing changed by asking ------------------------------------
	im, err := q.Impact(ctx, me, "statement", target.StatementID)
	must(t, "impact", err)
	if im.Target.Ref == nil || *im.Target.Ref != target.Ref || im.Target.State != nil || len(im.Incoming) != 1 ||
		im.Incoming[0].Relation != "mentions_by" || im.Incoming[0].Count != 2 || len(im.Incoming[0].Sample) != 2 {
		t.Fatalf("impact of a live record: %+v", im)
	}

	_, err = v.JournalPurge(ctx, sp, "statement", target.StatementID, "секрет у вмісті", nil)
	must(t, "purge", err)

	// --- recall_read: the tombstone, no title or body --------------------------------------------------------------
	rd, err := q.Read(ctx, me, []uuid.UUID{target.StatementID}, true)
	must(t, "read purged", err)
	if len(rd.Items) != 1 || rd.Items[0].Tombstone == nil {
		t.Fatalf("read purged: %+v", rd.Items)
	}
	ts := rd.Items[0].Tombstone
	if ts.State != "purged" || ts.Reason != "секрет у вмісті" || ts.By.ActorID != ada || ts.At.IsZero() || rd.Items[0].Body != nil {
		t.Errorf("tombstone: %+v", ts)
	}

	// --- a number query: the erased record in entity, then who mentions it -----------------------------------------
	sr, err := q.Search(ctx, me, query.SearchIn{Query: target.Ref, Limit: 20})
	must(t, "search purged number", err)
	if sr.Entity == nil || sr.Entity.State != "purged" || sr.Entity.Title != "" || sr.Entity.Tombstone == nil ||
		sr.Entity.ID != target.StatementID || len(sr.Items) == 0 || sr.Items[0].StatementID != mention.StatementID ||
		len(sr.MentionedBy) != 1 || sr.MentionedBy[0].Ref != task.Ref {
		t.Errorf("number of a purged record: entity %+v, %d items, by %+v", sr.Entity, len(sr.Items), sr.MentionedBy)
	}

	// --- links: the erased end is named with its state ---------------------------------------------------------------
	rm, err := q.Read(ctx, me, []uuid.UUID{mention.StatementID}, false)
	must(t, "read mention", err)
	state := ""
	for _, l := range rm.Items[0].Links {
		if l.Relation == "mentions" && l.To["entity_id"] == target.StatementID {
			state, _ = l.To["state"].(string)
		}
	}
	if state != "purged" {
		t.Errorf("link to a purged record: state %q, links %+v", state, rm.Items[0].Links)
	}

	// --- impact after the erasure: the same dependants, the target in its state ------------------------------------
	im, err = q.Impact(ctx, me, "statement", target.StatementID)
	must(t, "impact purged", err)
	if im.Target.State == nil || *im.Target.State != "purged" || len(im.Incoming) != 1 || im.Incoming[0].Count != 2 {
		t.Errorf("impact of a purged record (no cascade): %+v", im)
	}

	// nothing of the above carries the erased title
	for name, out := range map[string]any{"read": rd, "search": sr, "links": rm, "impact": im} {
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), "Zebra") {
			t.Errorf("%s leaks the erased title: %s", name, raw)
		}
	}

	// --- a retracted task answers its number with its state; the impact of a project counts what it holds ---------
	_, err = v.TaskRetract(ctx, sp, task.TaskID, "duplicate")
	must(t, "retract task", err)
	sr, err = q.Search(ctx, me, query.SearchIn{Query: task.Ref, Limit: 20})
	must(t, "search retracted task", err)
	if sr.Entity == nil || sr.Entity.State != "retracted" || sr.Entity.ID != task.TaskID {
		t.Errorf("number of a retracted task: %+v", sr.Entity)
	}

	// --- a purged task (0033): its number answers with the tombstone, lists drop it, the row keeps no content ------
	leak, err := v.TaskDeclare(ctx, sp, verbs.TaskDeclareIn{Project: "vault", Fields: map[string]any{"title": "Ключ Orca-Lime",
		"body": "токен у тілі", "labels": []any{"orca"}, "priority": "high"}})
	must(t, "task to purge", err)
	_, err = v.JournalPurge(ctx, sp, "task", leak.TaskID, "секрет у вмісті", nil)
	must(t, "purge task", err)
	sr, err = q.Search(ctx, me, query.SearchIn{Query: leak.Ref, Limit: 20})
	must(t, "search purged task", err)
	if sr.Entity == nil || sr.Entity.State != "purged" || sr.Entity.Title != "" || sr.Entity.Tombstone == nil ||
		sr.Entity.Tombstone.Reason != "секрет у вмісті" {
		t.Errorf("number of a purged task: %+v", sr.Entity)
	}
	var rest string
	must(t, "purged task row", app.QueryRow(sys, `SELECT concat_ws('|', title, body, labels::text, priority, purged::text, hidden::text)
	        FROM projection.task WHERE task_id = $1`, leak.TaskID).Scan(&rest))
	if rest != "[purged]|{}|high|true|true" {
		t.Errorf("purged task row: %s", rest)
	}
	if _, err := v.TaskAmend(ctx, sp, verbs.TaskAmendIn{TaskID: leak.TaskID, Fields: map[string]any{"title": "again"}}); verbs.Code(err) != "task_not_found" {
		t.Errorf("amend of a purged task: %v", err)
	}
	if _, err := v.JournalPurge(ctx, sp, "workflow", uuid.New(), "x", nil); verbs.Code(err) != "invalid_input" {
		t.Errorf("purge of a type the projector does not erase: %v", err)
	}

	var project uuid.UUID
	must(t, "project", app.QueryRow(sys, `SELECT project_id FROM projection.project WHERE name = 'vault'`).Scan(&project))
	im, err = q.Impact(ctx, me, "project", project)
	must(t, "impact project", err)
	if im.Contents == nil || im.Contents.Statements != 1 || im.Contents.Tasks != 0 || im.Target.Ref == nil || *im.Target.Ref != "vault" {
		t.Errorf("impact of a project: %+v %+v", im.Target, im.Contents)
	}
}
