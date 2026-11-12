package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeDirectory struct {
	uris  map[string][]string
	err   error
	calls int
}

func (d *fakeDirectory) RedirectURIs(_ context.Context, id string) ([]string, error) {
	d.calls++
	return d.uris[id], d.err
}

func TestVendorOf(t *testing.T) {
	dir := &fakeDirectory{uris: map[string][]string{
		"kc-claude":    {"https://claude.ai/api/mcp/auth_callback", "https://claude.com/api/mcp/auth_callback"},
		"kc-local":     {"http://localhost:33418/callback", "http://127.0.0.1:33418/callback"},
		"kc-mixed":     {"https://claude.ai/api/mcp/auth_callback", "https://evil.example/cb"},
		"kc-lookalike": {"https://claude.ai.evil.example/cb"},
		"kc-gpt":       {"https://chatgpt.com/connector_platform_oauth_redirect"},
		"kc-gemini": {"https://oauth-redirect.googleusercontent.com/r/user_bound_custom-mcp-1-mcp_litopys_smart-gw_co_ua",
			"https://oauth-redirect-test.googleusercontent.com/r/user_bound_custom-mcp-1-mcp_litopys_smart-gw_co_ua",
			"https://oauth-redirect-sandbox.googleusercontent.com/r/user_bound_custom-mcp-1-mcp_litopys_smart-gw_co_ua"}, // as Gemini registers
		"kc-gucontent": {"https://evil.googleusercontent.com/cb"},
		"kc-grok":      {"https://grok.com/connectors-oauth-exchange-code/"}, // as Grok registers
	}}
	c := &OAuthConfig{Overrides: map[string]string{"pinned": "openai"}, Directory: dir}
	ctx := context.Background()
	cases := map[string]string{
		"https://claude.ai/oauth/mcp-oauth-client-metadata":     "anthropic", // CIMD on the vendor's host
		"https://evil.example/claude.ai":                        "unknown",   // substring is not a host
		"https://claude.ai.evil.example/x":                      "unknown",   // look-alike host
		"https://antigravity.google/oauth/client-metadata.json": "google",    // Google Antigravity (CIMD)
		"https://chatgpt.com/oauth/codex/client.json":           "openai",    // Codex CLI (static client named by its URL)
		"https://localhost/x":                                   "unknown",   // CIMD never local
		"kc-claude":                                             "anthropic", // DCR: codes go to claude.ai/claude.com
		"kc-local":                                              "local",
		"kc-mixed":                                              "unknown", // can also receive codes elsewhere
		"kc-lookalike":                                          "unknown",
		"kc-gpt":                                                "openai",
		"kc-gemini":                                             "google",  // Gemini web: Google's redirect service
		"kc-gucontent":                                          "unknown", // other googleusercontent.com hosts are not Google
		"kc-grok":                                               "xai",
		"kc-unregistered":                                       "unknown",
		"pinned":                                                "openai", // explicit override wins
	}
	for id, want := range cases {
		if got := c.vendorOf(ctx, id); got != want {
			t.Errorf("vendorOf(%q) = %q, want %q", id, got, want)
		}
	}
	n := dir.calls
	c.vendorOf(ctx, "kc-claude")
	if dir.calls != n {
		t.Error("directory answer is not cached")
	}
	dir.err = errors.New("idp down")
	if got := c.vendorOf(ctx, "kc-new"); got != "unknown" {
		t.Errorf("directory error -> %q, want unknown", got)
	}
	dir.err = nil
	dir.uris["kc-new"] = []string{"https://claude.ai/cb"}
	if got := c.vendorOf(ctx, "kc-new"); got != "anthropic" {
		t.Errorf("an error must not be cached: got %q", got)
	}
}

// TestKeycloakDirectory: client credentials, then the exact client among the search results.
func TestKeycloakDirectory(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/litopys/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_secret") != "s3" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "expires_in": 300})
	})
	mux.HandleFunc("/admin/realms/litopys/clients", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"clientId": r.URL.Query().Get("clientId") + "-other", "redirectUris": []string{"https://evil.example/"}},
			{"clientId": r.URL.Query().Get("clientId"), "redirectUris": []string{"https://claude.ai/api/mcp/auth_callback"}},
		})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	k := &KeycloakDirectory{Issuer: srv.URL + "/realms/litopys", ClientID: "dir", Secret: "s3"}
	uris, err := k.RedirectURIs(context.Background(), "abc")
	if err != nil || len(uris) != 1 || uris[0] != "https://claude.ai/api/mcp/auth_callback" {
		t.Fatalf("RedirectURIs = %v, %v", uris, err)
	}
	k2 := &KeycloakDirectory{Issuer: srv.URL + "/realms/litopys", ClientID: "dir", Secret: "wrong"}
	if _, err := k2.RedirectURIs(context.Background(), "abc"); err == nil {
		t.Fatal("a wrong secret must fail")
	}
}
