package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// The Keycloak sign-in of the viewer against a fake identity provider (P6c): PKCE, state, the code exchange,
// the session cookie, the refresh of an expiring access token.
func TestViewerOIDC(t *testing.T) {
	var challenge string
	refreshed := 0
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/realms/x/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"authorization_endpoint": "https://idp/auth", "token_endpoint": "http://" + r.Host + "/token"})
		case "/token":
			_ = r.ParseForm()
			switch r.Form.Get("grant_type") {
			case "authorization_code":
				sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
				if r.Form.Get("code") != "good-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.Form.Get("client_id") != "litopys-viewer" {
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "AT1", "refresh_token": "RT1", "expires_in": 1, "refresh_expires_in": 3600})
			case "refresh_token":
				refreshed++
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "AT2", "refresh_token": "RT2", "expires_in": 300})
			}
		}
	}))
	defer idp.Close()
	o := &ViewerOIDC{Issuer: idp.URL + "/realms/x", ClientID: "litopys-viewer", Redirect: "https://host/viewer/callback", Scope: "openid litopys:core"}
	verify := func(_ context.Context, tok string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		if tok == "AT1" || tok == "AT2" {
			return &sdkauth.TokenInfo{}, nil
		}
		return nil, errors.New("bad token")
	}

	// start: a redirect to the IdP with state and an S256 challenge
	rec := httptest.NewRecorder()
	o.start(rec, httptest.NewRequest(http.MethodGet, "/viewer/oidc", nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if rec.Code != http.StatusFound || !strings.HasPrefix(loc.String(), "https://idp/auth?") || q.Get("code_challenge_method") != "S256" ||
		q.Get("client_id") != "litopys-viewer" || q.Get("redirect_uri") != "https://host/viewer/callback" || q.Get("state") == "" {
		t.Fatalf("start: %d %s", rec.Code, loc)
	}
	challenge = q.Get("code_challenge")

	// a state that was not started here is refused
	rec = httptest.NewRecorder()
	o.callback(rec, httptest.NewRequest(http.MethodGet, "/viewer/callback?state=forged&code=good-code", nil), verify)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("forged state: %d", rec.Code)
	}

	// the right state: code -> tokens -> a session cookie (HttpOnly), the state is single-use
	cb := "/viewer/callback?state=" + url.QueryEscape(q.Get("state")) + "&code=good-code"
	rec = httptest.NewRecorder()
	o.callback(rec, httptest.NewRequest(http.MethodGet, cb, nil), verify)
	var sid string
	for _, c := range rec.Result().Cookies() {
		if c.Name == viewerSessionCookie && c.HttpOnly {
			sid = c.Value
		}
	}
	if rec.Code != http.StatusSeeOther || sid == "" || strings.Contains(sid, "AT1") {
		t.Fatalf("callback: %d, session %q", rec.Code, sid)
	}
	rec = httptest.NewRecorder()
	o.callback(rec, httptest.NewRequest(http.MethodGet, cb, nil), verify)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a state reused: %d", rec.Code)
	}

	// the access token expires within a second: the next access refreshes it
	time.Sleep(10 * time.Millisecond)
	tok, err := o.access(context.Background(), sid)
	if err != nil || tok != "AT2" || refreshed != 1 {
		t.Fatalf("refresh: %q %v (refreshed %d)", tok, err, refreshed)
	}
	if tok, _ := o.access(context.Background(), sid); tok != "AT2" || refreshed != 1 {
		t.Errorf("a fresh token must not be refreshed again: %q %d", tok, refreshed)
	}
	o.forget(sid)
	if _, err := o.access(context.Background(), sid); err == nil {
		t.Error("a forgotten session still gives a token")
	}
}
