package api_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/api"
	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/server"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestCanary: a listed actor gets the channel markers in instructions (head, after the core
// part, tail), in the description of system_whoami and in its result; anyone else gets the normal surface; a
// token without listed actors is refused.
func TestCanary(t *testing.T) {
	t.Setenv("LITOPYS_CANARY", "x7k2")
	t.Setenv("LITOPYS_CANARY_ACTORS", "")
	if _, err := api.CanaryFromEnv(); err == nil {
		t.Fatal("a canary without actors must be refused")
	}
	t.Setenv("LITOPYS_CANARY_ACTORS", "not-a-uuid")
	if _, err := api.CanaryFromEnv(); err == nil {
		t.Fatal("a bad actor id must be refused")
	}
	t.Setenv("LITOPYS_CANARY", "")
	if c, err := api.CanaryFromEnv(); c != nil || err != nil {
		t.Fatalf("no token, no canary: %v %v", c, err)
	}

	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	var tenant uuid.UUID
	if err := app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	v := &verbs.Service{DB: app}
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := v.RegisterHuman(ctx, tenant, "Other", "other@example")
	if err != nil {
		t.Fatal(err)
	}
	issue := func(who uuid.UUID) string {
		tok, _, err := auth.IssueGrant(ctx, app, tenant, who, "owner", "canary", nil, policy.Grant{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	t.Setenv("LITOPYS_CANARY", "x7k2")
	t.Setenv("LITOPYS_CANARY_ACTORS", " "+ada.String()+" ,")
	canary, err := api.CanaryFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	f := server.New(app, nil, tenant)
	f.Canary = canary
	h, err := f.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	look := func(c *client) (instr, desc string, res map[string]any) {
		instr = c.s.InitializeResult().Instructions
		tl, err := c.s.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range tl.Tools {
			if x.Name == "system_whoami" {
				desc = x.Description
			}
		}
		return instr, desc, c.ok("system_whoami", nil)
	}

	instr, desc, res := look(connect(t, srv.URL, issue(ada)))
	lines := strings.Split(instr, "\n")
	if lines[0] != "CANARY-INSTR-HEAD-x7k2" || lines[len(lines)-1] != "CANARY-INSTR-TAIL-x7k2" {
		t.Errorf("head and tail markers: %q … %q", lines[0], lines[len(lines)-1])
	}
	if i := strings.Index(instr, "CANARY-INSTR-CORE-x7k2"); i < len(api.InstructionsCore) || !strings.Contains(instr, api.InstructionsCore+"\nCANARY-INSTR-CORE-x7k2") {
		t.Errorf("core marker right after the core part: at %d", i)
	}
	if !strings.HasSuffix(desc, " CANARY-DESC-x7k2") || res["canary"] != "CANARY-RES-x7k2" {
		t.Errorf("description %q, result %v", desc, res["canary"])
	}

	instr, desc, res = look(connect(t, srv.URL, issue(other)))
	if instr != api.Instructions || strings.Contains(desc, "CANARY") || res["canary"] != nil {
		t.Errorf("an unlisted actor sees markers: %q / %q / %v", instr[:40], desc, res["canary"])
	}
}
