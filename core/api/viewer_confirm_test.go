package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestGateViewerConfirm: a human-only retention label written through the API is proposed;
// only a Keycloak session of the viewer confirms it — the token form is refused, a form without the session's CSRF
// token is refused, a page of an older version is refused with the new one shown — and project_resume names the link.
func TestGateViewerConfirm(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	v := &verbs.Service{DB: app}
	var tenant uuid.UUID
	if err := app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	a := &API{V: v, Q: &query.Service{DB: app}, DB: app, Log: slog.Default(),
		Resolve: func(ctx context.Context, _ *sdkauth.TokenInfo, _ http.Header) (context.Context, Caller, error) {
			return ctx, Caller{TenantID: tenant, ActorID: ada, Owner: ada, Role: "owner"}, nil
		}}
	verify := func(_ context.Context, tok string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		if tok == "AT-person" || tok == "LTP_form" {
			return &sdkauth.TokenInfo{}, nil
		}
		return nil, errors.New("bad token")
	}
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	if _, _, err := v.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "vault"}); err != nil {
		t.Fatal(err)
	}
	erase := `{"applies_to":"record","after_days":1,"action":"purge"}`
	if _, err := v.FactRecord(ctx, me, verbs.FactRecordIn{Project: "vault", Kind: "retention", Key: "erase", Value: &erase}); err != nil {
		t.Fatal(err)
	}

	// project_resume names the label and the page; the model passes it on, it has no tool to confirm
	c := &Caller{TenantID: tenant, ActorID: ada, Owner: ada, Role: "owner"}
	pc, err := a.pendingConfirmations(ctx, c, "vault", projectIDOf(t, app, "vault"))
	if err != nil || len(pc) != 1 || pc[0]["confirm_url"] != "/viewer/confirm/vault/erase" {
		t.Fatalf("pending confirmations: %v %v", pc, err)
	}

	o := &ViewerOIDC{ClientID: "litopys-viewer"}
	sid := o.remember(tokenReply{Access: "AT-person", ExpiresIn: 3600})
	h := a.Viewer(verify, o)
	do := func(method, cookieName, cookie string, form url.Values) *httptest.ResponseRecorder {
		var body *strings.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		} else {
			body = strings.NewReader("")
		}
		r := httptest.NewRequest(method, "/viewer/confirm/vault/erase", body)
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	page := do(http.MethodGet, viewerSessionCookie, sid, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "запропонувала") {
		t.Fatalf("confirmation page: %d %s", page.Code, page.Body.String())
	}
	field := func(name string) string {
		m := regexp.MustCompile(`name="` + name + `" value="([^"]*)"`).FindStringSubmatch(page.Body.String())
		if m == nil {
			t.Fatalf("no %s in the page", name)
		}
		return m[1]
	}
	form := url.Values{"csrf": {field("csrf")}, "event_id": {field("event_id")}, "records": {field("records")}, "tasks": {field("tasks")},
		"action": {"confirm"}}

	if rec := do(http.MethodPost, viewerCookie, "LTP_form", form); rec.Code != http.StatusForbidden {
		t.Errorf("the token form confirmed: %d", rec.Code)
	}
	bad := url.Values{}
	for k, x := range form {
		bad[k] = x
	}
	bad.Set("csrf", "forged")
	if rec := do(http.MethodPost, viewerSessionCookie, sid, bad); rec.Code != http.StatusForbidden {
		t.Errorf("a forged CSRF token: %d", rec.Code)
	}
	stale := url.Values{}
	for k, x := range form {
		stale[k] = x
	}
	stale.Set("event_id", uuid.NewString())
	if rec := do(http.MethodPost, viewerSessionCookie, sid, stale); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "нічого не підтверджено") {
		t.Errorf("a page of another version: %d", rec.Code)
	}
	if rec := do(http.MethodPost, viewerSessionCookie, sid, form); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	var confirmed bool
	if err := app.QueryRow(sys, `SELECT confirmed_at IS NOT NULL FROM projection.fact WHERE kind = 'retention' AND key = 'erase'`).Scan(&confirmed); err != nil || !confirmed {
		t.Errorf("the label is not confirmed: %v %v", confirmed, err)
	}
	if pc, _ := a.pendingConfirmations(ctx, c, "vault", projectIDOf(t, app, "vault")); len(pc) != 0 {
		t.Errorf("a confirmed label still waits: %v", pc)
	}

	// without the Keycloak sign-in configured nothing can be confirmed
	r := httptest.NewRequest(http.MethodGet, "/viewer/confirm/vault/erase", nil)
	r.AddCookie(&http.Cookie{Name: viewerSessionCookie, Value: sid})
	rec := httptest.NewRecorder()
	a.Viewer(verify, nil).ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no OIDC: %d", rec.Code)
	}
}

func projectIDOf(t *testing.T, db DB, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.QueryRow(pg.WithScope(context.Background(), pg.ScopeSystem), `SELECT project_id FROM projection.project WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
