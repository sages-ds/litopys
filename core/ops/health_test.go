package ops

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPHealth: the commit from /version; the check wants /healthz ok, the commit, no projector lag and the
// code's projector version.
func TestHTTPHealth(t *testing.T) {
	lag, versionOK := "0", "1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			fmt.Fprint(w, "ok")
		case "/version":
			fmt.Fprint(w, `{"commit":"abc","migration":"0042_maintenance.sql"}`)
		case "/metrics":
			fmt.Fprintf(w, "# TYPE litopys_projector_lag gauge\nlitopys_projector_lag %s\nlitopys_projector_version_ok %s\nlitopys_up 1\n", lag, versionOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h := HTTPHealth{Endpoint: srv.URL}
	ctx := context.Background()
	if c := h.Commit(ctx); c != "abc" {
		t.Fatalf("commit %q", c)
	}
	if err := h.Check(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := h.Check(ctx, "def"); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Errorf("another commit: %v", err)
	}
	lag = "3"
	if err := h.Check(ctx, "abc"); err == nil || !strings.Contains(err.Error(), "projector_lag") {
		t.Errorf("lag: %v", err)
	}
	lag, versionOK = "0", "0"
	if err := h.Check(ctx, "abc"); err == nil || !strings.Contains(err.Error(), "version_ok") {
		t.Errorf("version: %v", err)
	}
	if c := (HTTPHealth{Endpoint: "http://127.0.0.1:1"}).Commit(ctx); c != "" {
		t.Errorf("a core that does not answer reports %q", c)
	}
}

// TestRenderCompose: the image by digest, secrets from the environment as YAML single-quoted scalars, nothing left.
func TestRenderCompose(t *testing.T) {
	tpl := "image: __IMAGE__\npw: __APP_PASSWORD__\n"
	env := map[string]string{"LITOPYS_APP_PASSWORD": "it's"}
	got, err := RenderCompose(tpl, "g/litopys@sha256:ab", func(k string) string { return env[k] })
	if err != nil || got != "image: g/litopys@sha256:ab\npw: 'it''s'\n" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := RenderCompose(tpl, "g/litopys:latest", func(k string) string { return env[k] }); err == nil {
		t.Error("an image by tag must be refused")
	}
	if _, err := RenderCompose(tpl, "g/l@sha256:ab", func(string) string { return "" }); err == nil {
		t.Error("a missing secret must be refused")
	}
	if _, err := RenderCompose(tpl+"x: __OTHER__\n", "g/l@sha256:ab", func(k string) string { return env[k] }); err == nil {
		t.Error("an unknown placeholder must be refused")
	}
	// an optional switch (the canary): empty when unset, its value when set
	opt := tpl + "c: __CANARY__\na: __CANARY_ACTORS__\n"
	if got, err := RenderCompose(opt, "g/l@sha256:ab", func(k string) string { return env[k] }); err != nil || !strings.HasSuffix(got, "c: ''\na: ''\n") {
		t.Errorf("an unset optional placeholder: %q %v", got, err)
	}
	env["LITOPYS_CANARY"], env["LITOPYS_CANARY_ACTORS"] = "abc123", "a-b"
	if got, err := RenderCompose(opt, "g/l@sha256:ab", func(k string) string { return env[k] }); err != nil || !strings.HasSuffix(got, "c: 'abc123'\na: 'a-b'\n") {
		t.Errorf("a set optional placeholder: %q %v", got, err)
	}
}
