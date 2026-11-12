package server

// Vendor of an OAuth client (policy D9: a non-Anthropic grant sees no project until allowed). The vendor
// is a trust condition, so it comes from a fact the client cannot fake: where the IdP delivers its
// authorization codes (redirect URIs) — only claude.ai can receive a code sent to claude.ai. Never from a
// substring of client_id: "https://evil.example/claude.ai" is not Anthropic.
//
// Order: an explicit override (LITOPYS_OAUTH_CLIENT_VENDORS, exact client_id) → a CIMD client_id (an
// https URL) by its host → the IdP's client directory (Keycloak: registered redirect URIs) → "unknown".

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// hostVendors: registrable domains (a host matches the domain itself or any subdomain of it).
var hostVendors = []struct{ domain, vendor string }{
	{"claude.ai", "anthropic"}, {"claude.com", "anthropic"}, {"anthropic.com", "anthropic"},
	{"chatgpt.com", "openai"}, {"openai.com", "openai"},
	// Grok's connectors (DCR, redirect https://grok.com/connectors-oauth-exchange-code/)
	{"grok.com", "xai"}, {"x.ai", "xai"},
	{"gemini.google.com", "google"}, {"aistudio.google.com", "google"}, {"antigravity.google", "google"},
	// Gemini web's custom connectors (2026-11-01): Google's redirect service, a path bound to the user — this host
	// only, never the whole googleusercontent.com (user content lives there)
	{"oauth-redirect.googleusercontent.com", "google"}, {"oauth-redirect-test.googleusercontent.com", "google"},
	{"oauth-redirect-sandbox.googleusercontent.com", "google"},
	{"localhost", "local"}, {"127.0.0.1", "local"}, {"::1", "local"},
}

func vendorOfHost(host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, h := range hostVendors {
		if host == h.domain || strings.HasSuffix(host, "."+h.domain) {
			return h.vendor
		}
	}
	return ""
}

// vendorOfURIs: one vendor shared by every redirect URI, else "unknown" (a client that can also receive
// codes elsewhere is not that vendor's client).
func vendorOfURIs(uris []string) string {
	vendor := ""
	for _, raw := range uris {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return "unknown"
		}
		v := vendorOfHost(u.Hostname())
		if v == "" || (vendor != "" && v != vendor) {
			return "unknown"
		}
		vendor = v
	}
	if vendor == "" {
		return "unknown"
	}
	return vendor
}

// ClientDirectory answers where the IdP sends a client's authorization codes.
type ClientDirectory interface {
	RedirectURIs(ctx context.Context, clientID string) ([]string, error)
}

type vendorEntry struct {
	vendor string
	at     time.Time
}

const vendorCacheTTL = time.Hour

// vendorOf resolves and caches the vendor of a client id; a directory error is not cached.
func (c *OAuthConfig) vendorOf(ctx context.Context, clientID string) string {
	if v, ok := c.Overrides[clientID]; ok {
		return v
	}
	if u, err := url.Parse(clientID); err == nil && u.Scheme == "https" && u.Hostname() != "" {
		if v := vendorOfHost(u.Hostname()); v != "" && v != "local" {
			return v // CIMD: the client id is the metadata URL on the vendor's own host
		}
		return "unknown"
	}
	if c.Directory == nil {
		return "unknown"
	}
	c.mu.Lock()
	e, ok := c.cache[clientID]
	c.mu.Unlock()
	if ok && time.Since(e.at) < vendorCacheTTL {
		return e.vendor
	}
	uris, err := c.Directory.RedirectURIs(ctx, clientID)
	if err != nil {
		return "unknown"
	}
	v := vendorOfURIs(uris)
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]vendorEntry{}
	}
	c.cache[clientID] = vendorEntry{v, time.Now()}
	c.mu.Unlock()
	return v
}

// KeycloakDirectory reads a client's redirect URIs with a service account that holds only
// realm-management/view-clients (scripts/deploy-keycloak.ps1 creates it).
type KeycloakDirectory struct {
	Issuer, ClientID, Secret string
	HTTP                     *http.Client

	mu    sync.Mutex
	token string
	exp   time.Time
}

func (k *KeycloakDirectory) accessToken(ctx context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.token != "" && time.Until(k.exp) > 30*time.Second {
		return k.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {k.ClientID}, "client_secret": {k.Secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.Issuer+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := k.do(req, &out); err != nil {
		return "", err
	}
	k.token, k.exp = out.AccessToken, time.Now().Add(time.Duration(out.ExpiresIn)*time.Second)
	return k.token, nil
}

func (k *KeycloakDirectory) RedirectURIs(ctx context.Context, clientID string) ([]string, error) {
	tok, err := k.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	admin := strings.Replace(k.Issuer, "/realms/", "/admin/realms/", 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, admin+"/clients?search=false&clientId="+url.QueryEscape(clientID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	var clients []struct {
		ClientID     string   `json:"clientId"`
		RedirectURIs []string `json:"redirectUris"`
	}
	if err := k.do(req, &clients); err != nil {
		return nil, err
	}
	for _, c := range clients {
		if c.ClientID == clientID {
			return c.RedirectURIs, nil
		}
	}
	return nil, nil // unknown to the IdP: no redirect URIs, vendor "unknown"
}

func (k *KeycloakDirectory) do(req *http.Request, v any) error {
	hc := k.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("keycloak directory: %s %s: HTTP %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	return json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(v)
}
