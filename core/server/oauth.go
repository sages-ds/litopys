package server

// OAuth resource server of the facade (broker-IdP Keycloak).
// Static LTP_ tokens keep working everywhere; a JWT of the trusted issuer is accepted on the native API (/v1/mcp and
// /v1/{domain}/{verb}); the runner's own endpoints take LTP_ tokens only (core/api/runner_http.go).
// The authorization server (consent with the profile choice, CIMD/DCR allowlists, refresh rotation)
// is the IdP's job; this side verifies, maps the person and the grant, and checks revocation on every
// request.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/internal/pg"
)

// OAuthConfig enables OAuth (LITOPYS_OAUTH_ENABLED=1).
type OAuthConfig struct {
	OIDC      *auth.OIDC
	Resource  string            // LITOPYS_OAUTH_RESOURCE: an accepted aud of the access tokens (https://<host>/v1/mcp)
	V1        string            // https://<host>/v1/mcp: the native API, accepted as an audience too
	Domains   []string          // e-mail domains whose verified people may be linked or created
	Overrides map[string]string // exact client_id -> vendor (LITOPYS_OAUTH_CLIENT_VENDORS)
	Directory ClientDirectory   // the IdP's client registry: vendor from redirect URIs (oauth_vendor.go)

	mu    sync.Mutex
	cache map[string]vendorEntry
}

// Scopes a consent may grant (D5): the profile chosen on the IdP's consent screen.
const (
	OAuthScopeChat = "litopys:chat"
	OAuthScopeCore = "litopys:core"
)

// OAuthFromEnv reads LITOPYS_OAUTH_* (nil when OAuth is off).
func OAuthFromEnv() (*OAuthConfig, error) {
	if os.Getenv("LITOPYS_OAUTH_ENABLED") != "1" {
		return nil, nil
	}
	iss, res := os.Getenv("LITOPYS_OAUTH_ISSUER"), os.Getenv("LITOPYS_OAUTH_RESOURCE")
	if iss == "" || res == "" {
		return nil, errors.New("LITOPYS_OAUTH_ENABLED=1 needs LITOPYS_OAUTH_ISSUER and LITOPYS_OAUTH_RESOURCE")
	}
	c := &OAuthConfig{OIDC: auth.NewOIDC(iss, canonicalResource(res)), Resource: canonicalResource(res), Overrides: map[string]string{}}
	c.V1 = c.base() + "/v1/mcp"
	c.OIDC.Audiences = []string{c.V1}
	for _, d := range strings.Split(os.Getenv("LITOPYS_OAUTH_ALLOWED_DOMAINS"), ",") {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			c.Domains = append(c.Domains, d)
		}
	}
	for _, kv := range strings.Split(os.Getenv("LITOPYS_OAUTH_CLIENT_VENDORS"), ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
			c.Overrides[k] = strings.ToLower(v) // exact client_id: ids are case-sensitive
		}
	}
	switch d := os.Getenv("LITOPYS_OAUTH_DIRECTORY"); d {
	case "":
	case "keycloak":
		id, secret := os.Getenv("LITOPYS_OAUTH_DIRECTORY_CLIENT_ID"), os.Getenv("LITOPYS_OAUTH_DIRECTORY_CLIENT_SECRET")
		if id == "" {
			id = "litopys-facade-directory"
		}
		if secret == "" {
			return nil, errors.New("LITOPYS_OAUTH_DIRECTORY=keycloak needs LITOPYS_OAUTH_DIRECTORY_CLIENT_SECRET")
		}
		c.Directory = &KeycloakDirectory{Issuer: c.OIDC.Issuer, ClientID: id, Secret: secret}
	default:
		return nil, fmt.Errorf("LITOPYS_OAUTH_DIRECTORY=%q: want keycloak or empty", d)
	}
	return c, nil
}

// canonicalResource: lower-case scheme and host, no trailing slash.
func canonicalResource(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return strings.TrimRight(s, "/")
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	return strings.TrimRight(u.String(), "/")
}

func (c *OAuthConfig) base() string {
	u, err := url.Parse(c.Resource)
	if err != nil {
		return c.Resource
	}
	return u.Scheme + "://" + u.Host
}

// metadataURL is the path variant MCP 2025-11-25 tries first.
func (c *OAuthConfig) metadataURL(string) string {
	return c.base() + "/.well-known/oauth-protected-resource/v1/mcp"
}

// protectedResource serves RFC 9728 metadata: the resource is the native API. Tokens issued for the
// configured LITOPYS_OAUTH_RESOURCE are still accepted, so the IdP needs no change.
func (f *Server) protectedResource(w http.ResponseWriter, _ *http.Request) {
	resource := f.OAuth.V1
	WriteJSON(w, http.StatusOK, map[string]any{
		"resource":                 resource,
		"authorization_servers":    []string{f.OAuth.OIDC.Issuer},
		"scopes_supported":         []string{OAuthScopeChat, OAuthScopeCore, "offline_access"},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Litopys memory",
	})
}

var errOAuthRefused = errors.New("oauth: refused")

