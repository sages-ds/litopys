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

// TestDeletionProposals: an agent proposes what is not its own to remove; only a human decides; the
// GO is given on a concrete impact and a changed one is refused; an approved retract is done at once and an approved
// erase by the worker, exactly once; a quarantine of a secret proposes the erase by itself.
func TestDeletionProposals(t *testing.T) {
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

	write := func(p verbs.Principal, title string, mentions ...verbs.EntityRef) verbs.StatementAssertOut {
		t.Helper()
		open, err := v.ContextOpen(ctx, p, verbs.ContextOpenIn{Project: "vault", Attach: "none", AutoCreate: true})
		must(t, "open", err)
		p.RunID = open.RunID
		out, err := v.StatementAssert(ctx, p, verbs.StatementAssertIn{Project: "vault", Type: "note", Title: title,
			Body: ptr("body of " + title), ContextID: &open.ContextID, Mentions: mentions})
		must(t, "assert "+title, err)
		return out
	}
	old := write(sp, "Стара домовленість")
	oldRef := verbs.EntityRef{EntityType: "statement", EntityID: old.StatementID}
	write(sp, "Посилається на стару", oldRef)

	// --- an agent does not retract someone else's record; it proposes ----------------------------------------------
	if _, err := v.StatementRetract(ctx, bot, old.StatementID, ptr("застаріло")); verbs.Code(err) != "retract_not_own" {
		t.Fatalf("an agent retracts a human's record: %v", err)
	}
	mine := write(bot, "Чернетка агента")
	if _, err := v.StatementRetract(ctx, bot, mine.StatementID, nil); err != nil {
		t.Errorf("an agent retracts its own record: %v", err)
	}
	d1, err := v.DeletionPropose(ctx, bot, oldRef, "retract", "застаріло", false)
	must(t, "propose", err)
	if d1.Ref != "vault/D-1" || d1.State != "open" || d1.Impact["mentions"] != 1 {
		t.Fatalf("proposal: %+v", d1)
	}
	again, err := v.DeletionPropose(ctx, bot, oldRef, "retract", "ще раз", false)
	must(t, "propose again", err)
	if !again.Already || again.DeletionID != d1.DeletionID {
		t.Errorf("a repeated proposal: %+v", again)
	}
	open, err := q.WorkOpenItems(ctx, me, "vault")
	must(t, "open items", err)
	if len(open.Deletions) != 1 || open.Deletions[0].Ref != "vault/D-1" || open.Deletions[0].Target.Ref == nil {
		t.Errorf("open items: deletions %+v", open.Deletions)
	}

	// --- only a human decides; the GO is on the impact the user saw -------------------------------------------------
	if _, err := v.DeletionApprove(ctx, bot, d1.DeletionID, nil, nil); verbs.Code(err) != "deletion_requires_human" {
		t.Errorf("an agent approves: %v", err)
	}
	write(sp, "Ще одна згадка", oldRef) // the impact grows after the proposal
	_, err = v.DeletionApprove(ctx, sp, d1.DeletionID, nil, nil)
	if verbs.Code(err) != "impact_changed" || !strings.Contains(err.Error(), `"mentions":2`) {
		t.Fatalf("approve on a changed impact: %v", err)
	}
	ok, err := v.DeletionApprove(ctx, sp, d1.DeletionID, map[string]int{"mentions": 2}, ptr("так, відклич"))
	must(t, "approve on the new impact", err)
	if ok.State != "done" {
		t.Errorf("an approved retract: %+v", ok)
	}
	var status string
	must(t, "status", app.QueryRow(sys, `SELECT status FROM projection.statement WHERE statement_id = $1`, old.StatementID).Scan(&status))
	if status != "retracted" {
		t.Errorf("the approved retract left the record %s", status)
	}
	if _, err := v.DeletionApprove(ctx, sp, d1.DeletionID, nil, nil); verbs.Code(err) != "deletion_closed" {
		t.Errorf("a second GO: %v", err)
	}

	// --- reject: nothing changes ------------------------------------------------------------------------------------
	keep := write(sp, "Лишити")
	d2, err := v.DeletionPropose(ctx, bot, verbs.EntityRef{EntityType: "statement", EntityID: keep.StatementID}, "purge", "дубль", false)
	must(t, "propose purge", err)
	_, err = v.DeletionReject(ctx, sp, d2.DeletionID, "не дубль")
	must(t, "reject", err)
	if n, err := v.ExecutePurges(ctx, tenant); err != nil || n != 0 {
		t.Errorf("worker after a rejection: %d, %v", n, err)
	}

	// --- a quarantine of a secret proposes the erase; the GO waits for the worker; the worker erases once -----------
	leak := write(sp, "Пароль Heronlock")
	leakRef := verbs.EntityRef{EntityType: "statement", EntityID: leak.StatementID}
	_, err = v.JournalQuarantine(ctx, bot, leakRef, "секрет у назві", true)
	must(t, "quarantine secret", err)
	dl, err := q.DeletionList(ctx, me, query.DeletionListIn{Project: "vault"})
	must(t, "list", err)
	var d3 *query.DeletionRow
	for i := range dl.Items {
		if dl.Items[i].Target.ID == leak.StatementID {
			d3 = &dl.Items[i]
		}
	}
	if d3 == nil || d3.Action != "purge" || !d3.Secret || d3.State != "open" || d3.ProposedBy.ActorID != agent ||
		d3.Target.State == nil || *d3.Target.State != "quarantined" {
		t.Fatalf("the proposal of a quarantined secret: %+v (list %+v)", d3, dl.Items)
	}
	ap, err := v.DeletionApprove(ctx, sp, d3.DeletionID, nil, nil)
	must(t, "approve purge", err)
	if ap.State != "approved" {
		t.Errorf("an approved erase before the worker: %+v", ap)
	}
	n, err := v.ExecutePurges(ctx, tenant)
	must(t, "worker", err)
	if n != 1 {
		t.Errorf("worker executed %d", n)
	}
	if n, err := v.ExecutePurges(ctx, tenant); err != nil || n != 0 {
		t.Errorf("worker, second pass: %d, %v", n, err)
	}
	rd, err := q.Read(ctx, me, []uuid.UUID{leak.StatementID}, false)
	must(t, "read erased", err)
	if !rd.Items[0].Purged || rd.Items[0].Tombstone == nil || rd.Items[0].Tombstone.State != "purged" || rd.Items[0].Tombstone.By.ActorID != ada {
		t.Errorf("the erased record: %+v", rd.Items[0])
	}
	done, err := q.DeletionList(ctx, me, query.DeletionListIn{Project: "vault", State: "done"})
	must(t, "list done", err)
	if len(done.Items) != 2 {
		t.Errorf("done proposals: %+v", done.Items)
	}
	var leaks int
	must(t, "journal", app.QueryRow(sys, `SELECT count(*) FROM journal WHERE payload::text LIKE '%Heronlock%'`).Scan(&leaks))
	if leaks != 0 {
		t.Errorf("the erased title is still in %d journal payloads", leaks)
	}
}
