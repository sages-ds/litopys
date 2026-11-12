package verbs_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/journal"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/projector"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/core/vocab"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestFamilies: every record has a family — an event without one takes it from the legacy
// map, an event that carries one keeps it, a new type needs no migration; an amended type moves the family; a rebuild
// gives the same; a vocabulary fact names a built-in vocabulary and on | off.
func TestFamilies(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	s := &verbs.Service{DB: app}
	var tenant uuid.UUID
	must(t, "tenant", app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant))
	ada, _, err := s.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must(t, "human", err)
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	project, _, err := s.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "litopys-test"})
	must(t, "project", err)

	family := func(id uuid.UUID) (typ, fam string) {
		t.Helper()
		must(t, "family", app.QueryRow(sys, `SELECT type, family FROM projection.statement WHERE statement_id = $1`, id).Scan(&typ, &fam))
		return
	}
	ids := map[string]uuid.UUID{}
	for _, typ := range []string{"bugfix", "discovery", "config", "decision", "question"} {
		st, err := s.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "litopys-test", Type: typ, Title: "a " + typ})
		must(t, "assert "+typ, err)
		ids[typ] = st.StatementID
	}
	for typ, want := range map[string]string{"bugfix": "event", "discovery": "finding", "config": "event", "decision": "decision", "question": "question"} {
		if _, got := family(ids[typ]); got != want {
			t.Errorf("%s: family %s, want %s", typ, got, want)
		}
	}
	_, err = s.StatementAmend(ctx, me, verbs.StatementAmendIn{StatementID: ids["bugfix"], Type: ptr("decision")})
	must(t, "amend type", err)
	if typ, fam := family(ids["bugfix"]); typ != "decision" || fam != "decision" {
		t.Errorf("amended type: %s / %s", typ, fam)
	}

	// an event of a vocabulary type that carries its family: no CHECK stops it, the family is kept as given
	newType := uuid.New()
	must(t, "journal", pg.InTx(sys, app, func(tx pgx.Tx) error {
		payload, _ := json.Marshal(map[string]any{"project_id": project, "type": "dba.migration", "family": "event", "title": "0038 applied"})
		e := &journal.Event{TenantID: tenant, Type: "statement.asserted", ActorID: ada, RunID: ada, CausationID: ada, CorrelationID: ada,
			EntityType: "statement", EntityID: newType, Payload: payload}
		if err := journal.Append(sys, tx, e); err != nil {
			return err
		}
		return projector.Apply(sys, tx, e)
	}))
	if typ, fam := family(newType); typ != "dba.migration" || fam != "event" {
		t.Errorf("a vocabulary type: %s / %s", typ, fam)
	}

	dump := func() []string {
		rows, err := app.Query(sys, `SELECT statement_id || ' ' || type || ' ' || family FROM projection.statement ORDER BY 1`)
		must(t, "dump", err)
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		must(t, "dump rows", err)
		return out
	}
	before := dump()
	if _, err := projector.Rebuild(ctx, app, tenant); err != nil {
		t.Fatal(err)
	}
	if after := dump(); !reflect.DeepEqual(before, after) {
		t.Errorf("rebuild changed families:\n%v\n%v", before, after)
	}

	// vocabulary facts
	_, err = s.FactRecord(ctx, me, verbs.FactRecordIn{Project: "litopys-test", Kind: "vocabulary", Key: "dba", Value: ptr("on")})
	must(t, "vocabulary on", err)
	for _, bad := range []verbs.FactRecordIn{
		{Project: "litopys-test", Kind: "vocabulary", Key: "general", Value: ptr("on")},
		{Project: "litopys-test", Kind: "vocabulary", Key: "nope", Value: ptr("on")},
		{Project: "litopys-test", Kind: "vocabulary", Key: "dba", Value: ptr("maybe")},
		{Project: "litopys-test", Kind: "vocabulary", Key: "dba"},
	} {
		_, err := s.FactRecord(ctx, me, bad)
		wantCode(t, "vocabulary fact "+bad.Key, err, "invalid_input")
	}
}

