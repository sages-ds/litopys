package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

func TestTokens(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem) // direct checks read every owner (RLS of 0008)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 2)
	var tenant uuid.UUID
	if err := app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	v := &verbs.Service{DB: app}
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := v.ActorRegister(ctx, verbs.Principal{TenantID: tenant, ActorID: ada}, verbs.ActorRegisterIn{Kind: "agent", DisplayName: "sub"})
	if err != nil {
		t.Fatal(err)
	}

	raw, id, err := auth.Issue(ctx, app, tenant, ada, "admin", "laptop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, auth.Prefix) || len(raw) != 47 {
		t.Fatalf("token format: %q", raw)
	}
	var stored int
	if err := app.QueryRow(sys, `SELECT count(*) FROM auth.token t WHERE t::text LIKE '%' || $1 || '%'`, raw).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatal("raw token material stored")
	}
	p, err := auth.Verify(ctx, app, raw)
	if err != nil || p.ActorID != ada || p.Role != "admin" || p.TokenID != id {
		t.Fatalf("verify: %+v %v", p, err)
	}
	for _, bad := range []string{"", "OTHER_x", auth.Prefix + strings.Repeat("A", 43), raw + "x"} {
		if _, err := auth.Verify(ctx, app, bad); !errors.Is(err, auth.ErrInvalid) {
			t.Errorf("verify(%q): %v", bad, err)
		}
	}
	if _, _, err := auth.Issue(ctx, app, tenant, agent, "owner", "", nil); err == nil {
		t.Error("token issued to an agent")
	}
	if _, _, err := auth.Issue(ctx, app, tenant, ada, "root", "", nil); err == nil {
		t.Error("unknown role accepted")
	}
	list, err := auth.List(ctx, app, tenant, &ada, false)
	if err != nil || len(list) != 1 || list[0].LastUsedAt == nil || len(list[0].Fingerprint) != 12 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if ok, err := auth.Revoke(ctx, app, tenant, id); !ok || err != nil {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if ok, _ := auth.Revoke(ctx, app, tenant, id); ok {
		t.Error("second revoke reported a change")
	}
	if _, err := auth.Verify(ctx, app, raw); !errors.Is(err, auth.ErrInvalid) {
		t.Fatalf("revoked token verified: %v", err)
	}
}