// oauthPrincipal maps a verified access token to a principal: identity (iss, sub) first; the e-mail
// links only on the first sign-in, only when verified, only within the allowed domains, and never to a
// self-registered account; the person and the grant must not be revoked (checked on every request).
func (f *Server) oauthPrincipal(ctx context.Context, raw string) (auth.Principal, time.Time, error) {
	var p auth.Principal
	cl, err := f.OAuth.OIDC.Verify(ctx, raw)
	if err != nil {
		return p, time.Time{}, err
	}
	sys := pg.WithScope(ctx, pg.ScopeSystem) // the edge resolves a person before an owner is known
	var actor uuid.UUID
	err = f.DB.QueryRow(sys, `SELECT actor_id FROM auth.identity WHERE iss = $1 AND sub = $2 AND tenant_id = $3`,
		cl.Issuer, cl.Subject, f.Tenant).Scan(&actor)
	if errors.Is(err, pgx.ErrNoRows) {
		if actor, err = f.linkIdentity(ctx, cl); err != nil {
			return p, time.Time{}, err
		}
	} else if err != nil {
		return p, time.Time{}, err
	}
	var revoked *time.Time
	if err := f.DB.QueryRow(sys, `SELECT revoked_at FROM projection.actor WHERE actor_id = $1 AND kind = 'human'`, actor).Scan(&revoked); err != nil || revoked != nil {
		f.Security(ctx, "oauth_revoked_person", "actor", actor)
		return p, time.Time{}, errOAuthRefused
	}
	if ok, err := policy.ClientAllowed(sys, f.DB, f.Tenant, cl.ClientID); err != nil || !ok {
		f.Security(ctx, "oauth_client_not_allowed", "client", cl.ClientID)
		return p, time.Time{}, errOAuthRefused
	}
	profile := policy.ProfileChat // D5: chat by default, core only when consented
	if slices.Contains(cl.Scope, OAuthScopeCore) {
		profile = policy.ProfileCore
	}
	vendor := f.OAuth.vendorOf(ctx, cl.ClientID)
	var grantID uuid.UUID
	err = f.DB.QueryRow(sys, `INSERT INTO auth.oauth_grant (tenant_id, actor_id, client_id, profile, vendor, last_used_at)
	        VALUES ($1, $2, $3, $4, $5, now())
	        ON CONFLICT (tenant_id, actor_id, client_id) DO UPDATE
	           SET profile = EXCLUDED.profile, vendor = EXCLUDED.vendor, last_used_at = now()
	        RETURNING grant_id, revoked_at`, f.Tenant, actor, cl.ClientID, profile, vendor).Scan(&grantID, &revoked)
	if err != nil {
		return p, time.Time{}, err
	}
	if revoked != nil {
		f.Security(ctx, "oauth_revoked_grant", "grant", grantID)
		return p, time.Time{}, errOAuthRefused
	}
	// The vendor does not decide access (decision 2026-10-06, replaces the default-deny of D9 carried
	// over from the corporate roadmap): every client of the person sees what the person sees; restrictions
	// are explicit rules (project_allowlist / project_label_deny for a vendor). The vendor stays for audit
	// and for those rules.
	p = auth.Principal{TenantID: f.Tenant, ActorID: actor, Role: "owner", TokenID: grantID, OAuth: true,
		Grant: policy.Grant{TokenID: grantID, Profile: profile, Vendor: vendor}}
	exp := cl.ExpiresAt
	p.ExpiresAt = &exp // whoami shows the access token's real lifetime, not a legacy token
	return p, cl.ExpiresAt, nil
}

func (f *Server) linkIdentity(ctx context.Context, cl auth.Claims) (uuid.UUID, error) {
	email := strings.ToLower(strings.TrimSpace(cl.Email))
	_, domain, _ := strings.Cut(email, "@")
	if !cl.EmailVerified || email == "" || !slices.Contains(f.OAuth.Domains, domain) {
		f.Security(ctx, "oauth_link_refused", "reason", "unverified or foreign domain")
		return uuid.Nil, errOAuthRefused
	}
	hs, err := f.Humans(ctx, email)
	if err != nil {
		return uuid.Nil, err
	}
	var actor uuid.UUID
	if len(hs) > 0 {
		actor = hs[0].ID
		var self bool
		if err := f.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM auth.token WHERE actor_id = $1 AND label = 'self-register')`, actor).Scan(&self); err != nil {
			return uuid.Nil, err
		}
		if self { // a self-registered account is linked by an admin, never automatically
			f.Security(ctx, "oauth_link_refused", "reason", "self-registered account", "actor", actor)
			return uuid.Nil, errOAuthRefused
		}
	} else if actor, _, err = f.V.RegisterHuman(ctx, f.Tenant, email, email); err != nil {
		return uuid.Nil, err
	}
	if _, err := f.DB.Exec(ctx, `INSERT INTO auth.identity (tenant_id, actor_id, iss, sub, email) VALUES ($1, $2, $3, $4, $5)
	        ON CONFLICT DO NOTHING`, f.Tenant, actor, cl.Issuer, cl.Subject, email); err != nil {
		return uuid.Nil, err
	}
	f.Log.Info("oauth identity linked", "actor", actor)
	return actor, nil
}

// oauthHeader extends WWW-Authenticate of a 401 when OAuth is on.
func (f *Server) oauthHeader(path string) string {
	if f.OAuth == nil {
		return ""
	}
	return fmt.Sprintf(`, resource_metadata=%q, scope=%q`, f.OAuth.metadataURL(path), OAuthScopeChat+" "+OAuthScopeCore)
}
