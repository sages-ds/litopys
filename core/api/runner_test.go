package api_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/executors"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/runner"
	"litopys.smart-gw.co.ua/core/server"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

const runnerDef = `{"steps": [
  {"key": "analyze", "title": "Find markers", "kind": "model", "verbs": ["recall.search"],
   "output": {"type": "object", "required": ["found"], "properties": {"found": {"type": "array", "items": {"type": "string"}}}},
   "acceptance": "every marker the step can see"}]}`

// fakeModel is a scripted OpenAI-compatible endpoint: mode "work" searches, then finishes with what it saw;
// mode "chatty" never calls a tool.
type fakeModel struct {
	mu       sync.Mutex
	mode     string
	requests []map[string]any
	auth     []string
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	mode := f.mode
	f.mu.Unlock()
	msgs := req["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	reply := map[string]any{"role": "assistant", "content": "thinking"}
	call := func(name string, args any) {
		b, _ := json.Marshal(args)
		reply = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": uuid.NewString(), "type": "function", "function": map[string]any{"name": name, "arguments": string(b)}}}}
	}
	if mode == "work" {
		if last["role"] == "tool" {
			var res struct {
				Items []struct {
					Title string `json:"title"`
				} `json:"items"`
			}
			_ = json.Unmarshal([]byte(last["content"].(string)), &res)
			found := []string{}
			for _, it := range res.Items {
				found = append(found, it.Title)
			}
			call(runner.FinishTool, map[string]any{"state": "completed", "output": map[string]any{"found": found}})
		} else {
			call("recall_search", map[string]any{"query": "marker"})
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": reply, "finish_reason": "stop"}}})
}

// TestRunner is the acceptance of litopys runner: it takes a ready model step for a registered
// executor, delegates it, runs the model with the step's tools and finish_step over the derived grant (the search
// sees only what the executor's class allows), finishes the attempt with the model's output, sends the provider key
// from the configured variable, and fails the attempt of a model that never calls finish_step.
func TestRunner(t *testing.T) {
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
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must("register", err)
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	_, _, err = v.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "work"})
	must("work", err)
	_, err = v.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "work", Type: "note", Title: "marker plain", Body: str("marker")})
	must("plain", err)
	_, err = v.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "work", Type: "note", Title: "marker secret", Body: str("marker"), DataClass: str("local")})
	must("secret", err)
	_, err = v.WorkflowDeclare(ctx, me, verbs.WorkflowDeclareIn{Project: "work", Key: "scan", Title: "Scan", Definition: json.RawMessage(runnerDef)})
	must("workflow", err)
	cloud, _, err := v.ActorRegister(ctx, me, verbs.ActorRegisterIn{Kind: "service", DisplayName: "cloud", ExternalIdentity: str("svc:cloud"),
		Executor: &verbs.ExecutorProfile{Provider: "test", Model: "fake", KeyKind: "paid", SecretRef: str("litopys/executors/test"), MaxDataClass: "internal"}})
	must("executor", err)
	tok, _, err := auth.IssueGrant(ctx, app, tenant, ada, "owner", "runner", nil, policy.Grant{Profile: "core"}, 0)
	must("token", err)

	h, err := server.New(app, admin, tenant).Handler()
	must("handler", err)
	srv := httptest.NewServer(h)
	defer srv.Close()
	fake := &fakeModel{mode: "work"}
	model := httptest.NewServer(fake)
	defer model.Close()
	t.Setenv("RUNNER_TEST_KEY", "test-key")

	o := connect(t, srv.URL, tok)
	x := o.ok("execution_start", map[string]any{"project": "work", "workflow": "scan"})["execution_id"].(string)

	cfg := runner.Config{LitopysURL: srv.URL, Executors: []runner.Executor{{ActorID: cloud, Name: "cloud", BaseURL: model.URL,
		Model: "fake", APIKeyEnv: "RUNNER_TEST_KEY"}}}
	cfgFile := t.TempDir() + "/runner.json"
	b, _ := json.Marshal(cfg)
	must("write config", os.WriteFile(cfgFile, b, 0o600))
	cfg, err = runner.LoadConfig(cfgFile)
	must("load config", err)
	r := runner.New(cfg, tok, nil)

	n, err := r.Once(ctx, cfg.Executors[0])
	if err != nil || n != 1 {
		t.Fatalf("once: %d finished, %v", n, err)
	}
	read := o.ok("execution_read", map[string]any{"execution_id": x})
	if read["execution"].(map[string]any)["state"] != "completed" {
		t.Errorf("run state: %v", read["execution"])
	}
	at := read["steps"].([]any)[0].(map[string]any)["attempts"].([]any)[0].(map[string]any)
	out, _ := json.Marshal(at["output"])
	if at["state"] != "completed" || at["actor_id"] != cloud.String() || !strings.Contains(string(out), "marker plain") ||
		strings.Contains(string(out), "marker secret") {
		t.Errorf("attempt: %v", at)
	}
	tools, _ := json.Marshal(fake.requests[0]["tools"])
	if !strings.Contains(string(tools), `"recall_search"`) || !strings.Contains(string(tools), `"finish_step"`) ||
		strings.Contains(string(tools), `"step_finish"`) || strings.Contains(string(tools), `"task_list"`) {
		t.Errorf("tools given to the model: %s", tools)
	}
	for _, a := range fake.auth {
		if a != "Bearer test-key" {
			t.Errorf("provider key header: %q", a)
		}
	}

	// a model that never calls finish_step: the attempt fails, the run stays open for a retry
	fake.mode = "chatty"
	x2 := o.ok("execution_start", map[string]any{"project": "work", "workflow": "scan"})["execution_id"].(string)
	n, err = r.Once(ctx, cfg.Executors[0])
	if err != nil || n != 1 {
		t.Fatalf("once (chatty): %d finished, %v", n, err)
	}
	read = o.ok("execution_read", map[string]any{"execution_id": x2})
	at = read["steps"].([]any)[0].(map[string]any)["attempts"].([]any)[0].(map[string]any)
	if at["state"] != "failed" || !strings.Contains(at["error"].(string), "finish_step") {
		t.Errorf("chatty attempt: %v", at)
	}

	// executors from the server: the person's key, sealed by the app, reaches the model; no file lists it
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	sealer, err := executors.NewSealer(base64.StdEncoding.EncodeToString(k))
	must("sealer", err)
	srvExec, _, err := v.ActorRegister(ctx, me, verbs.ActorRegisterIn{Kind: "service", DisplayName: "from server",
		Executor: &verbs.ExecutorProfile{Provider: "test", Model: "fake", KeyKind: "paid", MaxDataClass: "internal", BaseURL: str(model.URL)}})
	must("server executor", err)
	nonce, sealed, err := sealer.Seal("server-key", srvExec[:])
	must("seal", err)
	_, err = v.ExecutorKeySet(ctx, me, srvExec, executors.Fingerprint("server-key"), nonce, sealed)
	must("key", err)
	f2 := server.New(app, admin, tenant)
	f2.Sealer, f2.RunnerSecret = sealer, "runner-secret"
	h2, err := f2.Handler()
	must("handler 2", err)
	srv2 := httptest.NewServer(h2)
	defer srv2.Close()
	r2 := runner.New(runner.Config{LitopysURL: srv2.URL, ServerExecutors: true, PollS: 15, DelegateTTLS: 3600}, tok, nil)
	r2.Secret = "runner-secret"
	list, err := r2.ServerExecutors(ctx)
	must("server executors", err)
	if len(list) != 1 || list[0].ActorID != srvExec || list[0].Key != "server-key" {
		t.Fatalf("executors from the server: %+v", list)
	}
	fake.mode, fake.auth = "work", nil
	x3 := o.ok("execution_start", map[string]any{"project": "work", "workflow": "scan"})["execution_id"].(string)
	if n, err = r2.Once(ctx, list[0]); err != nil || n != 1 {
		t.Fatalf("once (server executor): %d finished, %v", n, err)
	}
	if st := o.ok("execution_read", map[string]any{"execution_id": x3})["execution"].(map[string]any)["state"]; st != "completed" {
		t.Errorf("run by the server executor: %v", st)
	}
	for _, a := range fake.auth {
		if a != "Bearer server-key" {
			t.Errorf("provider key header from the server: %q", a)
		}
	}
}
