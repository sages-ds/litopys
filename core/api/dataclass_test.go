package api_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
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

const classDef = `{"steps": [
  {"key": "collect", "title": "Collect", "kind": "model", "verbs": ["recall.search"], "max_data_class": "local"},
  {"key": "analyze", "title": "Analyze", "kind": "model", "verbs": ["recall.search"],
   "context_pack": {"selectors": ["steps.collect.output", "recall:marker"]}}]}`

// TestDelegationDataClass is the acceptance of Д9 part 2: the registry of executors (profile
// rules), step.open_items for an executor, step.delegate narrowing the grant to the executor's class and refusing a
// project above it, a session subagent without a profile, and outputs of a step above the grant's class withheld
// from execution.read and step.pack.
func TestDelegationDataClass(t *testing.T) {
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
	_, _, err = v.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "corp", DataClass: str("local")})
	must("corp", err)
	_, err = v.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "work", Type: "note", Title: "marker plain", Body: str("marker")})
	must("plain", err)
	_, err = v.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "work", Type: "note", Title: "marker secret", Body: str("marker"), DataClass: str("local")})
	must("secret", err)
	for _, p := range []string{"work", "corp"} {
		_, err := v.WorkflowDeclare(ctx, me, verbs.WorkflowDeclareIn{Project: p, Key: "pipe", Title: "Pipe", Definition: json.RawMessage(classDef)})
		must("workflow "+p, err)
	}

	// --- the registry: profile rules --------------------------------------------------
	reg := func(name string, x verbs.ExecutorProfile) (uuid.UUID, error) {
		id, _, err := v.ActorRegister(ctx, me, verbs.ActorRegisterIn{Kind: "service", DisplayName: name, ExternalIdentity: str("svc:" + name), Executor: &x})
		return id, err
	}
	no, yes := false, true
	if _, err := reg("bad-local", verbs.ExecutorProfile{Provider: "openai", Model: "m", KeyKind: "paid", MaxDataClass: "local"}); verbs.Code(err) != "invalid_input" {
		t.Errorf("a cloud model accepted for local data: %v", err)
	}
	if _, err := reg("bad-train", verbs.ExecutorProfile{Provider: "google", Model: "m", KeyKind: "free", MaxDataClass: "internal",
		DataPolicy: &verbs.ExecutorDataPolicy{Training: &yes}}); verbs.Code(err) != "invalid_input" {
		t.Errorf("a training provider accepted for internal data: %v", err)
	}
	if _, err := reg("bad-ref", verbs.ExecutorProfile{Provider: "openai", Model: "m", KeyKind: "paid", MaxDataClass: "internal",
		SecretRef: str("sk proj abc def")}); verbs.Code(err) != "invalid_input" {
		t.Errorf("a key value accepted as secret_ref: %v", err)
	}
	cloud, err := reg("cloud", verbs.ExecutorProfile{Provider: "openai", Model: "gpt", KeyKind: "paid", SecretRef: str("litopys/executors/openai"),
		MaxDataClass: "internal", DataPolicy: &verbs.ExecutorDataPolicy{Training: &no, Retention: &no}, TermsCheckedAt: str("2026-10-16")})
	must("cloud", err)
	local, err := reg("local", verbs.ExecutorProfile{Provider: "ollama", Model: "qwen", KeyKind: "local", MaxDataClass: "local"})
	must("local", err)

	issue := func() string {
		tok, _, err := auth.IssueGrant(ctx, app, tenant, ada, "owner", "orchestrator", nil, policy.Grant{Profile: "core"}, 0)
		must("issue", err)
		return tok
	}
	h, err := server.New(app, admin, tenant).Handler()
	must("handler", err)
	srv := httptest.NewServer(h)
	defer srv.Close()
	o := connect(t, srv.URL, issue())

	runWork := o.ok("execution_start", map[string]any{"project": "work", "workflow": "pipe"})["execution_id"].(string)
	runCorp := o.ok("execution_start", map[string]any{"project": "corp", "workflow": "pipe"})["execution_id"].(string)

	// --- step.open_items for an executor ------------------------------------------------------------
	open := func(executor uuid.UUID) []string {
		t.Helper()
		res := o.ok("step_open_items", map[string]any{"executor_actor_id": executor.String()})
		var runs []string
		for _, it := range res["items"].([]any) {
			runs = append(runs, it.(map[string]any)["execution_id"].(string))
		}
		return runs
	}
	if got := open(cloud); !slices.Contains(got, runWork) || slices.Contains(got, runCorp) {
		t.Errorf("open items of the cloud executor: %v (work %s, corp %s)", got, runWork, runCorp)
	}
	if got := open(local); !slices.Contains(got, runWork) || !slices.Contains(got, runCorp) {
		t.Errorf("open items of the local executor: %v", got)
	}

	// --- step.delegate narrows to the executor and refuses a project above it -----------------------
	o.fails("step_delegate", map[string]any{"execution_id": runCorp, "step_key": "collect", "actor_id": cloud.String()}, "data_class_exceeded")
	o.ok("step_delegate", map[string]any{"execution_id": runCorp, "step_key": "collect", "agent": "session-sub"}) // no profile: a subagent of the session

	dl := o.ok("step_delegate", map[string]any{"execution_id": runWork, "step_key": "collect", "actor_id": local.String()})
	el := connect(t, srv.URL, dl["token"].(string))
	titles := func(c *client) []string {
		t.Helper()
		res := c.ok("recall_search", map[string]any{"query": "marker"})
		var out []string
		for _, it := range res["items"].([]any) {
			out = append(out, it.(map[string]any)["title"].(string))
		}
		slices.Sort(out)
		return out
	}
	if got := titles(el); !slices.Equal(got, []string{"marker plain", "marker secret"}) {
		t.Errorf("the local executor finds %v", got)
	}
	at := el.ok("step_start", map[string]any{"execution_id": runWork, "step_key": "collect"})
	el.ok("step_finish", map[string]any{"attempt_id": at["attempt_id"], "state": "completed", "output": map[string]any{"found": "marker secret"}})

	dc := o.ok("step_delegate", map[string]any{"execution_id": runWork, "step_key": "analyze", "actor_id": cloud.String()})
	ec := connect(t, srv.URL, dc["token"].(string))
	if got := titles(ec); !slices.Equal(got, []string{"marker plain"}) {
		t.Errorf("the cloud executor finds %v", got)
	}
	pack := ec.ok("step_pack", map[string]any{"execution_id": runWork, "step_key": "analyze"})
	if _, ok := pack["inputs"].(map[string]any)["collect"]; ok {
		t.Errorf("the cloud executor's pack carries the local step's output: %v", pack["inputs"])
	}
	sel := pack["context"].([]any)[0].(map[string]any)
	if sel["value"] != nil || sel["unresolved"] == nil {
		t.Errorf("selector of the local step's output: %v", sel)
	}
	var recalled []string
	for _, it := range pack["context"].([]any)[1].(map[string]any)["value"].([]any) {
		recalled = append(recalled, it.(map[string]any)["title"].(string))
	}
	if !slices.Equal(recalled, []string{"marker plain"}) {
		t.Errorf("recall: selector of the cloud executor brings %v", recalled)
	}
	read := ec.ok("execution_read", map[string]any{"execution_id": runWork})
	collect := read["steps"].([]any)[0].(map[string]any)["attempts"].([]any)[0].(map[string]any)
	if collect["output"] != nil || collect["output_withheld"] != true {
		t.Errorf("execution.read of the cloud executor shows the local output: %v", collect)
	}
	// the orchestrator (no class) still reads it
	full := o.ok("execution_read", map[string]any{"execution_id": runWork})
	if full["steps"].([]any)[0].(map[string]any)["attempts"].([]any)[0].(map[string]any)["output"] == nil {
		t.Error("the orchestrator lost the output")
	}

	// --- actor.amend: lowering the local executor's profile takes the corp step away -----------------
	_, err = v.ActorAmend(ctx, me, verbs.ActorAmendIn{ActorID: local, Executor: json.RawMessage(
		`{"provider":"ollama","model":"qwen","key_kind":"local","max_data_class":"internal"}`)})
	must("amend local", err)
	if got := open(local); slices.Contains(got, runCorp) {
		t.Errorf("after the amend the local executor still gets the corp run: %v", got)
	}
	if _, err := v.ActorAmend(ctx, me, verbs.ActorAmendIn{ActorID: ada, Executor: json.RawMessage(`null`)}); verbs.Code(err) != "invalid_input" {
		t.Errorf("a person got an executor profile change: %v", err)
	}
}
