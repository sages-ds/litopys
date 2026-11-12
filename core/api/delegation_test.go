package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/server"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

const delegationDef = `{"steps": [
  {"key": "collect", "title": "Candidates", "kind": "model", "executor_class": "cheap", "write_mode": "candidate",
   "verbs": ["recall.search", "statement.assert"], "max_data_class": "internal"},
  {"key": "write", "title": "Write", "kind": "model", "verbs": ["recall.search"]},
  {"key": "commit", "title": "Commit", "kind": "script", "who_may_run": "assistant"}]}`

// status of a POST /v1/<domain>/<verb> with a token.
func status(t *testing.T, srv, token, path, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestDelegation is the acceptance of derived grants (decisions 2026-10-16) through
// the real HTTP stack: step.delegate gives an executor a token for one step of one run; that token speaks as the
// executor, calls only its step's verbs and its own step tools, only on its run and step, writes only the step
// output in candidate mode, cannot delegate without step.delegate among its verbs, and dies with its parent token
// (cascade) or with the run.
func TestDelegation(t *testing.T) {
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
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	if _, _, err := v.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "litopys-test"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.WorkflowDeclare(ctx, me, verbs.WorkflowDeclareIn{Project: "litopys-test", Key: "harvest", Title: "Harvest",
		Definition: json.RawMessage(delegationDef)}); err != nil {
		t.Fatal(err)
	}
	issue := func(profile string) (string, uuid.UUID) {
		tok, id, err := auth.IssueGrant(ctx, app, tenant, ada, "owner", "delegation "+profile, nil, policy.Grant{Profile: profile}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok, id
	}
	tokCore, _ := issue("core")
	tokChat, _ := issue("chat")
	tokParent, parentID := issue("core")
	s := server.New(app, admin, tenant)
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	o := connect(t, srv.URL, tokCore)

	run := o.ok("execution_start", map[string]any{"project": "litopys-test", "workflow": "harvest"})
	x := run["execution_id"].(string)
	// numbers within the project: the first workflow and the first execution
	if run["ref"] != "litopys-test/X-1" {
		t.Errorf("execution ref: %v", run["ref"])
	}
	if wl := o.ok("workflow_list", map[string]any{"project": "litopys-test"})["items"].([]any); len(wl) != 1 ||
		wl[0].(map[string]any)["ref"] != "litopys-test/W-1" {
		t.Errorf("workflow ref: %v", wl)
	}

	// who may delegate what
	if code := status(t, srv.URL, tokChat, "/v1/step/delegate", `{"execution_id":"`+x+`","step_key":"collect","agent":"a"}`); code != http.StatusForbidden {
		t.Fatalf("chat profile delegates: %d", code)
	}
	o.fails("step_delegate", map[string]any{"execution_id": x, "step_key": "commit", "agent": "runner"}, "step_not_delegable")
	o.fails("step_delegate", map[string]any{"execution_id": x, "step_key": "collect", "agent": "claude-haiku"}, "invalid_input")
	d := o.ok("step_delegate", map[string]any{"execution_id": x, "step_key": "collect", "agent": "harvester", "ttl_s": 3600})
	tok, _ := d["token"].(string)
	executor, _ := d["executor_actor_id"].(string)
	if !strings.HasPrefix(tok, auth.Prefix) || executor == "" || d["write_mode"] != "candidate" || d["depth"] != float64(1) {
		t.Fatalf("delegate: %v", d)
	}
	var stored int
	if err := app.QueryRow(sys, `SELECT count(*) FROM journal WHERE type = 'step.delegated' AND payload::text LIKE '%'||$1||'%'`, tok[len(auth.Prefix):]).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatal("the raw token reached the journal")
	}

	// what the executor's token can do
	e := connect(t, srv.URL, tok)
	// what the executor is listed: exactly what it may call, in the executor variant, within the
	// definition budget of a small model (≤ 3 000 tokens ≈ 9 000 characters); the parent still sees its whole profile
	listed, err := e.s.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	size := 0
	for _, lt := range listed.Tools {
		names = append(names, lt.Name)
		schema, _ := json.Marshal(lt.InputSchema)
		size += len(lt.Name) + len(lt.Title) + len(lt.Description) + len(schema)
		if lt.Name == "recall_search" && (len(lt.Description) > 300 || strings.Contains(string(schema), "include_folded")) {
			t.Errorf("recall_search is not listed in its executor variant: %d chars, %s", len(lt.Description), schema)
		}
	}
	slices.Sort(names)
	// collect: recall.search + statement.assert in candidate mode — the write is the step output, so not listed
	want := []string{"execution_read", "recall_search", "step_checkpoint", "step_finish", "step_pack", "step_start", "system_whoami"}
	if !slices.Equal(names, want) {
		t.Errorf("executor's tools/list: %v, want %v", names, want)
	}
	if size > 9000 {
		t.Errorf("executor's definitions: %d characters (≈ %d tokens), budget 9 000 (3 000 tokens)", size, size/3)
	}
	if parent, err := o.s.ListTools(ctx, nil); err != nil || len(parent.Tools) < 50 {
		t.Errorf("the parent's tools/list: %d tools, %v", len(parent.Tools), err)
	}
	e.fails("step_start", map[string]any{"execution_id": x, "step_key": "write"}, "outside_grant")
	e.fails("task_list", map[string]any{"project": "litopys-test"}, "not_in_grant")
	e.fails("statement_assert", map[string]any{"project": "litopys-test", "type": "note", "title": "t", "body": "b"}, "candidate_only")
	e.fails("step_delegate", map[string]any{"execution_id": x, "step_key": "collect", "agent": "deeper"}, "not_in_grant")
	e.fails("execution_start", map[string]any{"project": "litopys-test", "workflow": "harvest"}, "not_in_grant")
	// nothing past the native API: the viewer refuses an executor's grant — it would read the owner's whole memory past
	// derivedAllows
	login, _ := http.NewRequest(http.MethodPost, srv.URL+"/viewer/login", strings.NewReader(url.Values{"token": {tok}}.Encode()))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, err := noRedirect.Do(login); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an executor's grant signs in to the viewer: %v %v", resp.StatusCode, err)
	} else {
		resp.Body.Close()
	}
	pack := e.ok("step_pack", map[string]any{"execution_id": x, "step_key": "collect"})
	if pack["state"] != "submitted" {
		t.Fatalf("pack: %v", pack)
	}
	at := e.ok("step_start", map[string]any{"execution_id": x, "step_key": "collect"})
	e.ok("step_finish", map[string]any{"attempt_id": at["attempt_id"], "state": "completed", "output": map[string]any{"candidates": []any{3}}})
	read := e.ok("execution_read", map[string]any{"execution_id": x})
	steps := read["steps"].([]any)
	attempt := steps[0].(map[string]any)["attempts"].([]any)[0].(map[string]any)
	if attempt["actor_id"] != executor {
		t.Fatalf("the attempt is not the executor's: %v vs %s", attempt["actor_id"], executor)
	}
	// the token sees the run's project only
	e.fails("execution_read", map[string]any{"execution_id": uuid.NewString()}, "outside_grant")

	// cascade: revoking the parent kills the derived token
	po := connect(t, srv.URL, tokParent)
	dw := po.ok("step_delegate", map[string]any{"execution_id": x, "step_key": "write", "agent": "writer"})
	tokW := dw["token"].(string)
	if code := status(t, srv.URL, tokW, "/v1/step/pack", `{"execution_id":"`+x+`","step_key":"write"}`); code != http.StatusOK {
		t.Fatalf("derived token before revoke: %d", code)
	}
	if ok, err := auth.Revoke(ctx, app, tenant, parentID); err != nil || !ok {
		t.Fatalf("revoke parent: %v %v", ok, err)
	}
	if code := status(t, srv.URL, tokW, "/v1/step/pack", `{"execution_id":"`+x+`","step_key":"write"}`); code != http.StatusUnauthorized {
		t.Fatalf("derived token after its parent was revoked: %d", code)
	}

	// closing the run kills every grant of the run
	o.ok("execution_close", map[string]any{"execution_id": x, "outcome": "canceled"})
	if code := status(t, srv.URL, tok, "/v1/execution/read", `{"execution_id":"`+x+`"}`); code != http.StatusUnauthorized {
		t.Fatalf("derived token after the run closed: %d", code)
	}
}
