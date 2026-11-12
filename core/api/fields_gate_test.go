package api_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/files"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/server"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// gateServer: a fresh database, one person with a full token and the facade's handler (the /v1 surface).
func gateServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 8)
	admin := db.RolePool(t, "litopys_admin", 2)
	var tenant uuid.UUID
	if err := app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	ada, _, err := (&verbs.Service{DB: app}).RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := auth.IssueGrant(ctx, app, tenant, ada, "owner", "gate", nil, policy.Grant{Profile: "full"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	h, err := server.New(app, admin, tenant).Handler()
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(h), tok
}

// TestGateFieldsLedger: a household expense carries its sum as a number; a field the
// type has not, or a sum as text, is refused; ledger_read adds the sums per currency and payee; amend replaces the
// fields; a contact without its name warns.
func TestGateFieldsLedger(t *testing.T) {
	srv, tok := gateServer(t)
	defer srv.Close()
	a := connect(t, srv.URL, tok)
	a.ok("project_declare", map[string]any{"name": "kitchen"})
	a.ok("fact_record", map[string]any{"project": "kitchen", "kind": "vocabulary", "key": "household", "value": "on"})
	exp := func(title string, amount any, cur, payee string) map[string]any {
		return map[string]any{"project": "kitchen", "type": "expense", "title": title, "body": title,
			"fields": map[string]any{"amount": amount, "currency": cur, "payee": payee, "paid_at": "2026-11-01"}}
	}
	e1 := a.ok("statement_assert", exp("Worktop", 18000.0, "uah", "Master Dim"))
	a.ok("statement_assert", exp("Tiles", 7500.5, "UAH", "Epicentr"))
	a.ok("statement_assert", exp("Sink", 120.0, "EUR", "Epicentr"))
	a.fails("statement_assert", exp("Text sum", "18 000 грн", "UAH", "x"), "invalid_input")
	a.fails("statement_assert", map[string]any{"project": "kitchen", "type": "expense", "title": "Bad field", "body": "x",
		"fields": map[string]any{"colour": "red"}}, "invalid_input")
	a.fails("statement_assert", map[string]any{"project": "kitchen", "type": "note", "title": "Note with fields", "body": "x",
		"fields": map[string]any{"amount": 1.0}}, "invalid_input")

	l := a.ok("ledger_read", map[string]any{"project": "kitchen", "group_by": "payee"})
	totals := fmt.Sprint(l["totals"])
	if totals != "[map[count:1 currency:EUR group: total:120] map[count:2 currency:UAH group: total:25500.5]]" {
		t.Errorf("ledger totals: %s (rows %v)", totals, l["rows"])
	}
	// the currency was upper-cased; amend replaces the fields
	a.ok("statement_amend", map[string]any{"statement_id": e1["statement_id"],
		"fields": map[string]any{"amount": 17000.0, "currency": "UAH", "payee": "Master Dim", "paid_at": "2026-11-01"}})
	r := a.ok("recall_read", map[string]any{"ids": []string{e1["statement_id"].(string)}})["items"].([]any)[0].(map[string]any)
	if f := r["fields"].(map[string]any); f["amount"] != 17000.0 || f["currency"] != "UAH" {
		t.Errorf("fields after amend: %v", r["fields"])
	}
	if tt := fmt.Sprint(a.ok("ledger_read", map[string]any{"project": "kitchen", "type": "expense"})["totals"]); tt !=
		"[map[count:1 currency:EUR group: total:120] map[count:2 currency:UAH group: total:24500.5]]" {
		t.Errorf("ledger after amend: %s", tt)
	}
	c := a.ok("statement_assert", map[string]any{"project": "kitchen", "type": "contact", "title": "Electrician Oleh", "body": "wiring",
		"fields": map[string]any{"phone": "+380 67 123 45 67"}})
	if w := fmt.Sprint(c["warnings"]); w == "[]" || w == "<nil>" {
		t.Errorf("a contact without its name: no warning (%v)", c)
	}
}

// TestGateFactSource: a fact taken from a decision reads stale once the decision is superseded, with the
// record that replaced it; a source that is no record is refused.
func TestGateFactSource(t *testing.T) {
	srv, tok := gateServer(t)
	defer srv.Close()
	a := connect(t, srv.URL, tok)
	a.ok("project_declare", map[string]any{"name": "kitchen"})
	d := a.ok("statement_assert", map[string]any{"project": "kitchen", "type": "decision", "title": "Budget 180k", "body": "cap 180 000 UAH"})
	a.ok("fact_record", map[string]any{"project": "kitchen", "kind": "note", "key": "budget-cap", "value": "180000 UAH", "source": d["ref"]})
	a.fails("fact_record", map[string]any{"project": "kitchen", "kind": "note", "key": "x", "value": "y", "source": "R-999999"}, "statement_not_found")
	fact := func() map[string]any {
		for _, f := range a.ok("fact_list", map[string]any{"project": "kitchen"})["items"].([]any) {
			if m := f.(map[string]any); m["key"] == "budget-cap" {
				return m
			}
		}
		t.Fatal("no budget-cap")
		return nil
	}
	if f := fact(); f["source"] != d["ref"] || f["stale"] == true {
		t.Fatalf("fact with a live source: %v", f)
	}
	n := a.ok("statement_supersede", map[string]any{"supersedes": d["statement_id"],
		"statement": map[string]any{"project": "kitchen", "type": "decision", "title": "Budget 195k", "body": "cap 195 000 UAH"}})
	if f := fact(); f["stale"] != true || f["replaced_by"] != n["ref"] {
		t.Errorf("fact after its source was superseded: %v (new %v)", f, n["ref"])
	}
}

// TestGateAttachments: a file goes to the data directory by its sha, the same file again is
// already there, the list names it with its viewer path, a record points to it in files.
func TestGateAttachments(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 8)
	admin := db.RolePool(t, "litopys_admin", 2)
	var tenant uuid.UUID
	if err := app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	ada, _, err := (&verbs.Service{DB: app}).RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := auth.IssueGrant(ctx, app, tenant, ada, "owner", "gate", nil, policy.Grant{Profile: "full"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := server.New(app, admin, tenant)
	f.Files = &files.Store{Dir: t.TempDir()}
	h, err := f.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	a := connect(t, srv.URL, tok)
	a.ok("project_declare", map[string]any{"name": "kitchen"})
	content := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 contract"))
	att := a.ok("attachment_record", map[string]any{"project": "kitchen", "name": "contract.pdf", "mime": "application/pdf", "content_base64": content})
	ref, _ := att["ref"].(string)
	if !strings.HasPrefix(ref, "att:") || att["size"] != 17.0 {
		t.Fatalf("attachment_record: %v", att)
	}
	if again := a.ok("attachment_record", map[string]any{"project": "kitchen", "name": "copy.pdf", "content_base64": content}); again["already"] != true || again["ref"] != ref {
		t.Errorf("the same file again: %v", again)
	}
	a.fails("attachment_record", map[string]any{"project": "kitchen", "name": "x", "content_base64": "not base64!"}, "invalid_input")
	items := a.ok("attachment_list", map[string]any{"project": "kitchen"})["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "contract.pdf" || !strings.HasPrefix(items[0].(map[string]any)["path"].(string), "/viewer/att/") {
		t.Fatalf("attachment_list: %v", items)
	}
	a.ok("statement_assert", map[string]any{"project": "kitchen", "type": "note", "title": "Signed contract", "body": "scan attached", "files": []string{ref}})
}
