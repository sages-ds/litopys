package server_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/server"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// The gates of the server itself, on the native API: they stay when the legacy contract goes.

// fakeIdP is an OIDC issuer with one RSA key (instead of Keycloak).
type fakeIdP struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": idp.srv.URL, "jwks_uri": idp.srv.URL + "/jwks",
			"authorization_endpoint": idp.srv.URL + "/auth", "token_endpoint": idp.srv.URL + "/token"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		b := func(x *big.Int) string { return base64.RawURLEncoding.EncodeToString(x.Bytes()) }
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kid": "k1", "kty": "RSA", "use": "sig",
			"n": b(key.N), "e": b(big.NewInt(int64(key.E)))}}})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (i *fakeIdP) token(t *testing.T, aud, sub, email string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": i.srv.URL, "aud": aud, "sub": sub, "email": email,
		"email_verified": true, "azp": "https://claude.ai/oauth", "scope": "openid litopys:core", "exp": time.Now().Add(time.Hour).Unix()})
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(i.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func stand(t *testing.T) (*pgxpool.Pool, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 6)
	var tenant uuid.UUID
	if err := app.QueryRow(pg.WithScope(ctx, pg.ScopeSystem), `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	return app, tenant
}

func listTools(t *testing.T, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// callGuide is a real tool call: it goes through the principal, the owner and the policy of the request.
func callGuide(t *testing.T, url, token string) map[string]any {
	t.Helper()
	ctx := context.Background()
	c := mcp.NewClient(&mcp.Implementation{Name: "server-gate", Version: "1"}, nil)
	s, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url + "/v1/mcp",
		HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "system_guide", Arguments: map[string]any{}})
	if err != nil || res.IsError || len(res.Content) == 0 {
		t.Fatalf("system_guide: %v %+v", err, res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestViewerRedirect: the sign-in of the viewer comes back to /viewer/callback when the OAuth resource is /v1/mcp too.
func TestViewerRedirect(t *testing.T) {
	idp := newIdP(t)
	app, tenant := stand(t)
	for _, resource := range []string{"https://memory.example/mcp", "https://memory.example/v1/mcp"} {
		s := server.New(app, nil, tenant)
		s.OAuth = &server.OAuthConfig{OIDC: auth.NewOIDC(idp.srv.URL, resource), Resource: resource}
		h, err := s.Handler()
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h)
		c := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Get(srv.URL + "/viewer/oidc")
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || resp.StatusCode != http.StatusFound {
			t.Fatalf("%s: %d %q %v", resource, resp.StatusCode, resp.Header.Get("Location"), err)
		}
		if got := loc.Query().Get("redirect_uri"); got != "https://memory.example/viewer/callback" {
			t.Errorf("%s: redirect_uri %q", resource, got)
		}
	}
}

func insertExecutionEvent(ctx context.Context, t *testing.T, app *pgxpool.Pool, tenant, owner uuid.UUID) {
	t.Helper()
	if _, err := app.Exec(pg.WithOwner(ctx, owner), `INSERT INTO execution.event (tenant_id, owner_id, actor_id, context_id, project_id, kind, payload)
	        VALUES ($1, $2, $2, $3, $4, 'file_edit', '{}')`, tenant, owner, uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
}

// TestExecutionEventsSweep: the retention of execution events belongs to the server, not to the legacy contract.
func TestExecutionEventsSweep(t *testing.T) {
	ctx := context.Background()
	app, tenant := stand(t)
	v := &verbs.Service{DB: app}
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	insertExecutionEvent(ctx, t, app, tenant, ada)
	s := server.New(app, nil, tenant)
	if n, err := s.SweepExecutionEvents(ctx, 1); err != nil || n != 0 {
		t.Fatalf("a fresh event under a one-day TTL: %d %v", n, err)
	}
	if n, err := s.SweepExecutionEvents(ctx, 0); err != nil || n != 1 {
		t.Fatalf("the event past its TTL: %d %v", n, err)
	}
}

// TestOAuthOnNativeAPI: a JWT of a linked person works on /v1/mcp, a wrong audience does not, a revoked person does not.
func TestOAuthOnNativeAPI(t *testing.T) {
	ctx := context.Background()
	idp := newIdP(t)
	app, tenant := stand(t)
	const resource = "https://memory.example/v1/mcp"
	s := server.New(app, nil, tenant)
	s.OAuth = &server.OAuthConfig{OIDC: auth.NewOIDC(idp.srv.URL, resource), Resource: resource, V1: resource, Domains: []string{"corp.example"}}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	alice := idp.token(t, resource, "sub-alice", "alice@corp.example")
	if g := callGuide(t, srv.URL, alice); g["topic"] != "index" {
		t.Fatalf("a tool call with a JWT on /v1/mcp: %v", g)
	}
	if code := listTools(t, srv.URL, idp.token(t, "https://other.example/v1/mcp", "sub-alice", "alice@corp.example")); code != http.StatusUnauthorized {
		t.Errorf("foreign audience: %d", code)
	}
	var actor uuid.UUID
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	if err := app.QueryRow(sys, `SELECT actor_id FROM auth.identity WHERE sub = 'sub-alice'`).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	v := &verbs.Service{DB: app}
	if _, err := v.ActorRevoke(ctx, verbs.Principal{TenantID: tenant, ActorID: actor, Owner: actor}, actor, "gate"); err != nil {
		t.Fatal(err)
	}
	if code := listTools(t, srv.URL, alice); code != http.StatusUnauthorized {
		t.Errorf("revoked person: %d", code)
	}
}

// TestLimitsOnNativeAPI: 429 per person with a valid token, 429 for a repeated bad token.
func TestLimitsOnNativeAPI(t *testing.T) {
	ctx := context.Background()
	app, tenant := stand(t)
	ada, _, err := (&verbs.Service{DB: app}).RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := auth.IssueGrant(ctx, app, tenant, ada, "owner", "limits gate", nil, policy.Grant{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls, perKey := server.RateCalls, server.UnauthorizedPerKey
	t.Cleanup(func() { server.RateCalls, server.UnauthorizedPerKey = calls, perKey })
	server.RateCalls, server.UnauthorizedPerKey = 2, 2
	h, err := server.New(app, nil, tenant).Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	var last int
	for range 3 {
		last = listTools(t, srv.URL, tok)
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("third call of a budget of two: %d", last)
	}
	for range 3 {
		last = listTools(t, srv.URL, "LTP_not-a-token")
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("third bad token of a budget of two: %d", last)
	}
}

type headers struct {
	token string
	extra map[string]string
}

func (h headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.token)
	for k, v := range h.extra {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// TestClientHeader (#273): the server names the client by X-Litopys-Client — the session it opens keeps
// it as app — and no other client header (no synonym of an older one), so such a client falls back to its clientInfo.
func TestClientHeader(t *testing.T) {
	ctx := context.Background()
	app, tenant := stand(t)
	ada, _, err := (&verbs.Service{DB: app}).RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := auth.IssueGrant(ctx, app, tenant, ada, "owner", "client header gate", nil, policy.Grant{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	h, err := server.New(app, nil, tenant).Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	open := func(project string, extra map[string]string) string {
		t.Helper()
		c := mcp.NewClient(&mcp.Implementation{Name: "server-gate", Version: "1"}, nil)
		s, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/v1/mcp",
			HTTPClient: &http.Client{Transport: headers{tok, extra}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "session_open",
			Arguments: map[string]any{"project": project, "attach": "none", "auto_create": true}})
		if err != nil || res.IsError {
			t.Fatalf("session_open: %v %+v", err, res)
		}
		var got string
		if err := app.QueryRow(pg.WithScope(ctx, pg.ScopeSystem), `SELECT payload::text FROM journal WHERE type IN ('run.started', 'context.opened') AND payload::text LIKE '%"app"%'
		        ORDER BY seq DESC LIMIT 1`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if p := open("header-lab", map[string]string{"X-Litopys-Client": "gate-client/2"}); !strings.Contains(p, `"app": "gate-client/2"`) {
		t.Errorf("X-Litopys-Client not kept: %s", p)
	}
	if p := open("header-lab-2", map[string]string{"X-Other-Client": "old-client/1"}); strings.Contains(p, "old-client") || !strings.Contains(p, `"app": "server-gate/1"`) {
		t.Errorf("a client header other than X-Litopys-Client was read: %s", p)
	}
}
