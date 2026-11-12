package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"litopys.smart-gw.co.ua/core/auth"
)

// TestOIDCKeysEC: a P-256 key of the JWKS verifies ES256 tokens; a point off the curve and a coordinate longer than
// 32 bytes are skipped (no panic), so a token naming them is refused (ecdsa keys parsed, not built from X/Y).
func TestOIDCKeysEC(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := key.PublicKey.Bytes() // 04 || X || Y
	if err != nil {
		t.Fatal(err)
	}
	b := func(p []byte) string { return base64.RawURLEncoding.EncodeToString(p) }
	one := make([]byte, 32)
	one[31] = 1
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL, "jwks_uri": srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
			{"kid": "ec1", "kty": "EC", "crv": "P-256", "use": "sig", "x": b(raw[1:33]), "y": b(raw[33:])},
			{"kid": "off", "kty": "EC", "crv": "P-256", "use": "sig", "x": b(one), "y": b(one)},
			{"kid": "long", "kty": "EC", "crv": "P-256", "use": "sig", "x": b(append([]byte{1}, raw[1:33]...)), "y": b(raw[33:])},
		}})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	o := auth.NewOIDC(srv.URL, "https://memory.example/mcp")
	sign := func(kid string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": srv.URL, "aud": "https://memory.example/mcp",
			"sub": "u1", "exp": time.Now().Add(time.Hour).Unix()})
		tok.Header["kid"] = kid
		s, err := tok.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	ctx := context.Background()
	if c, err := o.Verify(ctx, sign("ec1")); err != nil || c.Subject != "u1" {
		t.Fatalf("ES256 with a P-256 key: %v %+v", err, c)
	}
	for _, kid := range []string{"off", "long"} {
		if _, err := o.Verify(ctx, sign(kid)); !errors.Is(err, auth.ErrInvalid) {
			t.Errorf("%s: a malformed key must not verify: %v", kid, err)
		}
	}
}
