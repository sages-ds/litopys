package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/server"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// The HTTP surface of the server, pinned while the server moves out of the facade package: every
// route, what it answers without a token, and which limits wrap it. The expected values were taken on the code before
// the move.

// routesHandler is the whole server: the native API, the viewer and the legacy routes.
func routesHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	t.Setenv("LITOPYS_OAUTH_ENABLED", "1")
	t.Setenv("LITOPYS_OAUTH_ISSUER", "https://id.example/realms/r")
	t.Setenv("LITOPYS_OAUTH_RESOURCE", "https://memory.example/mcp")
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 6)
	admin := db.RolePool(t, "litopys_admin", 2)
	var tenant uuid.UUID
	if err := app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	ada, _, err := (&verbs.Service{DB: app}).RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := auth.IssueGrant(ctx, app, tenant, ada, "owner", "routes gate", nil, policy.Grant{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := server.New(app, admin, tenant)
	if f.OAuth, err = server.OAuthFromEnv(); err != nil {
		t.Fatal(err)
	}
	h, err := f.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h, tok
}

const routeProbe = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

func routeRequest(ctx context.Context, t *testing.T, url, method, path, token string) (*http.Response, string) {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(routeProbe)
	}
	req, err := http.NewRequestWithContext(ctx, method, url+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	c := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return nil, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestRoutesWithoutToken(t *testing.T) {
	h, _ := routesHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	const (
		bearerV1 = `Bearer realm="litopys", resource_metadata="https://memory.example/.well-known/oauth-protected-resource/v1/mcp", scope="litopys:chat litopys:core"`
	)
	for _, tc := range []struct {
		method, path string
		status       int
		www          string
		has          string
	}{
		{"POST", "/v1/mcp", 401, bearerV1, ""},
		{"POST", "/v1/task/list", 401, bearerV1, ""},
		{"GET", "/healthz", 200, "", "ok"},
		{"GET", "/version", 200, "", `"startedAt"`},
		{"GET", "/metrics", 200, "", `litopys_info{version="`},
		{"GET", "/.well-known/oauth-protected-resource", 200, "", `"resource":"https://memory.example/v1/mcp"`},
		{"GET", "/.well-known/oauth-protected-resource/v1/mcp", 200, "", `"resource":"https://memory.example/v1/mcp"`},
		{"GET", "/nope", 404, "", ""},
		{"POST", "/mcp", 404, "", ""}, // the legacy contract is gone
		{"POST", "/api/events", 404, "", ""},
	} {
		resp, body := routeRequest(context.Background(), t, srv.URL, tc.method, tc.path, "")
		if resp == nil {
			t.Errorf("%s %s: no response", tc.method, tc.path)
			continue
		}
		if resp.StatusCode != tc.status {
			t.Errorf("%s %s: status %d, want %d", tc.method, tc.path, resp.StatusCode, tc.status)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got != tc.www {
			t.Errorf("%s %s: WWW-Authenticate %q, want %q", tc.method, tc.path, got, tc.www)
		}
		if !strings.Contains(body, tc.has) {
			t.Errorf("%s %s: body %.200q lacks %q", tc.method, tc.path, body, tc.has)
		}
	}
	// the viewer's way in; its OIDC redirect has its own gate in core/server
	resp, _ := routeRequest(context.Background(), t, srv.URL, "GET", "/viewer/", "")
	if resp == nil {
		t.Fatal("viewer: no response")
	}
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/viewer/login" {
		t.Errorf("viewer: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// TestRouteLimits: which route is wrapped by which limit. The second call shows it: one call is the whole budget.
func TestRouteLimits(t *testing.T) {
	h, tok := routesHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	second := func(method, path string) int {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		code := 0
		for range 2 { // a live stream never ends: the timeout gives no response, which is not a refusal
			resp, _ := routeRequest(ctx, t, srv.URL, method, path, tok)
			code = 0
			if resp != nil {
				code = resp.StatusCode
			}
		}
		return code
	}
	routes := []struct {
		method, path         string
		perPerson, perTenant bool
	}{
		{"POST", "/v1/mcp", true, true},
		{"POST", "/v1/task/list", true, true},
	}
	limited := func(name string, set func(n int), want func(i int) bool) {
		t.Helper()
		for i, r := range routes {
			set(1)
			got := second(r.method, r.path) == http.StatusTooManyRequests
			set(0)
			if got != want(i) {
				t.Errorf("%s on %s %s: limited=%v, want %v", name, r.method, r.path, got, want(i))
			}
		}
	}
	calls, tenant := server.RateCalls, server.RateTenantCalls
	t.Cleanup(func() { server.RateCalls, server.RateTenantCalls = calls, tenant })
	server.RateCalls, server.RateTenantCalls = 0, 0
	limited("per person", func(n int) { server.RateCalls = n }, func(i int) bool { return routes[i].perPerson })
	limited("per tenant", func(n int) { server.RateTenantCalls = n }, func(i int) bool { return routes[i].perTenant })
}
