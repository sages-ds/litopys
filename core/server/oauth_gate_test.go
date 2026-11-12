package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/server"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
)

// tokenFor is a JWT of the fake IdP with the given claims (sign-in rules of oauth.go, carried over from the legacy tests).
func (i *fakeIdP) tokenFor(t *testing.T, aud, sub, email string, verified bool, client, scope string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": i.srv.URL, "aud": aud, "sub": sub, "email": email,
		"email_verified": verified, "azp": client, "scope": scope, "exp": jwtExp()})
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(i.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func jwtExp() int64 { return time.Now().Add(time.Hour).Unix() }

// toolCount is the size of the surface a token is listed.
func toolCount(t *testing.T, url, token string) int {
	t.Helper()
	ctx := context.Background()
	c := mcp.NewClient(&mcp.Implementation{Name: "server-gate", Version: "1"}, nil)
	s, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url + "/v1/mcp", HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return len(res.Tools)
}

// catalog counts the projects a token sees: project_resume without a project lists them.
func catalog(t *testing.T, url, token string) int {
	t.Helper()
	ctx := context.Background()
	c := mcp.NewClient(&mcp.Implementation{Name: "server-gate", Version: "1"}, nil)
	s, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url + "/v1/mcp", HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "project_resume", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("project_resume: %v %+v", err, res)
	}
	var out struct {
		Catalog []json.RawMessage `json:"catalog"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	return len(out.Catalog)
}

// TestOAuthSignInRules: who may sign in and what the vendor decides, on the native API.
func TestOAuthSignInRules(t *testing.T) {
	ctx := context.Background()
	idp := newIdP(t)
	app, tenant := stand(t)
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	const resource = "https://memory.example/v1/mcp"
	s := server.New(app, nil, tenant)
	s.OAuth = &server.OAuthConfig{OIDC: auth.NewOIDC(idp.srv.URL, resource), Resource: resource, V1: resource, Domains: []string{"corp.example"}}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	claude := idp.tokenFor(t, resource, "sub-alice", "alice@corp.example", true, "https://claude.ai/oauth", "openid litopys:core")
	if code := listTools(t, srv.URL, claude); code != http.StatusOK {
		t.Fatalf("first sign-in by a verified e-mail of the allowed domain: %d", code)
	}
	// the consent decides the profile: without litopys:core a client gets chat, a narrower surface
	chat := idp.tokenFor(t, resource, "sub-alice", "alice@corp.example", true, "https://claude.ai/oauth", "openid litopys:chat")
	if nc, nf := toolCount(t, srv.URL, chat), toolCount(t, srv.URL, claude); nc == 0 || nc >= nf {
		t.Errorf("chat consent lists %d tools, core %d: chat must be narrower", nc, nf)
	}
	for name, tok := range map[string]string{
		"unverified email": idp.tokenFor(t, resource, "sub-bob", "bob@corp.example", false, "https://claude.ai/oauth", ""),
		"foreign domain":   idp.tokenFor(t, resource, "sub-eve", "eve@evil.example", true, "https://claude.ai/oauth", ""),
	} {
		if code := listTools(t, srv.URL, tok); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, code)
		}
	}

	// the vendor does not decide access: a non-Anthropic client sees what Claude sees, an owner rule for its vendor narrows it
	var alice uuid.UUID
	if err := app.QueryRow(sys, `SELECT actor_id FROM auth.identity WHERE sub = 'sub-alice'`).Scan(&alice); err != nil {
		t.Fatal(err)
	}
	v := &verbs.Service{DB: app}
	me := verbs.Principal{TenantID: tenant, ActorID: alice, Owner: alice}
	for _, p := range []string{"alpha", "beta"} {
		if _, _, err := v.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: p}); err != nil {
			t.Fatal(err)
		}
	}
	gpt := idp.tokenFor(t, resource, "sub-alice", "alice@corp.example", true, "https://chatgpt.com/connector/1", "litopys:core")
	all := catalog(t, srv.URL, claude)
	if n := catalog(t, srv.URL, gpt); n != all || n < 2 {
		t.Errorf("openai client sees %d projects, Claude %d (want the same, at least 2)", n, all)
	}
	if _, _, err := v.PolicySet(ctx, verbs.Principal{TenantID: tenant, ActorID: alice}, verbs.PolicySetIn{Name: "gpt-alpha", Scope: "owner",
		Rule: policy.Rule{Type: policy.ProjectAllowlist, Projects: []string{"alpha"}, Vendors: []string{"openai"}}}); err != nil {
		t.Fatal(err)
	}
	if n := catalog(t, srv.URL, gpt); n != 1 {
		t.Errorf("after the owner's allowlist the openai client sees %d projects", n)
	}
	if n := catalog(t, srv.URL, claude); n != all {
		t.Errorf("the openai-only rule narrowed Claude too: %d of %d", n, all)
	}

	// a revoked grant is refused on the next request
	if _, err := app.Exec(sys, `UPDATE auth.oauth_grant SET revoked_at = now() WHERE client_id = 'https://chatgpt.com/connector/1'`); err != nil {
		t.Fatal(err)
	}
	if code := listTools(t, srv.URL, gpt); code != http.StatusUnauthorized {
		t.Errorf("revoked grant: %d", code)
	}

	// the tenant's client allowlist
	admin, _, err := v.RegisterHuman(ctx, tenant, "Admin", "admin@corp.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.PolicySet(ctx, verbs.Principal{TenantID: tenant, ActorID: admin, Role: "admin"}, verbs.PolicySetIn{Name: "clients",
		Scope: "tenant", Rule: policy.Rule{Type: policy.ClientAllowlist, ClientIDs: []string{"https://claude.ai/oauth"}}}); err != nil {
		t.Fatal(err)
	}
	if code := listTools(t, srv.URL, idp.tokenFor(t, resource, "sub-alice", "alice@corp.example", true, "https://cursor.com/x", "")); code != http.StatusUnauthorized {
		t.Errorf("a client outside the allowlist: %d", code)
	}
	if code := listTools(t, srv.URL, claude); code != http.StatusOK {
		t.Errorf("an allowed client: %d", code)
	}
	// static tokens keep working next to OAuth
	tok, _, err := auth.Issue(ctx, app, tenant, admin, "owner", "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	if code := listTools(t, srv.URL, tok); code != http.StatusOK {
		t.Errorf("static token next to OAuth: %d", code)
	}
}

// TestProtectedResource: the metadata names /v1/mcp, whatever resource the IdP was configured with.
func TestProtectedResource(t *testing.T) {
	app, tenant := stand(t)
	s := server.New(app, nil, tenant)
	s.OAuth = &server.OAuthConfig{OIDC: auth.NewOIDC("https://id.example", "https://memory.example/mcp"), Resource: "https://memory.example/mcp",
		V1: "https://memory.example/v1/mcp"}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	for path, want := range map[string]int{"/.well-known/oauth-protected-resource": 200, "/.well-known/oauth-protected-resource/v1/mcp": 200,
		"/.well-known/oauth-protected-resource/mcp": 404} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var md map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&md)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, want)
		}
		if want == 200 && md["resource"] != "https://memory.example/v1/mcp" {
			t.Errorf("%s: resource %v", path, md["resource"])
		}
	}
}
