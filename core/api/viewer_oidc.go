package api

// Keycloak sign-in of the viewer: authorization code with PKCE (S256) for a public
// client. The browser gets a random session id in an HttpOnly cookie; the tokens stay in this process (lost
// on restart — the person signs in again). The access token is verified like any bearer token of the API
// and refreshed with the refresh token when it is about to expire.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ViewerOIDC configures the Keycloak sign-in; nil keeps the token form only.
type ViewerOIDC struct {
	Issuer   string // https://<idp>/realms/<realm>
	ClientID string // a public client with PKCE (deploy-keycloak.ps1: litopys-viewer)
	Redirect string // https://<host>/viewer/callback
	Scope    string // "openid litopys:core"
	HTTP     *http.Client

	auth     string
	token    string
	mu       sync.Mutex
	pending  map[string]pendingLogin
	sessions map[string]*viewerSession
}

type pendingLogin struct {
	verifier string
	at       time.Time
}

type viewerSession struct {
	access, refresh string
	expires, until  time.Time
	csrf            string // the form token of the confirmation page: bound to this session, never in a URL
}

const viewerSessionCookie = "litopys_viewer_session"

func randomID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (o *ViewerOIDC) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// endpoints come from the issuer's discovery document; a failed discovery is retried on the next sign-in
func (o *ViewerOIDC) endpoints(ctx context.Context) error {
	o.mu.Lock()
	done := o.auth != "" && o.token != ""
	o.mu.Unlock()
	if done {
		return nil
	}
	var d struct {
		Auth  string `json:"authorization_endpoint"`
		Token string `json:"token_endpoint"`
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(o.Issuer, "/")+"/.well-known/openid-configuration", nil)
	resp, err := o.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return err
	}
	if d.Auth == "" || d.Token == "" {
		return errors.New("discovery without authorization or token endpoint")
	}
	o.mu.Lock()
	o.auth, o.token = d.Auth, d.Token
	o.mu.Unlock()
	return nil
}

// start: redirect to the identity provider with a fresh state and PKCE challenge
func (o *ViewerOIDC) start(w http.ResponseWriter, r *http.Request) {
	if err := o.endpoints(r.Context()); err != nil {
		http.Error(w, "sign-in unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	state, verifier := randomID(), randomID()
	sum := sha256.Sum256([]byte(verifier))
	o.mu.Lock()
	if o.pending == nil {
		o.pending = map[string]pendingLogin{}
	}
	for k, p := range o.pending { // abandoned sign-ins expire after 10 minutes
		if time.Since(p.at) > 10*time.Minute {
			delete(o.pending, k)
		}
	}
	o.pending[state] = pendingLogin{verifier: verifier, at: time.Now()}
	o.mu.Unlock()
	q := url.Values{"response_type": {"code"}, "client_id": {o.ClientID}, "redirect_uri": {o.Redirect}, "scope": {o.Scope},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, o.auth+"?"+q.Encode(), http.StatusFound)
}

type tokenReply struct {
	Access           string `json:"access_token"`
	Refresh          string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
	Error            string `json:"error"`
	Description      string `json:"error_description"`
}

func (o *ViewerOIDC) exchange(ctx context.Context, form url.Values) (tokenReply, error) {
	var t tokenReply
	form.Set("client_id", o.ClientID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, o.token, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.client().Do(req)
	if err != nil {
		return t, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return t, err
	}
	if t.Error != "" || t.Access == "" {
		return t, fmt.Errorf("token endpoint: %s %s", t.Error, t.Description)
	}
	return t, nil
}

func (o *ViewerOIDC) remember(t tokenReply) string {
	sid := randomID()
	s := &viewerSession{access: t.Access, refresh: t.Refresh, expires: time.Now().Add(time.Duration(t.ExpiresIn) * time.Second),
		csrf: randomID()}
	s.until = time.Now().Add(12 * time.Hour) // a viewer session never outlives a working day
	if t.RefreshExpiresIn > 0 {
		if r := time.Now().Add(time.Duration(t.RefreshExpiresIn) * time.Second); r.Before(s.until) {
			s.until = r
		}
	}
	o.mu.Lock()
	if o.sessions == nil {
		o.sessions = map[string]*viewerSession{}
	}
	for k, x := range o.sessions {
		if time.Now().After(x.until) {
			delete(o.sessions, k)
		}
	}
	o.sessions[sid] = s
	o.mu.Unlock()
	return sid
}

// callback: the code comes back; it becomes tokens and a viewer session
func (o *ViewerOIDC) callback(w http.ResponseWriter, r *http.Request, verify TokenVerifier) {
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	o.mu.Lock()
	p, ok := o.pending[state]
	delete(o.pending, state)
	o.mu.Unlock()
	if !ok || code == "" || time.Since(p.at) > 10*time.Minute {
		w.WriteHeader(http.StatusBadRequest)
		_ = page(w, "login", map[string]any{"Error": "The sign-in expired or was not started here — try again.", "OIDC": true})
		return
	}
	t, err := o.exchange(r.Context(), url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {o.Redirect},
		"code_verifier": {p.verifier}})
	if err == nil {
		_, err = verify(r.Context(), t.Access, r) // the same check as every API call: issuer, audience, the person
	}
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_ = page(w, "login", map[string]any{"Error": "Sign-in failed: " + err.Error(), "OIDC": true})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: viewerSessionCookie, Value: o.remember(t), Path: "/viewer", HttpOnly: true, Secure: secure(r),
		SameSite: http.SameSiteLaxMode, MaxAge: 12 * 3600}) // Lax: the cookie must survive the redirect back from the IdP
	http.Redirect(w, r, "/viewer/", http.StatusSeeOther)
}

// access returns a valid access token of the session, refreshed when it is about to expire
func (o *ViewerOIDC) access(ctx context.Context, sid string) (string, error) {
	o.mu.Lock()
	s, ok := o.sessions[sid]
	o.mu.Unlock()
	if !ok || time.Now().After(s.until) {
		return "", errors.New("no session")
	}
	if time.Until(s.expires) > 30*time.Second {
		return s.access, nil
	}
	if s.refresh == "" {
		return "", errors.New("session expired")
	}
	t, err := o.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {s.refresh}})
	if err != nil {
		o.forget(sid)
		return "", err
	}
	o.mu.Lock()
	s.access, s.expires = t.Access, time.Now().Add(time.Duration(t.ExpiresIn)*time.Second)
	if t.Refresh != "" {
		s.refresh = t.Refresh
	}
	o.mu.Unlock()
	return s.access, nil
}

// csrf is the form token of a live session ("" for none)
func (o *ViewerOIDC) csrf(sid string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if s, ok := o.sessions[sid]; ok && time.Now().Before(s.until) {
		return s.csrf
	}
	return ""
}

func (o *ViewerOIDC) forget(sid string) {
	o.mu.Lock()
	delete(o.sessions, sid)
	o.mu.Unlock()
}
