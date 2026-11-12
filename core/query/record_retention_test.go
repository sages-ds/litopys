package query_test

import (
	"context"
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

// TestRecordRetention (0043): the retention of one record is set on assert or by
// statement.retain, checked when set (a date not in the past, with its action), shown by recall_read and not a new
// version; an agent schedules retract only on its own records and never changes what a human set; on its day the
// worker retracts (retract) or opens one deletion proposal for a human's GO (purge) — not again after a rejection,
// again after a new setting; a lifted retention does nothing; a rebuild gives the same.
func TestRecordRetention(t *testing.T) {
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
	agent, _, err := v.ActorRegister(ctx, sp, verbs.ActorRegisterIn{Kind: "agent", DisplayName: "helper", ExternalIdentity: ptr("agent:helper")})
	must(t, "agent", err)
	bot := verbs.Principal{TenantID: tenant, ActorID: agent}

	zone, err := time.LoadLocation(verbs.RetentionZone)
	must(t, "zone", err)
	now := time.Now()
	day := func(d int) string { return now.In(zone).AddDate(0, 0, d).Format(time.DateOnly) }

	write := func(p verbs.Principal, title string, until, action *string) (uuid.UUID, error) {
		t.Helper()
		open, err := v.ContextOpen(ctx, p, verbs.ContextOpenIn{Project: "keep", Attach: "none", AutoCreate: true})
		must(t, "open", err)
		p.RunID = open.RunID
		out, err := v.StatementAssert(ctx, p, verbs.StatementAssertIn{Project: "keep", Type: "note", Title: title,
			Body: ptr("body of " + title), ContextID: &open.ContextID, RetainUntil: until, OnExpiry: action})
		return out.StatementID, err
	}
	retain := func(p verbs.Principal, id uuid.UUID, until string, action *string) (verbs.Ack, error) {
		return v.StatementRetain(ctx, p, verbs.StatementRetainIn{StatementID: id, RetainUntil: until, OnExpiry: action})
	}
	read := func(id uuid.UUID) query.FullRow {
		t.Helper()
		out, err := q.Read(ctx, query.Principal{TenantID: tenant, ActorID: ada}, []uuid.UUID{id}, false)
		must(t, "read", err)
		if len(out.Items) != 1 {
			t.Fatalf("read %s: %d items", id, len(out.Items))
		}
		return out.Items[0]
	}
	pass := func(at time.Time, want int) {
		t.Helper()
		n, err := v.ExecuteRetention(ctx, tenant, at)
		must(t, "retention pass", err)
		if n != want {
			t.Errorf("retention pass at %s: %d, want %d", at.In(zone).Format(time.DateOnly), n, want)
		}
	}
	statusOf := func(id uuid.UUID) string {
		t.Helper()
		var st string
		must(t, "status", app.QueryRow(sys, `SELECT status FROM projection.statement WHERE statement_id = $1`, id).Scan(&st))
		return st
	}
	proposals := func(id uuid.UUID) (open, all int) {
		t.Helper()
		must(t, "proposals", app.QueryRow(sys, `SELECT count(*) FILTER (WHERE state = 'open'), count(*) FROM projection.deletion
		        WHERE target_id = $1 AND action = 'purge'`, id).Scan(&open, &all))
		return
	}

	// --- checked when set --------------------------------------------------------------------------------------------
	if _, err := write(sp, "past", ptr(day(-1)), ptr("retract")); verbs.Code(err) != "retain_until_past" {
		t.Errorf("a date in the past: %v", err)
	}
	if _, err := write(sp, "no action", ptr(day(3)), nil); verbs.Code(err) != "invalid_input" {
		t.Errorf("a date without on_expiry: %v", err)
	}
	if _, err := write(sp, "archive", ptr(day(3)), ptr("archive")); verbs.Code(err) != "invalid_input" {
		t.Errorf("on_expiry archive: %v", err)
	}
	if _, err := write(sp, "bad date", ptr("31.12.2030"), ptr("retract")); verbs.Code(err) != "invalid_input" {
		t.Errorf("a date not YYYY-MM-DD: %v", err)
	}

	// --- on assert, shown by recall_read -----------------------------------------------------------------------------
	gone, err := write(sp, "Чернетка на три дні", ptr(day(3)), ptr("retract"))
	must(t, "assert with retention", err)
	if r := read(gone); r.Retention == nil || r.Retention.RetainUntil != day(3) || r.Retention.OnExpiry != "retract" || r.Revision != 1 {
		t.Fatalf("read after assert: %+v rev %d", r.Retention, r.Revision)
	}

	// --- statement.retain: an agent and a human ---------------------------------------------------------------------
	mine, err := write(bot, "Нотатка агента", nil, nil)
	must(t, "agent's record", err)
	theirs, err := write(sp, "Запис людини", nil, nil)
	must(t, "human's record", err)
	if _, err := retain(bot, mine, day(2), ptr("retract")); err != nil {
		t.Errorf("an agent schedules retract on its own record: %v", err)
	}
	if _, err := retain(bot, theirs, day(2), ptr("retract")); verbs.Code(err) != "retract_not_own" {
		t.Errorf("an agent schedules retract on a human's record: %v", err)
	}
	if _, err := retain(bot, theirs, day(2), ptr("purge")); err != nil {
		t.Errorf("an agent schedules a purge proposal (it waits for a GO): %v", err)
	}
	if _, err := retain(bot, gone, "", nil); verbs.Code(err) != "retention_set_by_human" {
		t.Errorf("an agent lifts what a human set: %v", err)
	}
	if _, err := retain(sp, gone, "", ptr("retract")); verbs.Code(err) != "invalid_input" {
		t.Errorf("lifting with an action: %v", err)
	}
	if a, err := retain(sp, gone, day(3), ptr("retract")); err != nil || !a.Already {
		t.Errorf("the same retention again: %+v %v", a, err)
	}
	if r := read(mine); r.Retention == nil || r.Revision != 1 {
		t.Errorf("a retention is not a new version: %+v rev %d", r.Retention, r.Revision)
	}
	lifted, err := write(sp, "Знятий термін", ptr(day(1)), ptr("purge"))
	must(t, "to lift", err)
	if a, err := retain(sp, lifted, "", nil); err != nil || a.Already {
		t.Fatalf("lift: %+v %v", a, err)
	}
	if read(lifted).Retention != nil {
		t.Error("a lifted retention is still shown")
	}

	// --- the worker: nothing before the day, then once ---------------------------------------------------------------
	pass(now, 0)
	pass(now.AddDate(0, 0, 2), 2) // mine retracted, a proposal for theirs
	if st := statusOf(mine); st != "retracted" {
		t.Errorf("own record after its day: %s", st)
	}
	if st := statusOf(theirs); st != "current" {
		t.Errorf("purge on expiry must not erase or retract by itself: %s", st)
	}
	if open, _ := proposals(theirs); open != 1 {
		t.Fatalf("open purge proposals for the human's record: %d, want 1", open)
	}
	var reason string
	must(t, "reason", app.QueryRow(sys, `SELECT reason FROM projection.deletion WHERE target_id = $1`, theirs).Scan(&reason))
	if want := "retention: "; len(reason) < len(want) || reason[:len(want)] != want {
		t.Errorf("proposal reason %q", reason)
	}
	pass(now.AddDate(0, 0, 3), 1) // gone retracted; no second proposal for theirs
	if st := statusOf(gone); st != "retracted" {
		t.Errorf("record on its day: %s", st)
	}
	if _, all := proposals(theirs); all != 1 {
		t.Errorf("a second proposal for the same setting: %d", all)
	}

	// a rejected proposal is not reopened; a new setting opens one again
	var d uuid.UUID
	must(t, "proposal id", app.QueryRow(sys, `SELECT deletion_id FROM projection.deletion WHERE target_id = $1`, theirs).Scan(&d))
	_, err = v.DeletionReject(ctx, sp, d, "keep it")
	must(t, "reject", err)
	pass(now.AddDate(0, 0, 4), 0)
	_, err = retain(sp, theirs, day(5), ptr("purge"))
	must(t, "new setting", err)
	pass(now.AddDate(0, 0, 5), 1)
	if open, all := proposals(theirs); open != 1 || all != 2 {
		t.Errorf("after a new setting: open %d, all %d", open, all)
	}

	// --- review cases --------------------------------------------------------------------------------------------------
	// a retract-retention is used once: reinstated after the worker retracted it, the record stays
	_, err = v.StatementReinstate(ctx, sp, mine, ptr("still needed"))
	must(t, "reinstate", err)
	pass(now.AddDate(0, 0, 6), 0)
	if st := statusOf(mine); st != "current" {
		t.Errorf("a reinstated record was retracted again by a used retention: %s", st)
	}
	// an agent changes only a retention it set itself; it does not supersede around a human's
	other, _, err := v.ActorRegister(ctx, sp, verbs.ActorRegisterIn{Kind: "agent", DisplayName: "other", ExternalIdentity: ptr("agent:other")})
	must(t, "second agent", err)
	bot2 := verbs.Principal{TenantID: tenant, ActorID: other}
	byBot, err := write(bot, "Термін агента", ptr(day(9)), ptr("purge"))
	must(t, "agent's retention", err)
	if _, err := retain(bot2, byBot, day(10), ptr("purge")); verbs.Code(err) != "retention_set_by_another" {
		t.Errorf("an agent re-dates another agent's retention: %v", err)
	}
	if _, err := retain(bot, byBot, day(10), ptr("purge")); err != nil {
		t.Errorf("an agent re-dates its own retention: %v", err)
	}
	keptByHuman, err := write(bot, "Агент пише, людина тримає", nil, nil)
	must(t, "agent's record kept by a human", err)
	_, err = retain(sp, keptByHuman, day(9), ptr("retract"))
	must(t, "human's retention on it", err)
	if _, err := v.StatementSupersede(ctx, bot, keptByHuman, verbs.StatementAssertIn{Project: "keep", Type: "note",
		Title: "Заміна без терміну"}, nil); verbs.Code(err) != "retention_set_by_human" {
		t.Errorf("an agent supersedes around a human's retention: %v", err)
	}
	// a deduped assert does not set a retention, and says so
	ext := "retain-dedup"
	first, err := v.StatementAssert(ctx, sp, verbs.StatementAssertIn{Project: "keep", Type: "note", Title: "Дубль", ExternalRef: &ext})
	must(t, "first", err)
	again, err := v.StatementAssert(ctx, sp, verbs.StatementAssertIn{Project: "keep", Type: "note", Title: "Дубль", ExternalRef: &ext,
		RetainUntil: ptr(day(9)), OnExpiry: ptr("retract")})
	must(t, "deduped", err)
	if !again.Deduped || len(again.Warnings) == 0 || read(first.StatementID).Retention != nil {
		t.Errorf("a deduped assert with a retention: %+v", again)
	}
	// a purge on expiry with a GO: the record is erased, the retention goes with it (live = replay, E7)
	erased, err := write(sp, "Стерти за терміном", ptr(day(7)), ptr("purge"))
	must(t, "to erase", err)
	pass(now.AddDate(0, 0, 7), 1)
	var dl uuid.UUID
	must(t, "erase proposal", app.QueryRow(sys, `SELECT deletion_id FROM projection.deletion WHERE target_id = $1 AND state = 'open'`, erased).Scan(&dl))
	_, err = v.DeletionApprove(ctx, sp, dl, map[string]int{}, ptr("так"))
	must(t, "approve purge", err)
	_, err = v.ExecutePurges(ctx, tenant)
	must(t, "execute purges", err)
	var purged bool
	var until *string
	must(t, "erased row", app.QueryRow(sys, `SELECT purged, to_char(retain_until, 'YYYY-MM-DD') FROM projection.statement WHERE statement_id = $1`,
		erased).Scan(&purged, &until))
	if !purged || until != nil {
		t.Errorf("an erased record keeps its retention: purged %v, retain_until %v", purged, until)
	}
	if _, err := retain(sp, erased, day(9), ptr("retract")); verbs.Code(err) != "already_purged" {
		t.Errorf("a retention on an erased record: %v", err)
	}
	// a revoked setter skips its record, the others go on
	revokedBy, _, err := v.ActorRegister(ctx, sp, verbs.ActorRegisterIn{Kind: "agent", DisplayName: "gone", ExternalIdentity: ptr("agent:gone")})
	must(t, "agent to revoke", err)
	gonePrincipal := verbs.Principal{TenantID: tenant, ActorID: revokedBy}
	orphan, err := write(gonePrincipal, "Термін відкликаного агента", ptr(day(8)), ptr("retract"))
	must(t, "revoked agent's record", err)
	live, err := write(sp, "Термін живої людини", ptr(day(8)), ptr("retract"))
	must(t, "live record", err)
	_, err = v.ActorRevoke(ctx, sp, revokedBy, "test")
	must(t, "revoke", err)
	n, err := v.ExecuteRetention(ctx, tenant, now.AddDate(0, 0, 8))
	if n != 1 || err == nil || statusOf(live) != "retracted" || statusOf(orphan) != "current" {
		t.Errorf("a revoked setter: done %d, err %v, live %s, orphan %s", n, err, statusOf(live), statusOf(orphan))
	}

	// --- replay ------------------------------------------------------------------------------------------------------
	snapshot := func() string {
		var s string
		must(t, "snapshot", app.QueryRow(sys, `SELECT string_agg(statement_id || ' ' || COALESCE(retain_until::text, '-') || ' ' ||
		        COALESCE(on_expiry, '-') || ' ' || COALESCE(retain_by::text, '-') || ' ' || COALESCE(retain_seq::text, '-') || ' ' || status,
		        ',' ORDER BY statement_id) FROM projection.statement`).Scan(&s))
		return s
	}
	before := snapshot()
	_, err = projector.Rebuild(ctx, app, tenant)
	must(t, "rebuild", err)
	if after := snapshot(); after != before {
		t.Errorf("rebuild changed the retention:\n%s\n%s", before, after)
	}
}
