package api_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/executors"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/server"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestGateExecutorKeys: a training vendor gets public data unless a person widens the access with
// consent — an agent cannot; a provider key is sealed outside the journal (only its fingerprint goes there); the
// runner gets the key back only with the runner secret and the owner's token, and its heartbeat is stored.
func TestGateExecutorKeys(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 8)
	admin := db.RolePool(t, "litopys_admin", 2)
	var tenant uuid.UUID
	if err := app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	v := &verbs.Service{DB: app}
	str := func(s string) *string { return &s }
	yes := true
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	code := func(err error) string {
		var e *verbs.Error
		if errors.As(err, &e) {
			return e.Code
		}
		if err == nil {
			return ""
		}
		return err.Error()
	}
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must("register", err)
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	gemini, _ := executors.Lookup("gemini")
	profile := func(access *verbs.ExecutorAccess) *verbs.ExecutorProfile {
		return &verbs.ExecutorProfile{Provider: "google", Model: gemini.Model, KeyKind: "free", MaxDataClass: "internal",
			DataPolicy: &verbs.ExecutorDataPolicy{Training: &yes}, Preset: str("gemini"), Adapter: str("openai"),
			BaseURL: str(gemini.BaseURL), Access: access}
	}

	// a vendor that trains on the data: internal is refused without the person's widening, accepted with it
	_, _, err = v.ActorRegister(ctx, me, verbs.ActorRegisterIn{Kind: "service", DisplayName: "gemini", Executor: profile(nil)})
	if err == nil {
		t.Fatal("a training vendor got internal data without a widening")
	}
	all := &verbs.ExecutorAccess{Mode: "all", Consent: str("Google trains on prompts of the free tier; reviewers may read them")}
	gem, _, err := v.ActorRegister(ctx, me, verbs.ActorRegisterIn{Kind: "service", DisplayName: "gemini", Executor: profile(all)})
	must("widened by a person", err)
	_, _, err = v.ActorRegister(ctx, me, verbs.ActorRegisterIn{Kind: "service", DisplayName: "no consent",
		Executor: profile(&verbs.ExecutorAccess{Mode: "all"})})
	if err == nil {
		t.Error("a widening without consent was accepted")
	}
	// an agent of the person cannot widen
	agent, _, err := v.ActorRegister(ctx, me, verbs.ActorRegisterIn{Kind: "agent", DisplayName: "helper"})
	must("agent", err)
	_, _, err = v.ActorRegister(ctx, verbs.Principal{TenantID: tenant, ActorID: agent}, verbs.ActorRegisterIn{Kind: "service",
		DisplayName: "sneaky", Executor: profile(all)})
	if code(err) != "person_required" {
		t.Errorf("an agent widened an executor's access: %v", err)
	}
	// the class it gets: all → every class; projects → only the named ones
	var raw []byte
	must("profile", app.QueryRow(sys, `SELECT executor FROM projection.actor WHERE actor_id = $1`, gem).Scan(&raw))
	if c := policy.ExecutorClass(raw, "any"); c != "local" {
		t.Errorf("access all: class %q", c)
	}

	// the key: sealed, outside the journal, the fingerprint in the profile
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	sealer, err := executors.NewSealer(base64.StdEncoding.EncodeToString(k))
	must("sealer", err)
	const key = "AIzaTestKey0123456789"
	nonce, sealed, err := sealer.Seal(key, gem[:])
	must("seal", err)
	_, err = v.ExecutorKeySet(ctx, verbs.Principal{TenantID: tenant, ActorID: agent}, gem, executors.Fingerprint(key), nonce, sealed)
	if code(err) != "person_required" {
		t.Errorf("an agent set a key: %v", err)
	}
	_, err = v.ExecutorKeySet(ctx, me, gem, executors.Fingerprint(key), nonce, sealed)
	must("key set", err)
	var inJournal bool
	must("journal", app.QueryRow(sys, `SELECT EXISTS (SELECT 1 FROM journal WHERE payload::text LIKE '%' || $1 || '%')`, key).Scan(&inJournal))
	if inJournal {
		t.Fatal("the provider key is in the journal")
	}
	must("profile", app.QueryRow(sys, `SELECT executor FROM projection.actor WHERE actor_id = $1`, gem).Scan(&raw))
	var withKey verbs.ExecutorProfile
	must("decode", json.Unmarshal(raw, &withKey))
	if withKey.Key == nil || withKey.Key.Fingerprint != executors.Fingerprint(key) || withKey.Access == nil {
		t.Errorf("profile after the key: %+v", withKey)
	}

	// the runner: needs the secret and the owner's token; gets the key opened; its heartbeat is kept
	tok, _, err := auth.IssueGrant(ctx, app, tenant, ada, "owner", "runner", nil, policy.Grant{Profile: "core"}, 0)
	must("token", err)
	f := server.New(app, admin, tenant)
	f.Sealer, f.RunnerSecret = sealer, "runner-secret"
	h, err := f.Handler()
	must("handler", err)
	srv := httptest.NewServer(h)
	defer srv.Close()
	get := func(secret string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/runner/config", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		if secret != "" {
			req.Header.Set("X-Litopys-Runner", secret)
		}
		resp, err := http.DefaultClient.Do(req)
		must("runner config", err)
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if st, _ := get(""); st != http.StatusForbidden {
		t.Errorf("runner config without the secret: %d", st)
	}
	if st, _ := get("wrong"); st != http.StatusForbidden {
		t.Errorf("runner config with a wrong secret: %d", st)
	}
	st, cfg := get("runner-secret")
	ex, _ := cfg["executors"].([]any)
	if st != http.StatusOK || len(ex) != 1 || ex[0].(map[string]any)["key"] != key || ex[0].(map[string]any)["base_url"] != gemini.BaseURL {
		t.Fatalf("runner config: %d %v", st, cfg)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/runner/heartbeat",
		jsonBody(map[string]any{"runner": "r1", "version": "test", "executors": []any{map[string]any{"name": "gemini", "error": ""}}}))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Litopys-Runner", "runner-secret")
	resp, err := http.DefaultClient.Do(req)
	must("heartbeat", err)
	resp.Body.Close()
	var beats int
	must("status", app.QueryRow(sys, `SELECT count(*) FROM auth.runner_status WHERE runner = 'r1' AND owner_id = $1`, ada).Scan(&beats))
	if resp.StatusCode != http.StatusOK || beats != 1 {
		t.Errorf("heartbeat: %d, rows %d", resp.StatusCode, beats)
	}
}

func jsonBody(v any) *strings.Reader {
	b, _ := json.Marshal(v)
	return strings.NewReader(string(b))
}
