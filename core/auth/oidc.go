package auth

// OIDC verifies access tokens of the broker-IdP (Keycloak) as a resource server:
// signature by the issuer's JWKS, iss, the EXACT canonical resource as aud (RFC 8707), exp. Keys are
// discovered through /.well-known/openid-configuration and refreshed on an unknown kid (at most once
// a minute, so a flood of forged kids cannot hammer the IdP).

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is what Litopys reads from an access token.
type Claims struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	ClientID      string // azp (Keycloak) or client_id
	Scope         []string
	ExpiresAt     time.Time
}

// OIDC is one trusted issuer with its expected audiences: one service, several resource paths (the
// facade's /mcp and the native API's /v1/mcp) — a token must name at least one exactly.
type OIDC struct {
	Issuer    string
	Audience  string
	Audiences []string // more accepted audiences besides Audience
	HTTP      *http.Client

	mu      sync.Mutex
	keys    map[string]any
	fetched time.Time
}

// NewOIDC trusts tokens of issuer addressed to audience (https://<host>/v1/mcp).
func NewOIDC(issuer, audience string) *OIDC {
	return &OIDC{Issuer: strings.TrimRight(issuer, "/"), Audience: audience, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// LooksLikeJWT: three base64url segments — anything else is not an OAuth access token of ours.
func LooksLikeJWT(raw string) bool {
	return strings.Count(raw, ".") == 2 && strings.HasPrefix(raw, "eyJ")
}

// Verify checks the token and returns its claims.
func (o *OIDC) Verify(ctx context.Context, raw string) (Claims, error) {
	var c Claims
	tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return o.key(ctx, kid)
	}, jwt.WithIssuer(o.Issuer), jwt.WithExpirationRequired(),
		jwt.WithValidMethods([]string{"RS256", "ES256"}), jwt.WithLeeway(30*time.Second))
	if err != nil {
		return c, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	m, _ := tok.Claims.(jwt.MapClaims)
	aud, _ := m.GetAudience()
	if !slices.ContainsFunc(append([]string{o.Audience}, o.Audiences...), func(a string) bool { return slices.Contains(aud, a) }) {
		return c, fmt.Errorf("%w: audience %v is not this resource", ErrInvalid, []string(aud))
	}
	c.Issuer, _ = m["iss"].(string)
	c.Subject, _ = m["sub"].(string)
	c.Email, _ = m["email"].(string)
	c.EmailVerified, _ = m["email_verified"].(bool)
	if c.ClientID, _ = m["azp"].(string); c.ClientID == "" {
		c.ClientID, _ = m["client_id"].(string)
	}
	if s, ok := m["scope"].(string); ok {
		c.Scope = strings.Fields(s)
	}
	if exp, err := m.GetExpirationTime(); err == nil && exp != nil {
		c.ExpiresAt = exp.Time
	}
	if c.Subject == "" {
		return c, fmt.Errorf("%w: no sub", ErrInvalid)
	}
	return c, nil
}

func (o *OIDC) key(ctx context.Context, kid string) (any, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if k, ok := o.keys[kid]; ok {
		return k, nil
	}
	if time.Since(o.fetched) < time.Minute && o.keys != nil {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	keys, err := o.fetch(ctx)
	o.fetched = time.Now()
	if err != nil {
		return nil, err
	}
	o.keys = keys
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown kid %q", kid)
}

func (o *OIDC) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(v)
}

func (o *OIDC) fetch(ctx context.Context) (map[string]any, error) {
	var conf struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := o.getJSON(ctx, o.Issuer+"/.well-known/openid-configuration", &conf); err != nil {
		return nil, err
	}
	if strings.TrimRight(conf.Issuer, "/") != o.Issuer || conf.JWKSURI == "" {
		return nil, errors.New("openid-configuration: issuer mismatch or no jwks_uri")
	}
	var set struct {
		Keys []struct {
			Kid, Kty, Use, Alg, N, E, Crv, X, Y string
		} `json:"keys"`
	}
	if err := o.getJSON(ctx, conf.JWKSURI, &set); err != nil {
		return nil, err
	}
	b64 := func(s string) *big.Int {
		b, _ := base64.RawURLEncoding.DecodeString(s)
		return new(big.Int).SetBytes(b)
	}
	keys := map[string]any{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		switch k.Kty {
		case "RSA":
			keys[k.Kid] = &rsa.PublicKey{N: b64(k.N), E: int(b64(k.E).Int64())}
		case "EC":
			if k.Crv == "P-256" {
				// the uncompressed point 04||X||Y (32 bytes each); parsing also checks it lies on the curve,
				// so a malformed key is skipped instead of being kept as raw coordinates (SA1019)
				x, y := b64(k.X), b64(k.Y)
				if x.BitLen() > 256 || y.BitLen() > 256 { // FillBytes would panic on a coordinate this long
					continue
				}
				point := append(append([]byte{4}, x.FillBytes(make([]byte, 32))...), y.FillBytes(make([]byte, 32))...)
				if pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point); err == nil {
					keys[k.Kid] = pub
				}
			}
		}
	}
	return keys, nil
}
