package query_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestRetention: without a label nothing goes; a label is checked when it is set and an erasing one
// only a human sets; the worker's pass retracts or erases what is older, as the owner's automation with the reason
// "retention: <project>/<key>", exactly once; retain_until acts from its day on what was there before it.
func TestRetention(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	v := &verbs.Service{DB: app, Admin: db.RolePool(t, "litopys_admin", 1)}
	var tenant uuid.UUID
	must(t, "tenant", app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant))
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must(t, "human", err)
	sp := verbs.Principal{TenantID: tenant, ActorID: ada}
	agent, _, err := v.ActorRegister(ctx, sp, verbs.ActorRegisterIn{Kind: "agent", DisplayName: "helper", ExternalIdentity: ptr("agent:helper")})
	must(t, "agent", err)
	bot := verbs.Principal{TenantID: tenant, ActorID: agent}

	write := func(project, typ, title string) uuid.UUID {
		t.Helper()
		open, err := v.ContextOpen(ctx, sp, verbs.ContextOpenIn{Project: project, Attach: "none", AutoCreate: true})
		must(t, "open", err)
		p := sp
		p.RunID = open.RunID
		out, err := v.StatementAssert(ctx, p, verbs.StatementAssertIn{Project: project, Type: typ, Title: title,
			Body: ptr("body of " + title), ContextID: &open.ContextID})
		must(t, "assert "+title, err)
		return out.StatementID
	}
	label := func(p verbs.Principal, project, key, value string) error {
		_, err := v.FactRecord(ctx, p, verbs.FactRecordIn{Project: project, Kind: "retention", Key: key, Value: &value})
		return err
	}
	status := func(id uuid.UUID) (s string, purged bool) {
		t.Helper()
		must(t, "status", app.QueryRow(sys, `SELECT status, purged FROM projection.statement WHERE statement_id = $1`, id).Scan(&s, &purged))
		return
	}
	pass := func(at time.Time, want int) {
		t.Helper()
		n, err := v.ExecuteRetention(ctx, tenant, at)
		must(t, "retention pass", err)
		if n != want {
			t.Errorf("retention pass at %s: %d, want %d", at.Format(time.DateOnly), n, want)
		}
	}
	now := time.Now()
	note := write("vault", "note", "Чернетка Quillmarsh")
	decision := write("vault", "decision", "Рішення лишається")
	other := write("keep", "note", "Без мітки")

	// --- no label: nothing goes, however old -------------------------------------------------------------------------
	pass(now.AddDate(1, 0, 0), 0)

	// --- a label is checked when it is set; an erasing one only a human sets ----------------------------------------
	for _, bad := range []string{`not json`, `{"applies_to":"note","action":"retract"}`,
		`{"applies_to":"note","after_days":30,"retain_until":"2030-01-01","action":"retract"}`,
		`{"applies_to":"notes","after_days":30,"action":"retract"}`, `{"applies_to":"note","after_days":30,"action":"archive"}`,
		`{"applies_to":"note","after_days":30,"action":"retract","extra":1}`} {
		if err := label(sp, "vault", "bad", bad); verbs.Code(err) != "invalid_input" {
			t.Errorf("label %s: %v", bad, err)
		}
	}
	if err := label(bot, "vault", "erase", `{"applies_to":"record","after_days":60,"action":"purge"}`); verbs.Code(err) != "retention_purge_requires_human" {
		t.Errorf("an agent sets an erasing label: %v", err)
	}
	if err := label(bot, "vault", "all", `{"applies_to":"record","after_days":60,"action":"retract"}`); verbs.Code(err) != "retention_record_requires_human" {
		t.Errorf("an agent labels every record of the project: %v", err)
	}
	must(t, "agent's retract label", label(bot, "vault", "notes", `{"applies_to":"note","after_days":30,"action":"retract"}`))

	// --- retract: what is older than after_days, of the label's type, once ------------------------------------------
	pass(now, 0)
	pass(now.AddDate(0, 0, 31), 1)
	if s, _ := status(note); s != "retracted" {
		t.Errorf("the old note is %s", s)
	}
	if s, _ := status(decision); s != "current" {
		t.Errorf("a decision under a note label is %s", s)
	}
	if s, _ := status(other); s != "current" {
		t.Errorf("a record of a project without a label is %s", s)
	}
	pass(now.AddDate(0, 0, 31), 0)
	var kind, identity, reason string
	var behalf uuid.UUID
	must(t, "retract event", app.QueryRow(sys, `SELECT a.kind, a.external_identity, a.on_behalf_of, j.payload->>'reason'
	        FROM journal j JOIN projection.actor a ON a.actor_id = j.actor_id
	       WHERE j.type = 'statement.retracted' AND j.entity_id = $1`, note).Scan(&kind, &identity, &behalf, &reason))
	if kind != "automation" || identity != "litopys:retention" || behalf != ada || reason != "retention: vault/notes" {
		t.Errorf("the retract: %s %s on behalf of %s, reason %q", kind, identity, behalf, reason)
	}

	// --- a human-only label is proposed until the person confirms that very version in the viewer -----
	erase := `{"applies_to":"record","after_days":60,"action":"purge"}`
	ack, err := v.FactRecord(ctx, sp, verbs.FactRecordIn{Project: "vault", Kind: "retention", Key: "erase", Value: &erase})
	must(t, "human's erase label", err)
	if len(ack.Warnings) != 1 || ack.Warnings[0][:len(verbs.NeedsConfirmation)] != verbs.NeedsConfirmation {
		t.Errorf("fact.record of a human-only label: warnings %v", ack.Warnings)
	}
	at := now.AddDate(0, 0, 61)
	pass(at, 0) // proposed: the worker does not apply it, whoever wrote it
	l, err := v.RetentionLabelOf(ctx, sp, "vault", "erase", at)
	must(t, "label", err)
	if !l.HumanOnly || l.Confirmed != nil || l.Impact != (verbs.RetentionImpact{Records: 2}) {
		t.Fatalf("label to confirm: %+v", l)
	}
	if _, err := v.FactConfirm(ctx, bot, "vault", "erase", l.RecordEventID, l.Impact, at); verbs.Code(err) == "" {
		t.Error("an agent confirmed a human-only label")
	}
	if _, err := v.FactConfirm(ctx, sp, "vault", "erase", l.RecordEventID, verbs.RetentionImpact{Records: 1}, at); verbs.Code(err) != "impact_changed" {
		t.Errorf("a confirmation of another impact: %v", err)
	}
	must(t, "confirm", func() error {
		_, err := v.FactConfirm(ctx, sp, "vault", "erase", l.RecordEventID, l.Impact, at)
		return err
	}())
	// written again — even with the same value — the label is proposed again and the old page is refused
	must(t, "re-record", label(sp, "vault", "erase", erase))
	if _, err := v.FactConfirm(ctx, sp, "vault", "erase", l.RecordEventID, l.Impact, at); verbs.Code(err) != "label_changed" {
		t.Errorf("a confirmation of an older version: %v", err)
	}
	pass(at, 0)
	l, err = v.RetentionLabelOf(ctx, sp, "vault", "erase", at)
	must(t, "label again", err)
	must(t, "confirm again", func() error {
		_, err := v.FactConfirm(ctx, sp, "vault", "erase", l.RecordEventID, l.Impact, at)
		return err
	}())
	if again, err := v.FactConfirm(ctx, sp, "vault", "erase", l.RecordEventID, l.Impact, at); err != nil || !again.Already {
		t.Errorf("a second confirmation of the same version: %+v %v", again, err)
	}

	// --- purge: a human's confirmed label erases what is older, retracted or not; the content leaves the journal ------
	pass(at, 2)
	for _, id := range []uuid.UUID{note, decision} {
		if _, purged := status(id); !purged {
			t.Errorf("%s not erased", id)
		}
	}
	pass(now.AddDate(0, 0, 61), 0)
	var leaks int
	must(t, "journal", app.QueryRow(sys, `SELECT count(*) FROM journal WHERE payload::text LIKE '%Quillmarsh%'`).Scan(&leaks))
	if leaks != 0 {
		t.Errorf("the erased title is still in %d journal payloads", leaks)
	}

	// --- tasks: closed ones only; retain_until acts from its day ----------------------------------------------------
	_, err = v.ContextOpen(ctx, sp, verbs.ContextOpenIn{Project: "work", Attach: "none", AutoCreate: true})
	must(t, "open work", err)
	closed, err := v.TaskDeclare(ctx, sp, verbs.TaskDeclareIn{Project: "work", Fields: map[string]any{"title": "Зроблено"}})
	must(t, "task", err)
	_, err = v.TaskClose(ctx, sp, closed.TaskID, "done", nil)
	must(t, "close", err)
	open, err := v.TaskDeclare(ctx, sp, verbs.TaskDeclareIn{Project: "work", Fields: map[string]any{"title": "Ще в роботі"}})
	must(t, "open task", err)
	until := now.AddDate(0, 0, 2).Format(time.DateOnly)
	must(t, "task label", label(sp, "work", "closed", `{"applies_to":"task","retain_until":"`+until+`","action":"retract"}`))
	pass(now, 0)
	pass(now.AddDate(0, 0, 3), 1)
	var closedGone, openGone bool
	must(t, "tasks", app.QueryRow(sys, `SELECT (SELECT retracted FROM projection.task WHERE task_id = $1),
	        (SELECT retracted FROM projection.task WHERE task_id = $2)`, closed.TaskID, open.TaskID).Scan(&closedGone, &openGone))
	if !closedGone || openGone {
		t.Errorf("tasks after retain_until: closed retracted %v, open retracted %v", closedGone, openGone)
	}

	// --- a retired label does nothing ------------------------------------------------------------------------------
	late := write("vault", "note", "Пізня нотатка")
	_, err = v.FactRetire(ctx, sp, "vault", "retention", "notes", nil)
	must(t, "retire label", err)
	_, err = v.FactRetire(ctx, sp, "vault", "retention", "erase", nil)
	must(t, "retire erase label", err)
	pass(now.AddDate(1, 0, 0), 0)
	if s, purged := status(late); s != "current" || purged {
		t.Errorf("a record after the labels were retired: %s purged=%v", s, purged)
	}
}