// TestTypeResolution: the type of a record is resolved against the project's
// set of vocabularies — the stack global → spaces → project of the owner's vocabulary facts; an agent's fact does
// not count; short names, full names, ambiguity, the legacy alias with a warning; the facade keeps its old list.
func TestTypeResolution(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	s := &verbs.Service{DB: app}
	var tenant uuid.UUID
	must(t, "tenant", app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant))
	ada, _, err := s.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must(t, "human", err)
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	agent, _, err := s.ActorRegister(ctx, me, verbs.ActorRegisterIn{Kind: "agent", DisplayName: "helper", ExternalIdentity: ptr("agent:helper")})
	must(t, "agent", err)
	bot := verbs.Principal{TenantID: tenant, ActorID: agent}
	lab, _, err := s.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "lab"})
	must(t, "lab", err)
	_, _, err = s.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "global"})
	must(t, "global", err)
	_, err = s.ProjectAmend(ctx, me, verbs.ProjectAmendIn{Project: "global", Labels: &[]string{"global"}})
	must(t, "global label", err)
	dept, _, err := s.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "dept"})
	must(t, "dept", err)
	_, _, err = s.LinkAssert(ctx, me, verbs.EntityRef{EntityType: "project", EntityID: lab}, verbs.EntityRef{EntityType: "project", EntityID: dept}, "member_of", nil)
	must(t, "member_of", err)
	vocabulary := func(p verbs.Principal, project, key, value string) {
		t.Helper()
		_, err := s.FactRecord(ctx, p, verbs.FactRecordIn{Project: project, Kind: "vocabulary", Key: key, Value: ptr(value)})
		must(t, "vocabulary "+project+"/"+key, err)
	}
	assert := func(typ string) (verbs.StatementAssertOut, error) {
		return s.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "lab", Type: typ, Title: "a " + typ})
	}
	stored := func(typ, want string) verbs.StatementAssertOut {
		t.Helper()
		out, err := assert(typ)
		must(t, "assert "+typ, err)
		var got, fam string
		must(t, "stored", app.QueryRow(sys, `SELECT type, family FROM projection.statement WHERE statement_id = $1`, out.StatementID).Scan(&got, &fam))
		if got != want || out.Type != want {
			t.Errorf("type %s: stored %s (out %s), want %s", typ, got, out.Type, want)
		}
		if f, _ := vocabFamily(want); fam != f {
			t.Errorf("type %s: family %s, want %s", typ, fam, f)
		}
		return out
	}

	// only general: a vocabulary type is refused with the vocabulary to enable named
	_, err = assert("migration")
	wantCode(t, "migration without dba", err, "type_unknown")
	if err == nil || !strings.Contains(err.Error(), "dba") || !strings.Contains(err.Error(), "Allowed: decision") {
		t.Errorf("type_unknown names the vocabulary and the allowed types: %v", err)
	}
	stored("decision", "decision")
	// an old name outside the set lands on its family and the warning names the type it would be
	out := stored("bugfix", "event")
	if len(out.Warnings) != 1 || !strings.HasPrefix(out.Warnings[0], "type_legacy_alias") || !strings.Contains(out.Warnings[0], "dev.bugfix") {
		t.Errorf("legacy alias warning: %v", out.Warnings)
	}
	_, err = assert("outcome")
	wantCode(t, "outcome by hand", err, "type_reserved")
	// with dev in the set the old name takes its exact type (by the short name, no alias warning); off again,
	// since dev.migration would make migration ambiguous below
	vocabulary(me, "lab", "dev", "on")
	if out := stored("bugfix", "dev.bugfix"); len(out.Warnings) != 0 {
		t.Errorf("bugfix with dev enabled: warnings %v", out.Warnings)
	}
	vocabulary(me, "lab", "dev", "off")

	// the project enables dba: short and full names
	vocabulary(me, "lab", "dba", "on")
	stored("migration", "dba.migration")
	stored("dba.query-plan", "dba.query-plan")
	// the global layer enables infra; the project turns it off again
	vocabulary(me, "global", "infra", "on")
	stored("infra.change", "infra.change")
	_, err = assert("incident")
	wantCode(t, "incident in dba and infra", err, "type_ambiguous")
	vocabulary(me, "lab", "infra", "off")
	_, err = assert("infra.change")
	wantCode(t, "infra turned off by the project", err, "type_unknown")
	stored("incident", "dba.incident")
	// a space the project is member_of
	vocabulary(me, "dept", "hr", "on")
	stored("vacancy", "hr.vacancy")
	// an agent's vocabulary fact is a proposal: it does not enable anything
	vocabulary(bot, "lab", "legal-case", "on")
	_, err = assert("claim")
	wantCode(t, "a vocabulary the agent turned on", err, "type_unknown")

	// amend resolves the new type the same way
	d := stored("decision", "decision")
	_, err = s.StatementAmend(ctx, me, verbs.StatementAmendIn{StatementID: d.StatementID, Type: ptr("migration")})
	must(t, "amend to a vocabulary type", err)
	var got string
	must(t, "amended", app.QueryRow(sys, `SELECT type || '/' || family FROM projection.statement WHERE statement_id = $1`, d.StatementID).Scan(&got))
	if got != "dba.migration/event" {
		t.Errorf("amended type: %s", got)
	}
	_, err = s.StatementAmend(ctx, me, verbs.StatementAmendIn{StatementID: d.StatementID, Type: ptr("claim")})
	wantCode(t, "amend to a type outside the set", err, "type_unknown")

	// the facade 1.19 keeps its old closed list, mapped by the fixed legacy map whatever the project's set
	f, err := s.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "lab", Type: "bugfix", Title: "facade", LegacyTypes: true})
	must(t, "facade bugfix", err)
	if f.Type != "dev.bugfix" {
		t.Errorf("facade type: %s", f.Type)
	}
	f, err = s.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "lab", Type: "decision", Title: "facade", LegacyTypes: true})
	must(t, "facade decision", err)
	if f.Type != "decision" {
		t.Errorf("facade type: %s", f.Type)
	}
	_, err = s.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "lab", Type: "migration", Title: "facade", LegacyTypes: true})
	wantCode(t, "facade outside the old list", err, "invalid_input")
}

func vocabFamily(t string) (string, bool) { return vocab.FamilyOf(t) }
