package api_test

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"litopys.smart-gw.co.ua/core/api"
	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/server"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// Acceptance of the native API v1 through the real HTTP stack of the facade binary:
// bearer tokens, /v1/mcp and POST /v1/<domain>/<verb>, on a throw-away database of the dev server.

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

type client struct {
	t *testing.T
	s *mcp.ClientSession
}

func connect(t *testing.T, url, token string) *client {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "api-gate", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url + "/v1/mcp",
		HTTPClient: &http.Client{Transport: bearer{token: token, base: http.DefaultTransport}}}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return &client{t: t, s: s}
}

func (c *client) call(name string, args map[string]any) (map[string]any, bool) {
	c.t.Helper()
	res, err := c.s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		c.t.Fatalf("%s: %v", name, err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &v); err != nil {
		c.t.Fatalf("%s: not a JSON object", name)
	}
	return v, res.IsError
}

func (c *client) ok(name string, args map[string]any) map[string]any {
	c.t.Helper()
	v, isErr := c.call(name, args)
	if isErr {
		c.t.Fatalf("%s(%v): %v", name, args, v["error"])
	}
	return v
}

// fails expects a tool error with the code and returns it.
func (c *client) fails(name string, args map[string]any, code string) map[string]any {
	c.t.Helper()
	v, isErr := c.call(name, args)
	e, _ := v["error"].(map[string]any)
	if !isErr || e["code"] != code {
		c.t.Fatalf("%s: want error %s, got %v", name, code, v)
	}
	return e
}

func (c *client) tools() map[string]bool {
	c.t.Helper()
	res, err := c.s.ListTools(context.Background(), nil)
	if err != nil {
		c.t.Fatal(err)
	}
	m := map[string]bool{}
	for _, t := range res.Tools {
		m[t.Name] = true
	}
	return m
}

func sessionOf(t *testing.T, v map[string]any) string {
	t.Helper()
	s, _ := v["session"].(map[string]any)
	id, _ := s["session_id"].(string)
	if id == "" {
		t.Fatalf("no session in %v", v)
	}
	return id
}

func TestNativeAPI(t *testing.T) {
	// one person drives the whole surface here: well over the per-minute budget of real use (its own test: facade)
	old := server.RateCalls
	server.RateCalls = 1000
	t.Cleanup(func() { server.RateCalls = old })
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
	issue := func(profile string) string {
		tok, _, err := auth.IssueGrant(ctx, app, tenant, ada, "owner", "api-gate "+profile, nil, policy.Grant{Profile: profile}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	tokA, tokB, tokChat, tokRO := issue(""), issue(""), issue("chat"), issue("readonly")
	h, err := server.New(app, admin, tenant).Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	a := connect(t, srv.URL, tokA)
	b := connect(t, srv.URL, tokB)

	// --- surfaces by profile -------------------------------------------------------------------------
	full, chat, ro := a.tools(), connect(t, srv.URL, tokChat).tools(), connect(t, srv.URL, tokRO).tools()
	if !full["project_resume"] || !full["task_list"] || full["journal_purge"] || full["system_health"] {
		t.Errorf("owner/full surface: %v", full)
	}
	if len(chat) != 16 || chat["task_list"] || !chat["system_guide"] || !ro["system_guide"] || !full["system_guide"] {
		t.Errorf("chat surface (%d): %v", len(chat), chat)
	}
	// --- the any-client guide: the tool in chat, the rules block in every resume ---------------
	cc := connect(t, srv.URL, tokChat)
	if g := cc.ok("system_guide", map[string]any{}); g["topic"] != "index" || !strings.Contains(g["text"].(string), "cross-project") {
		t.Errorf("system_guide index: %v", g)
	}
	if g := cc.ok("system_guide", map[string]any{"topic": "close"}); g["topic"] != "close" || !strings.Contains(g["text"].(string), "session_close") {
		t.Errorf("system_guide close: %v", g)
	}
	rulesOf := func(v map[string]any) []any {
		r, _ := v["rules"].(map[string]any)
		l, _ := r["lines"].([]any)
		if r["version"] != api.Guide.Version || !strings.HasPrefix(fmt.Sprint(r["guide"]), "system_guide") {
			t.Errorf("rules block: %v", r)
		}
		return l
	}
	if l := rulesOf(a.ok("project_resume", map[string]any{})); len(l) != len(api.Guide.Rules.Lines) {
		t.Errorf("project_resume catalog rules: %v", l)
	}
	if l := rulesOf(a.ok("project_resume", map[string]any{"project": "guide-lab", "auto_create": true})); len(l) != len(api.Guide.Rules.Lines) {
		t.Errorf("project_resume rules: %v", l)
	}
	if l := rulesOf(a.ok("session_open", map[string]any{"project": "guide-lab", "attach": "none"})); len(l) != len(api.Guide.Rules.Lines) {
		t.Errorf("session_open rules: %v", l)
	}
	if ro["statement_assert"] || !ro["recall_search"] || !ro["project_resume"] {
		t.Errorf("readonly surface: %v", ro)
	}
	// --- actor.list: admin only; who has access, with tokens and grants -----------------
	if full["actor_list"] || chat["actor_list"] {
		t.Errorf("actor_list is visible without the admin role")
	}
	tokAdmin, _, err := auth.IssueGrant(ctx, app, tenant, ada, "admin", "api-gate admin", nil, policy.Grant{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	adm := connect(t, srv.URL, tokAdmin)
	al := adm.ok("actor_list", map[string]any{"query": "ADA"})
	items, _ := al["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("actor_list query: %v", al)
	}
	me := items[0].(map[string]any)
	if me["display_name"] != "Ada" || me["email"] != "ada@example" || me["role"] != "admin" || me["kind"] != "human" {
		t.Errorf("actor_list row: %v", me)
	}
	if tk, _ := me["tokens"].(map[string]any); tk == nil || tk["active"].(float64) != 5 || tk["last_used_at"] == nil {
		t.Errorf("actor_list tokens (4 issued + admin, used): %v", me["tokens"])
	}
	if o, _ := me["oauth"].([]any); o == nil || len(o) != 0 {
		t.Errorf("actor_list oauth: %v", me["oauth"])
	}
	if al := adm.ok("actor_list", map[string]any{"kind": "agent", "query": "ada"}); len(al["items"].([]any)) != 0 {
		t.Errorf("actor_list kind filter: %v", al)
	}
	// HTTP projection: a tool outside the profile is refused with a code, not hidden
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/recall/timeline", strings.NewReader(`{"project":"x"}`))
	req.Header.Set("Authorization", "Bearer "+tokChat)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var herr map[string]map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&herr)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || herr["error"]["code"] != "not_in_profile" {
		t.Errorf("chat → recall_timeline over HTTP: %d %v", resp.StatusCode, herr)
	}

	// --- project_resume: catalog, then auto_create from a human title -------------------------------
	cat := a.ok("project_resume", nil)
	if l, _ := cat["catalog"].([]any); l == nil {
		t.Fatalf("catalog: %v", cat)
	}
	r1 := a.ok("project_resume", map[string]any{"project": "Квартира Сад", "auto_create": true,
		"client": map[string]any{"surface": "claude.ai-web", "client_type": "chat"}})
	sA1 := sessionOf(t, r1)
	// the client's own surface is kept as its label, the server adds the app (roast r1)
	for _, it := range a.ok("session_list", map[string]any{"project": "kvartyra-sad"})["items"].([]any) {
		if m := it.(map[string]any); m["session_id"] == sA1 {
			if cl := m["client"].(map[string]any); cl["label"] != "claude.ai-web" || cl["surface"] != "api/v1" || cl["app"] == nil || cl["client_type"] != "chat" {
				t.Errorf("client info of the session: %v", cl)
			}
		}
	}
	if r1["project_created"] != true {
		t.Errorf("auto_create: %v", r1)
	}
	bundle := r1["bundle"].(map[string]any)
	if p := bundle["project"].(map[string]any); p["name"] != "kvartyra-sad" || p["title"] != "Квартира Сад" {
		t.Errorf("slug and title: %v", p)
	}
	if again := a.ok("project_resume", map[string]any{"project": "квартира сад"}); sessionOf(t, again) != sA1 {
		t.Errorf("a repeated resume of the same client must reuse its session")
	}

	// --- writes bind to the session of their own client ------------------------------------------------
	w := a.ok("statement_assert", map[string]any{"project": "kvartyra-sad", "type": "decision",
		"title": "Split the flat 1/2 + 1/2", "body": "Agreed with my sister. See task #1."})
	if w["session_id"] != sA1 || w["session_inherited"] != true {
		t.Errorf("statement without session_id: %v (session %s)", w, sA1)
	}
	// a retry of the same record (a lost answer, a 502) returns the one already written
	if again := a.ok("statement_assert", map[string]any{"project": "kvartyra-sad", "type": "decision",
		"title": "Split the flat 1/2 + 1/2", "body": "Agreed with my sister. See task #1."}); again["deduped"] != true || again["statement_id"] != w["statement_id"] {
		t.Errorf("a repeated statement_assert: %v", again)
	}
	sB := sessionOf(t, b.ok("project_resume", map[string]any{"project": "kvartyra-sad"}))
	if sB == sA1 {
		t.Fatal("another grant reused the first client's session")
	}
	if wb := b.ok("statement_assert", map[string]any{"project": "kvartyra-sad", "type": "note", "title": "From client B", "body": "b"}); wb["session_id"] != sB {
		t.Errorf("client B wrote into %v, want its own %s", wb["session_id"], sB)
	}

	// two open sessions of one client: no binding, a warning
	sA2 := sessionOf(t, a.ok("session_open", map[string]any{"project": "kvartyra-sad", "attach": "none"}))
	w2 := a.ok("statement_assert", map[string]any{"project": "kvartyra-sad", "type": "note", "title": "Ambiguous", "body": "x"})
	if w2["session_id"] != nil || w2["warnings"] == nil {
		t.Errorf("two open sessions: %v", w2)
	}
	// a closed session takes no more writes; the rule picks the remaining open one
	a.ok("session_close", map[string]any{"session_id": sA1, "summary": "first part done"})
	w3 := a.ok("statement_assert", map[string]any{"project": "kvartyra-sad", "type": "note", "title": "After close", "body": "x", "session_id": sA1})
	if w3["session_miss"] != "closed" || w3["session_id"] != sA2 {
		t.Errorf("write into a closed session: %v (open %s)", w3, sA2)
	}

	// --- a write no session takes keeps its client (the detached run), and so does project.declared ---
	a.ok("session_close", map[string]any{"session_id": sA2, "summary": "second part done"})
	runOf := func(entity string) (run, actor uuid.UUID, grant, purpose string) {
		t.Helper()
		err := app.QueryRow(sys, `SELECT j.run_id, j.actor_id, COALESCE(r.client->>'grant', ''), COALESCE(r.purpose, '')
		   FROM journal j LEFT JOIN projection.run r ON r.run_id = j.run_id
		  WHERE j.entity_id = $1 ORDER BY j.seq LIMIT 1`, entity).Scan(&run, &actor, &grant, &purpose)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	u1 := a.ok("statement_assert", map[string]any{"project": "kvartyra-sad", "type": "note", "title": "Unbound 1", "body": "x"})
	u2 := a.ok("statement_assert", map[string]any{"project": "kvartyra-sad", "type": "note", "title": "Unbound 2", "body": "x", "session_id": sA1})
	if u1["session_id"] != nil || u2["session_miss"] != "closed" {
		t.Errorf("no open session: %v / %v", u1, u2)
	}
	r1run, r1actor, r1grant, r1purpose := runOf(u1["statement_id"].(string))
	r2run, _, _, _ := runOf(u2["statement_id"].(string))
	if r1run == r1actor || r1grant == "" || r1purpose != "detached" || r2run != r1run {
		t.Errorf("unbound writes: run %s (actor %s), grant %q, purpose %q; second run %s", r1run, r1actor, r1grant, r1purpose, r2run)
	}
	pid := bundle["project"].(map[string]any)["project_id"].(string)
	if prun, pactor, pgrant, _ := runOf(pid); prun == pactor || pgrant == "" {
		t.Errorf("project.declared by project_resume auto_create: run %s (actor %s), grant %q", prun, pactor, pgrant)
	}

	// --- the catalog of project_resume is compact — a long summary is cut and marked ------------------
	a.ok("project_declare", map[string]any{"name": "long-summary", "summary": strings.Repeat("Довгий опис проєкту. ", 60)})
	for _, row := range a.ok("project_resume", nil)["catalog"].([]any) {
		r := row.(map[string]any)
		s, _ := r["summary"].(string)
		if n := len([]rune(s)); n > 161 {
			t.Errorf("catalog row %v: summary of %d characters", r["name"], n)
		}
		if r["name"] == "long-summary" && r["summary_truncated"] != true {
			t.Errorf("long summary not marked truncated: %v", r)
		}
	}

	// --- P2: folder bindings are facts of kind binding — listed across projects, kept out of the pack
	a.ok("fact_record", map[string]any{"project": "kvartyra-sad", "kind": "binding", "key": "host-1:0123456789abcdef",
		"value": `{"host":"host-1","path":"C:\\work\\sad"}`})
	a.ok("fact_record", map[string]any{"project": "kvartyra-sad", "kind": "instruction", "key": "language", "value": "Answer in Ukrainian."})
	if l := a.ok("fact_list", map[string]any{"kind": "binding"})["items"].([]any); len(l) != 1 {
		t.Errorf("bindings across projects: %v", l)
	}
	pk := a.ok("project_resume", map[string]any{"project": "kvartyra-sad"})["bundle"].(map[string]any)
	for _, f := range pk["facts"].([]any) {
		if f.(map[string]any)["kind"] == "binding" {
			t.Errorf("a binding reached the start package: %v", f)
		}
	}

	// --- P3: the resolved stack global → projects → <project>, narrower wins, owner's rules only ---
	g := a.ok("project_declare", map[string]any{"name": "my-global"})
	a.ok("project_amend", map[string]any{"project": "my-global", "labels": []string{"global"}})
	a.ok("fact_record", map[string]any{"project": "my-global", "kind": "instruction", "key": "language", "value": "Answer in English."})
	a.ok("fact_record", map[string]any{"project": "my-global", "kind": "style", "key": "tone", "value": "short"})
	a.ok("fact_record", map[string]any{"project": "my-global", "kind": "skill", "key": "deploy", "value": "how to deploy"})
	sp := a.ok("project_declare", map[string]any{"name": "household"})
	a.ok("project_amend", map[string]any{"project": "household", "labels": []string{"space"}})
	a.ok("fact_record", map[string]any{"project": "household", "kind": "style", "key": "tone", "value": "formal"})
	pr := a.ok("project_resume", map[string]any{"project": "kvartyra-sad"})["bundle"].(map[string]any)["project"].(map[string]any)
	a.ok("link_assert", map[string]any{"relation": "member_of",
		"from": map[string]any{"entity_type": "project", "entity_id": pr["project_id"]},
		"to":   map[string]any{"entity_type": "project", "entity_id": sp["project_id"]}})
	_ = g

	// --- links to other projects in project_resume: both directions, member_of is not one ---
	if _, ok := a.ok("project_resume", map[string]any{"project": "kvartyra-sad"})["links"]; ok {
		t.Errorf("member_of reached the links block of project_resume")
	}
	up := a.ok("project_declare", map[string]any{"name": "links-upstream"})
	a.ok("link_assert", map[string]any{"relation": "depends_on",
		"from": map[string]any{"entity_type": "project", "entity_id": pr["project_id"]},
		"to":   map[string]any{"entity_type": "project", "entity_id": up["project_id"]}})
	linksOf := func(project string) []any {
		lb, _ := a.ok("project_resume", map[string]any{"project": project})["links"].(map[string]any)
		items, _ := lb["items"].([]any)
		return items
	}
	if l := linksOf("kvartyra-sad"); len(l) != 1 || fmt.Sprint(l[0]) != "map[to:links-upstream type:depends_on]" {
		t.Errorf("outgoing project link: %v", l)
	}
	if l := linksOf("links-upstream"); len(l) != 1 || fmt.Sprint(l[0]) != "map[from:kvartyra-sad type:depends_on]" {
		t.Errorf("incoming project link: %v", l)
	}

	lb := a.ok("project_resume", map[string]any{"project": "kvartyra-sad"})["bundle"].(map[string]any)
	layers, _ := lb["layers"].(map[string]any)
	if layers == nil || layers["global"] != "my-global" || len(layers["spaces"].([]any)) != 1 {
		t.Fatalf("layers: %v", layers)
	}
	stack := map[string]map[string]any{}
	for _, r := range layers["stack"].([]any) {
		m := r.(map[string]any)
		stack[m["kind"].(string)+"/"+m["key"].(string)] = m
	}
	// project overrides global (language), the space overrides global (tone), global alone (deploy)
	if r := stack["instruction/language"]; r["value"] != "Answer in Ukrainian." || r["layer"] != "project" {
		t.Errorf("instruction/language: %v", r)
	}
	if r := stack["style/tone"]; r["value"] != "formal" || r["source"] != "household" || r["layer"] != "projects" {
		t.Errorf("style/tone: %v", r)
	}
	if r := stack["skill/deploy"]; r["layer"] != "global" {
		t.Errorf("skill/deploy: %v", r)
	}
	for _, f := range lb["facts"].([]any) {
		if k := f.(map[string]any)["kind"]; k == "instruction" || k == "style" {
			t.Errorf("a layer rule stayed among the facts: %v", f)
		}
	}
	// a rule a sub-agent records is a proposal: it stays out of the stack (the acceptance of sub-agent proposals)
	agentSession := sessionOf(t, b.ok("project_resume", map[string]any{"project": "household", "agent": "planner"}))
	_ = agentSession
	b.ok("fact_record", map[string]any{"project": "household", "kind": "instruction", "key": "agent-rule", "value": "Always deploy on Fridays."})
	lb2 := a.ok("project_resume", map[string]any{"project": "kvartyra-sad"})["bundle"].(map[string]any)
	for _, r := range lb2["layers"].(map[string]any)["stack"].([]any) {
		if r.(map[string]any)["key"] == "agent-rule" {
			t.Errorf("a sub-agent's rule entered the stack: %v", r)
		}
	}
	// P4: layer_list gives the same stack without opening a session
	before := len(a.ok("session_list", map[string]any{"project": "kvartyra-sad"})["items"].([]any))
	ll := a.ok("layer_list", map[string]any{"project": "Квартира Сад"})
	if len(ll["stack"].([]any)) != len(lb2["layers"].(map[string]any)["stack"].([]any)) || ll["global"] != "my-global" {
		t.Errorf("layer_list: %v", ll)
	}
	if after := len(a.ok("session_list", map[string]any{"project": "kvartyra-sad"})["items"].([]any)); after != before {
		t.Errorf("layer_list opened a session: %d -> %d", before, after)
	}

	// --- titles: ambiguity is an error with the candidates ----------------------------------------------
	a.ok("project_declare", map[string]any{"name": "dup-a", "title": "Dup"})
	a.ok("project_declare", map[string]any{"name": "dup-b", "title": "Dup"})
	a.fails("recall_search", map[string]any{"project": "dup", "query": "x"}, "project_ambiguous")

	// --- agent roles and proposals --------------------------------------------------------------------
	e := a.fails("project_resume", map[string]any{"project": "kvartyra-sad", "agent": "gpt-4o"}, "invalid_input")
	if !strings.Contains(e["hint"].(string), "role") {
		t.Errorf("agent hint: %v", e)
	}
	sub := sessionOf(t, a.ok("project_resume", map[string]any{"project": "kvartyra-sad", "agent": "reviewer"}))
	p := a.ok("statement_assert", map[string]any{"project": "kvartyra-sad", "type": "proposal", "session_id": sub,
		"title": "Sell instead of splitting", "body": "Reviewer's recommendation."})
	found := a.ok("recall_read", map[string]any{"ids": []string{p["statement_id"].(string)}})
	if item := found["items"].([]any)[0].(map[string]any); item["type"] != "proposal" || item["authority"] != "agent" {
		t.Errorf("proposal by a sub-agent: %v", item)
	}

	// --- tasks by number, task_resume -------------------------------------------------------------------
	td := a.ok("task_declare", map[string]any{"project": "Квартира Сад", "title": "Order the valuation", "priority": "high"})
	if td["num"].(float64) != 1 {
		t.Fatalf("task_declare: %v", td)
	}
	// the same title again with other fields: the task is returned and the fields it did not apply are named
	if again := a.ok("task_declare", map[string]any{"project": "kvartyra-sad", "title": "Order the valuation", "priority": "low", "body": "x"}); again["existed"] != true ||
		fmt.Sprint(again["ignored_fields"]) != "[body priority]" {
		t.Errorf("task_declare of an existing title: %v", again)
	}
	a.ok("task_amend", map[string]any{"project": "kvartyra-sad", "num": 1, "status": "in_progress"})
	tr := a.ok("task_resume", map[string]any{"project": "kvartyra-sad", "num": 1})
	if tr["task"].(map[string]any)["status"] != "in_progress" || len(tr["related"].([]any)) == 0 {
		t.Errorf("task_resume: %v", tr)
	}
	a.ok("task_close", map[string]any{"project": "kvartyra-sad", "num": 1, "outcome": "done", "resolution": "ordered"})

	// --- session_resume and topic_resume ---------------------------------------------------------------
	sr := a.ok("session_resume", map[string]any{"session_id": sA1})
	if sr["resumed_from"] == nil || sr["summary"] != "first part done" || len(sr["statements"].([]any)) == 0 {
		t.Errorf("session_resume of a closed session: %v", sr)
	}
	tp := a.ok("topic_resume", map[string]any{"topic": "split flat", "project": "kvartyra-sad", "mode": "preflight"})
	if len(tp["decisions"].([]any)) == 0 || tp["open_tasks"] == nil {
		t.Errorf("topic_resume: %v", tp)
	}
	// preflight finds tasks and facts by the stems of the topic's words, not the whole phrase (roast r1)
	a.ok("task_declare", map[string]any{"project": "kvartyra-sad", "title": "Подзвонити газовику щодо відключення плити"})
	a.ok("fact_record", map[string]any{"project": "kvartyra-sad", "kind": "note", "key": "budget-cap", "value": "Ліміт на плиту — 30 тис."})
	tp2 := a.ok("topic_resume", map[string]any{"topic": "плита газ індукція", "project": "kvartyra-sad", "mode": "preflight"})
	if ot, _ := tp2["open_tasks"].([]any); len(ot) != 1 {
		t.Errorf("preflight open_tasks by stems: %v", tp2["open_tasks"])
	}
	if fs, _ := tp2["facts"].([]any); len(fs) != 1 {
		t.Errorf("preflight facts by stems: %v", tp2["facts"])
	}

	// --- readonly: resume without a session, no writes ---------------------------------------------------
	rc := connect(t, srv.URL, tokRO)
	rr := rc.ok("project_resume", map[string]any{"project": "kvartyra-sad"})
	if rr["session"] != nil || rr["bundle"] == nil {
		t.Errorf("readonly resume: %v", rr)
	}
	// open_session false: a look at the state opens nothing, from a client that has no session there (roast r1)
	peekOpen := len(b.ok("session_list", map[string]any{"project": "kvartyra-sad"})["items"].([]any))
	peek := b.ok("project_resume", map[string]any{"project": "kvartyra-sad", "open_session": false})
	b.ok("topic_resume", map[string]any{"topic": "split flat", "project": "kvartyra-sad", "open_session": false})
	if peek["session"] != nil || peek["bundle"] == nil || len(b.ok("session_list", map[string]any{"project": "kvartyra-sad"})["items"].([]any)) != peekOpen {
		t.Errorf("project_resume open_session false: %v", peek["session"])
	}

	// --- secrets are refused before the write ------------------------------------------------------------
	a.fails("statement_assert", map[string]any{"project": "kvartyra-sad", "type": "note", "title": "token",
		"body": "ghp_" + strings.Repeat("a1B2", 9)}, "secret_rejected")
	// the same cases are refused by every write verb, not only by some (fact_record once refused,
	// statement_assert accepted "Пароль: sk-test-0000-FAKE")
	for _, secret := range []string{"Пароль: sk-test-0000-FAKE", "password=hunter22", "API-ключ: k3y-Abc-12345",
		"Authorization: Bearer " + strings.Repeat("aB3", 8), "ghp_" + strings.Repeat("a1B2", 9)} {
		a.fails("statement_assert", map[string]any{"project": "kvartyra-sad", "type": "note", "title": "creds", "body": secret}, "secret_rejected")
		a.fails("statement_assert", map[string]any{"project": "kvartyra-sad", "type": "note", "title": secret}, "secret_rejected")
		a.fails("task_declare", map[string]any{"project": "kvartyra-sad", "title": "Rotate", "body": secret}, "secret_rejected")
		a.fails("fact_record", map[string]any{"project": "kvartyra-sad", "kind": "instruction", "key": "db", "value": secret}, "secret_rejected")
		a.fails("session_checkpoint", map[string]any{"project": "kvartyra-sad", "summary": secret}, "secret_rejected")
	}
	a.ok("statement_assert", map[string]any{"project": "kvartyra-sad", "type": "note", "title": "Where the DB password lives",
		"body": "пароль: ${DB_PASS} — в OpenBao; токен: оновлено 2026-10-20"})

	// --- outcomes on session_close -------------------------------------------------------
	resO := a.ok("project_resume", map[string]any{"project": "Outcomes Lab", "auto_create": true})
	sO := sessionOf(t, resO)

	// --- the vocabulary block and the vocabulary verbs -------------------------------
	vb, _ := resO["vocabulary"].(map[string]any)
	if sets, _ := vb["sets"].([]any); len(sets) != 1 || sets[0] != "general" || vb["hint"] == "" {
		t.Errorf("project_resume vocabulary block: %v", vb)
	}
	if types, _ := vb["types"].([]any); len(types) == 0 || types[0].(map[string]any)["family"] == nil {
		t.Errorf("vocabulary block types: %v", vb["types"])
	}
	enabled := map[string]any{}
	for _, v := range a.ok("vocabulary_list", map[string]any{"project": "outcomes-lab"})["vocabularies"].([]any) {
		m := v.(map[string]any)
		enabled[m["key"].(string)] = m["enabled"]
	}
	if enabled["general"] != true || enabled["dba"] != false {
		t.Errorf("vocabulary_list enabled flags: %v", enabled)
	}
	if bare := a.ok("vocabulary_list", nil)["vocabularies"].([]any); bare[0].(map[string]any)["enabled"] != nil {
		t.Errorf("vocabulary_list without a project marks enabled: %v", bare[0])
	}
	if d := a.ok("vocabulary_read", map[string]any{"key": "dba"}); d["key"] != "dba" || len(d["types"].([]any)) == 0 ||
		d["types"].([]any)[0].(map[string]any)["type"] == nil {
		t.Errorf("vocabulary_read dba: %v", d)
	}
	// session_open carries the same block, also for a project it creates (the id is resolved after auto_create)
	if so := a.ok("session_open", map[string]any{"project": "Vocabulary Lab", "attach": "none", "auto_create": true}); so["vocabulary"] == nil ||
		fmt.Sprint(so["vocabulary"].(map[string]any)["sets"]) != "[general]" {
		t.Errorf("session_open vocabulary block: %v", so["vocabulary"])
	}
	// reads by family and by a resolved type: a decision of the exec vocabulary is a decision
	a.ok("fact_record", map[string]any{"project": "outcomes-lab", "kind": "vocabulary", "key": "exec", "value": "on"})
	board := a.ok("statement_assert", map[string]any{"project": "outcomes-lab", "type": "board-decision", "title": "Quarterly budget approved",
		"body": "the board approved the budget"})
	if board["type"] != "exec.board-decision" {
		t.Errorf("board-decision stored as %v", board["type"])
	}
	hasBoard := func(res map[string]any, key string) bool {
		items, _ := res[key].([]any)
		for _, it := range items {
			if m, _ := it.(map[string]any); m["statement_id"] == board["statement_id"] {
				return true
			}
		}
		return false
	}
	if !hasBoard(a.ok("recall_search", map[string]any{"project": "outcomes-lab", "type": "board-decision"}), "items") {
		t.Error("recall_search by the short type misses the record")
	}
	if !hasBoard(a.ok("recall_search", map[string]any{"project": "outcomes-lab", "family": "decision"}), "items") ||
		hasBoard(a.ok("recall_search", map[string]any{"project": "outcomes-lab", "family": "finding"}), "items") {
		t.Error("recall_search by family")
	}
	if !hasBoard(a.ok("topic_resume", map[string]any{"project": "outcomes-lab", "topic": "quarterly budget"}), "decisions") {
		t.Error("topic_resume does not group a vocabulary decision under decisions")
	}
	a.fails("recall_search", map[string]any{"family": "nonsense"}, "invalid_input")
	// has: the statement is in a list of rows (timeline rows are wrapped in "row")
	has := func(list any, id any) bool {
		items, _ := list.([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			if row, ok := m["row"].(map[string]any); ok {
				m = row
			}
			if m["statement_id"] == id {
				return true
			}
		}
		return false
	}
	if !has(a.ok("recall_timeline", map[string]any{"project": "outcomes-lab", "family": "decision"})["items"], board["statement_id"]) {
		t.Error("recall_timeline by family misses the record")
	}
	if !has(a.ok("recall_as_of", map[string]any{"project": "outcomes-lab", "effective_at": "2100-01-01T00:00:00Z", "family": "decision"})["items"],
		board["statement_id"]) {
		t.Error("recall_as_of by family misses the record")
	}
	// an old name that lands on a general type does not widen the filter to the family: a plain finding stays out
	plain := a.ok("statement_assert", map[string]any{"project": "outcomes-lab", "type": "finding", "title": "A plain finding", "body": "x"})
	if has(a.ok("recall_search", map[string]any{"project": "outcomes-lab", "type": "discovery"})["items"], plain["statement_id"]) {
		t.Error("type discovery matched every finding")
	}
	// a question of a vocabulary is an open question; an ambiguous short type is refused on a read too
	a.ok("fact_record", map[string]any{"project": "outcomes-lab", "kind": "vocabulary", "key": "pm", "value": "on"})
	pmReq := a.ok("statement_assert", map[string]any{"project": "outcomes-lab", "type": "requirement", "title": "Export to CSV", "body": "asked by finance"})
	if !has(a.ok("work_open_items", map[string]any{"project": "outcomes-lab"})["questions"], pmReq["statement_id"]) {
		t.Errorf("work_open_items misses the pm.requirement question (%v)", pmReq["type"])
	}
	// an answer resolves the question: it leaves the open ones, topic_resume shows it as resolved (roast r1)
	ans := a.ok("statement_assert", map[string]any{"project": "outcomes-lab", "type": "decision", "title": "CSV export in v2", "body": "finance agreed"})
	a.ok("link_assert", map[string]any{"relation": "resolves",
		"from": map[string]any{"entity_type": "statement", "entity_id": ans["statement_id"]},
		"to":   map[string]any{"entity_type": "statement", "entity_id": pmReq["statement_id"]}})
	if has(a.ok("work_open_items", map[string]any{"project": "outcomes-lab"})["questions"], pmReq["statement_id"]) {
		t.Error("a resolved question is still open")
	}
	if tr := a.ok("topic_resume", map[string]any{"topic": "Export CSV", "project": "outcomes-lab"}); !has(tr["resolved"], pmReq["statement_id"]) || has(tr["questions"], pmReq["statement_id"]) {
		t.Errorf("topic_resume of a resolved question: %v", tr)
	}
	a.ok("fact_record", map[string]any{"project": "outcomes-lab", "kind": "vocabulary", "key": "rnd", "value": "on"})
	a.fails("recall_search", map[string]any{"project": "outcomes-lab", "type": "requirement"}, "type_ambiguous")
	a.fails("vocabulary_read", map[string]any{"key": "nonsense"}, "vocabulary_not_found")
	a.fails("vocabulary_read", map[string]any{"key": "dba", "version": 99}, "vocabulary_not_found")
	for _, title := range []string{"Valuation report", "Notary appointment", "Old idea", "Insurance"} {
		a.ok("task_declare", map[string]any{"project": "outcomes-lab", "title": title})
	}
	a.ok("task_close", map[string]any{"project": "outcomes-lab", "num": 3, "outcome": "dropped", "resolution": "not needed"})
	recOut := a.ok("statement_assert", map[string]any{"project": "outcomes-lab", "type": "discovery", "title": "Search latency spikes",
		"body": "p95 over budget"})
	rec := recOut["statement_id"].(string)
	// an old type name lands on its new type with a warning next to the session binding
	if w, _ := recOut["warnings"].([]any); recOut["type"] != "finding" || len(w) == 0 || !strings.HasPrefix(fmt.Sprint(w[len(w)-1]), "type_legacy_alias") {
		t.Errorf("statement_assert with an old type: type %v, warnings %v", recOut["type"], recOut["warnings"])
	}
	closeWith := func(outcomes ...map[string]any) map[string]any {
		list := make([]any, len(outcomes))
		for i, o := range outcomes {
			list[i] = o
		}
		return map[string]any{"session_id": sO, "summary": "lab day", "outcomes": list}
	}
	// refused before anything is written: unknown subject, unknown form, no next step, a subject twice, an outcome by hand
	a.fails("session_close", closeWith(map[string]any{"ref": "T-99", "result": "r", "state": "done"}), "task_not_found")
	a.fails("session_close", closeWith(map[string]any{"ref": "the flat", "result": "r", "state": "done"}), "invalid_ref")
	a.fails("session_close", closeWith(map[string]any{"ref": "T-1", "result": "r", "state": "partial"}), "invalid_input")
	a.fails("session_close", closeWith(map[string]any{"ref": "T-1", "result": "a", "state": "done"},
		map[string]any{"ref": "t-1", "result": "b", "state": "done"}), "invalid_input")
	a.fails("statement_assert", map[string]any{"project": "outcomes-lab", "type": "outcome", "title": "by hand", "body": "x"}, "type_reserved")
	closed := a.ok("session_close", map[string]any{"session_id": sO, "summary": "lab day", "next_steps": []string{"Call the notary"},
		"outcomes": []any{
			map[string]any{"ref": "T-1", "result": "Report ordered", "state": "partial", "next": "Pay the invoice", "evidence": []string{"invoice 17"}},
			map[string]any{"ref": "T-2", "result": "Appointment booked", "state": "done"},
			map[string]any{"ref": rec, "result": "p95 under budget", "state": "done"},
			map[string]any{"ref": "topic:valuation", "result": "Two quotes compared", "state": "done"},
			map[string]any{"ref": "kvartyra-sad/T-1", "result": "Valuation closed earlier", "state": "done"},
		}})
	written, _ := closed["outcomes"].([]any)
	missing, _ := closed["missing_outcomes"].([]any)
	hints, _ := closed["hints"].([]any)
	if closed["closed"] != true || len(written) != 5 || len(missing) != 1 || missing[0] != "T-4" || len(hints) != 1 ||
		!strings.HasPrefix(hints[0].(string), "T-2 is still open") {
		t.Errorf("session_close with outcomes: %v", closed)
	}
	ids := []string{}
	for _, w := range written {
		ids = append(ids, w.(map[string]any)["statement_id"].(string))
	}
	read := a.ok("recall_read", map[string]any{"ids": ids})["items"].([]any)
	projects := map[string]bool{}
	for _, it := range read {
		m := it.(map[string]any)
		if m["type"] != "outcome" {
			t.Errorf("outcome statement: %v", m)
		}
		projects[m["project"].(string)] = true
	}
	if !projects["kvartyra-sad"] || !projects["outcomes-lab"] {
		t.Errorf("an outcome of another project's task lands in that project: %v", projects)
	}
	// reads: the task's latest outcome, the topic label, the session, what the project left hanging
	lo, _ := a.ok("task_resume", map[string]any{"project": "outcomes-lab", "num": 1})["latest_outcome"].(map[string]any)
	if lo == nil || lo["state"] != "partial" || lo["next"] != "Pay the invoice" || lo["ref"] != "T-1" || lo["result"] != "Report ordered" {
		t.Errorf("task_resume latest_outcome: %v", lo)
	}
	tro := a.ok("topic_resume", map[string]any{"topic": "valuation"})
	if lt, _ := tro["latest_outcome"].(map[string]any); lt == nil || lt["result"] != "Two quotes compared" {
		t.Errorf("topic_resume latest_outcome: %v", tro["latest_outcome"])
	}
	sro := a.ok("session_resume", map[string]any{"session_id": sO})
	if ns, _ := sro["next_steps"].([]any); len(ns) != 1 || ns[0] != "Call the notary" {
		t.Errorf("session_resume next_steps: %v", sro["next_steps"])
	}
	if so, _ := sro["outcomes"].([]any); len(so) != 5 {
		t.Errorf("session_resume outcomes: %d", len(so))
	}
	po, _ := a.ok("project_resume", map[string]any{"project": "outcomes-lab"})["open_outcomes"].([]any)
	if len(po) != 1 || po[0].(map[string]any)["ref"] != "T-1" {
		t.Errorf("project_resume open_outcomes: %v", po)
	}
	// closing again writes nothing new and says so
	again := a.ok("session_close", closeWith(map[string]any{"ref": "T-4", "result": "Insured", "state": "done"}))
	if again["already_closed"] != true || again["hints"] == nil || again["outcomes"] != nil {
		t.Errorf("repeated close: %v", again)
	}

	// --- entity numbers: issued in order, never reused, in every reply ------------------
	n1 := a.ok("statement_assert", map[string]any{"project": "outcomes-lab", "type": "note", "title": "Numbered one"})
	n2 := a.ok("statement_assert", map[string]any{"project": "outcomes-lab", "type": "note", "title": "Numbered two"})
	num1, _ := n1["num"].(float64)
	if num1 < 1 || n1["ref"] != fmt.Sprintf("R-%d", int(num1)) || n2["num"] != num1+1 {
		t.Errorf("record numbers: %v %v", n1, n2)
	}
	a.ok("statement_retract", map[string]any{"statement_id": n2["statement_id"], "reason": "mistake"})
	if n3 := a.ok("statement_assert", map[string]any{"project": "outcomes-lab", "type": "note", "title": "Numbered three"}); n3["num"] != num1+2 {
		t.Errorf("a retracted record's number is reused: %v", n3)
	}
	if rr := a.ok("recall_read", map[string]any{"ids": []string{n1["statement_id"].(string)}})["items"].([]any); len(rr) != 1 ||
		rr[0].(map[string]any)["ref"] != n1["ref"] {
		t.Errorf("recall_read ref: %v", rr)
	}
	// the retention of one record (0043): set through the API, shown by recall_read
	until := time.Now().AddDate(0, 0, 10).Format(time.DateOnly)
	a.ok("statement_retain", map[string]any{"statement_id": n1["statement_id"], "retain_until": until, "on_expiry": "purge"})
	if rr := a.ok("recall_read", map[string]any{"ids": []string{n1["statement_id"].(string)}})["items"].([]any); len(rr) != 1 ||
		fmt.Sprint(rr[0].(map[string]any)["retention"]) != fmt.Sprintf("map[on_expiry:purge retain_until:%s]", until) {
		t.Errorf("recall_read retention: %v", rr)
	}
	if td := a.ok("task_declare", map[string]any{"project": "outcomes-lab", "title": "Numbered task"}); td["ref"] != fmt.Sprintf("outcomes-lab/T-%v", td["num"]) {
		t.Errorf("task ref: %v", td)
	}
	if tl := a.ok("task_list", map[string]any{"project": "outcomes-lab"})["items"].([]any); len(tl) == 0 ||
		!strings.HasPrefix(tl[0].(map[string]any)["ref"].(string), "outcomes-lab/T-") {
		t.Errorf("task_list ref: %v", tl)
	}
	// --- explicit refs → mentions: checked, linked, replaced on amend; others kept as strings ----
	a.fails("statement_assert", map[string]any{"project": "outcomes-lab", "type": "note", "title": "Broken ref",
		"refs": []string{"R-999999"}}, "ref_not_found")
	a.fails("statement_assert", map[string]any{"project": "outcomes-lab", "type": "note", "title": "Broken ref",
		"refs": []string{"T-77"}}, "ref_not_found")
	a.fails("task_declare", map[string]any{"project": "outcomes-lab", "title": "Broken ref", "refs": []string{"nowhere/T-1"}}, "ref_not_found")
	m1 := a.ok("statement_assert", map[string]any{"project": "outcomes-lab", "type": "discovery", "title": "Invoice follows the report",
		"refs": []string{n1["ref"].(string), "T-1", "jira:ABC-1"}})
	mentionsOf := func(id string) map[string]bool {
		got := map[string]bool{}
		item := a.ok("recall_read", map[string]any{"ids": []string{id}})["items"].([]any)[0].(map[string]any)
		for _, l := range item["links"].([]any) {
			l := l.(map[string]any)
			if l["relation"] == "mentions" {
				to := l["to"].(map[string]any)
				got[to["entity_type"].(string)+":"+to["entity_id"].(string)] = true
				if ref, _ := to["ref"].(string); ref == "" { // the other end is named
					got["no ref on a link"] = true
				}
			}
		}
		if refs, _ := item["refs"].([]any); len(refs) == 0 {
			got["no refs"] = true
		}
		return got
	}
	task1, _ := a.ok("task_resume", map[string]any{"project": "outcomes-lab", "num": 1})["task"].(map[string]any)
	got := mentionsOf(m1["statement_id"].(string))
	if len(got) != 2 || !got["statement:"+n1["statement_id"].(string)] || task1 == nil || !got["task:"+task1["task_id"].(string)] {
		t.Errorf("mentions of refs: %v", got)
	}
	if rel, _ := a.ok("task_resume", map[string]any{"project": "outcomes-lab", "num": 1})["related"].([]any); len(rel) == 0 ||
		rel[0].(map[string]any)["statement_id"] != m1["statement_id"] {
		t.Errorf("task_resume related starts with the record that mentions the task: %v", rel)
	}
	a.ok("statement_amend", map[string]any{"statement_id": m1["statement_id"], "refs": []string{n1["ref"].(string)}})
	if got := mentionsOf(m1["statement_id"].(string)); len(got) != 1 || !got["statement:"+n1["statement_id"].(string)] {
		t.Errorf("amend replaces mentions: %v", got)
	}
	// history shows what each revision's refs were
	hist, _ := a.ok("recall_read", map[string]any{"ids": []string{m1["statement_id"].(string)}, "history": true})["items"].([]any)[0].(map[string]any)["history"].([]any)
	if len(hist) != 2 || len(hist[0].(map[string]any)["refs"].([]any)) != 3 || fmt.Sprint(hist[1].(map[string]any)["refs"]) != fmt.Sprint([]any{n1["ref"]}) {
		t.Errorf("history refs: %v", hist)
	}
	td2 := a.ok("task_declare", map[string]any{"project": "outcomes-lab", "title": "Pay the invoice", "refs": []string{m1["ref"].(string)}})
	mentionedBy := func() bool {
		item := a.ok("recall_read", map[string]any{"ids": []string{m1["statement_id"].(string)}})["items"].([]any)[0].(map[string]any)
		for _, l := range item["links"].([]any) {
			l := l.(map[string]any)
			if l["relation"] == "mentions_by" && l["to"].(map[string]any)["entity_id"] == td2["task_id"] {
				return true
			}
		}
		return false
	}
	if !mentionedBy() {
		t.Error("a task's refs: the record is not mentioned by it")
	}
	a.ok("task_amend", map[string]any{"project": "outcomes-lab", "num": td2["num"], "refs": []string{}})
	if mentionedBy() {
		t.Error("task_amend refs [] keeps the mention")
	}
	// --- a retracted task is still a ref target (a mention speaks of the past), not a subject of work;
	// deletion_impact names who depends on it and changes nothing --------------------------------------------------
	a.ok("task_retract", map[string]any{"project": "outcomes-lab", "num": td2["num"], "reason": "duplicate"})
	m2 := a.ok("statement_assert", map[string]any{"project": "outcomes-lab", "type": "note", "title": "Paid elsewhere",
		"refs": []string{td2["ref"].(string)}})
	imp := a.ok("deletion_impact", map[string]any{"ref": td2["ref"]})
	if tg := imp["target"].(map[string]any); tg["state"] != "retracted" || tg["ref"] != td2["ref"] {
		t.Errorf("deletion_impact target: %v", tg)
	}
	if in := imp["incoming"].([]any); len(in) != 1 || in[0].(map[string]any)["relation"] != "mentions_by" ||
		in[0].(map[string]any)["sample"].([]any)[0].(map[string]any)["entity_id"] != m2["statement_id"] {
		t.Errorf("deletion_impact incoming: %v", in)
	}
	if c := a.ok("deletion_impact", map[string]any{"ref": "outcomes-lab"})["contents"].(map[string]any); c["tasks"] == nil {
		t.Errorf("deletion_impact of a project: %v", c)
	}
	a.fails("deletion_impact", map[string]any{"ref": "R-999999"}, "statement_not_found")
	// --- quarantine by ref; the record answers its number with the quarantine; the person releases it
	q1 := a.ok("journal_quarantine", map[string]any{"ref": m2["ref"], "reason": "secret in the title", "secret": true})
	if a.ok("journal_quarantine", map[string]any{"ref": m2["ref"], "reason": "again"})["already"] != true {
		t.Error("journal_quarantine twice: not already")
	}
	if r := a.ok("recall_read", map[string]any{"ids": []string{m2["statement_id"].(string)}})["items"].([]any)[0].(map[string]any); r["quarantined"] != true || r["title"] != "[quarantined]" || r["tombstone"] == nil {
		t.Errorf("read of a quarantined record: %v (quarantine %v)", r, q1)
	}
	a.fails("journal_quarantine", map[string]any{"ref": "outcomes-lab", "reason": "x"}, "invalid_input") // a project is not quarantined
	a.ok("journal_release", map[string]any{"ref": m2["ref"], "reason": "a false alarm"})
	a.fails("journal_release", map[string]any{"ref": m2["ref"], "reason": "again"}, "not_quarantined")
	// --- a proposal by ref, listed and in project_resume, decided by <project>/D-N
	dp := a.ok("deletion_propose", map[string]any{"ref": m2["ref"], "action": "retract", "reason": "outdated"})
	dref, _ := dp["ref"].(string)
	if !strings.HasPrefix(dref, "outcomes-lab/D-") || dp["impact"].(map[string]any)["target"] == nil {
		t.Errorf("deletion_propose: %v", dp)
	}
	if items := a.ok("deletion_list", map[string]any{"project": "outcomes-lab"})["items"].([]any); len(items) != 1 {
		t.Errorf("deletion_list: %v", items)
	}
	if pd, _ := a.ok("project_resume", map[string]any{"project": "outcomes-lab"})["pending_deletions"].([]any); len(pd) != 1 {
		t.Errorf("project_resume pending_deletions: %v", pd)
	}
	if st := a.ok("deletion_approve", map[string]any{"deletion": dref, "note": "yes"})["state"]; st != "done" {
		t.Errorf("deletion_approve: state %v", st)
	}
	a.fails("deletion_reject", map[string]any{"deletion": dref, "reason": "late"}, "deletion_closed")
	a.fails("deletion_approve", map[string]any{"deletion": "D-1"}, "invalid_input") // no project
	sR := sessionOf(t, a.ok("project_resume", map[string]any{"project": "outcomes-lab"}))
	// a subject that is no longer live does not fail the close (roast r1): the outcome stays as a topic, a
	// warning says so; a ref to nothing (R-999999 below) is still refused
	gc := a.ok("session_close", map[string]any{"session_id": sR, "summary": "retracted task", "outcomes": []any{
		map[string]any{"ref": td2["ref"], "result": "r", "state": "done"}}})
	if w, _ := gc["warnings"].([]any); gc["closed"] != true || len(w) != 1 || !strings.HasPrefix(fmt.Sprint(w[0]), "outcome_subject_gone") {
		t.Errorf("close with a gone subject: %v", gc)
	}
	sN := sessionOf(t, a.ok("project_resume", map[string]any{"project": "outcomes-lab"}))
	a.fails("session_close", map[string]any{"session_id": sN, "summary": "s", "outcomes": []any{
		map[string]any{"ref": "R-999999", "result": "r", "state": "done"}}}, "statement_not_found")
	if c := a.ok("session_close", map[string]any{"session_id": sN, "summary": "numbers", "outcomes": []any{
		map[string]any{"ref": n1["ref"], "result": "Numbered", "state": "done"}}}); c["closed"] != true {
		t.Errorf("outcome by R-N: %v", c)
	}

	// --- HTTP projection and capabilities ---------------------------------------------------------------
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/system/capabilities", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tokChat)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var capOut map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&capOut)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden { // capabilities is not in chat: a refusal, as above
		conn, _ := capOut["connection"].(map[string]any)
		t.Errorf("chat → capabilities: %d %v", resp.StatusCode, conn)
	}
	// --- P6a: the viewer: cookie sign-in, pages under the same RLS, no XSS, export, no sessions -----
	xss := a.ok("statement_assert", map[string]any{"project": "kvartyra-sad", "type": "note", "title": "Markdown note",
		"body": "<script>alert(1)</script>\n\n**bold** and [[R-1]] <img src=x onerror=alert(2)>"})
	a.ok("link_assert", map[string]any{"relation": "related",
		"from": map[string]any{"entity_type": "statement", "entity_id": xss["statement_id"]},
		"to":   map[string]any{"entity_type": "statement", "entity_id": w["statement_id"]}})
	sessionsBefore := len(a.ok("session_list", map[string]any{"project": "kvartyra-sad"})["items"].([]any))
	nr := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string, ck *http.Cookie) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if ck != nil {
			req.AddCookie(ck)
		}
		resp, err := nr.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	if code, _, h := get("/viewer/", nil); code != http.StatusSeeOther || h.Get("Location") != "/viewer/login" {
		t.Errorf("viewer without sign-in: %d %s", code, h.Get("Location"))
	}
	login := func(token string) *http.Response {
		resp, err := nr.PostForm(srv.URL+"/viewer/login", url.Values{"token": {token}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := login("LTP_not-a-token"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("viewer login with a bad token: %d", r.StatusCode)
	}
	r := login(tokA)
	var ck *http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == "litopys_viewer" {
			ck = c
		}
	}
	if r.StatusCode != http.StatusSeeOther || ck == nil || !ck.HttpOnly {
		t.Fatalf("viewer login: %d, cookie %v", r.StatusCode, ck)
	}
	for path, want := range map[string]string{
		"/viewer/":                                "Квартира Сад",
		"/viewer/p/kvartyra-sad":                  "Правила шарів",
		"/viewer/p/kvartyra-sad/all":              "Markdown note",
		"/viewer/search?q=Split":                  "Split the flat",
		"/viewer/p/kvartyra-sad/graph":            `<a href="/viewer/s/` + w["statement_id"].(string),
		"/viewer/login":                           "Увійти",
		"/viewer/s/" + w["statement_id"].(string): "Split the flat 1/2",
	} {
		if code, body, _ := get(path, ck); code != 200 || !strings.Contains(body, want) {
			t.Errorf("viewer %s: %d, %q not found", path, code, want)
		}
	}
	// the graph colours by family: the decision w is drawn in the decision colour
	if _, body, _ := get("/viewer/p/kvartyra-sad/graph", ck); !strings.Contains(body, `fill="#0b5cad"><title>Split the flat`) {
		t.Errorf("viewer graph: the decision is not in the decision colour")
	}
	code, body, hdr := get("/viewer/s/"+xss["statement_id"].(string), ck)
	if code != 200 || strings.Contains(body, "<script>alert(1)") || strings.Contains(body, "onerror") || !strings.Contains(body, "<strong>bold</strong>") ||
		!strings.Contains(body, `href="/viewer/r/1"`) || !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("statement page: markdown, wikilink, no raw HTML, CSP: %d %s", code, body)
	}
	// /viewer/r/{n}: the owner's record by number -> its page; a number the owner has not -> not found
	var wNum int64
	if err := app.QueryRow(sys, `SELECT num FROM projection.statement WHERE statement_id = $1`, w["statement_id"]).Scan(&wNum); err != nil {
		t.Fatal(err)
	}
	if code, _, h := get(fmt.Sprintf("/viewer/r/%d", wNum), ck); code != http.StatusFound || h.Get("Location") != "/viewer/s/"+w["statement_id"].(string) {
		t.Errorf("viewer R-%d: %d %s", wNum, code, h.Get("Location"))
	}
	if code, _, _ := get("/viewer/r/987654", ck); code == http.StatusFound || code == http.StatusOK {
		t.Errorf("viewer: an unknown number answered %d", code)
	}
	if code, body, h := get("/viewer/export/kvartyra-sad.zip", ck); code != 200 || h.Get("Content-Type") != "application/zip" {
		t.Errorf("export: %d %s", code, h.Get("Content-Type"))
	} else if zr, err := zip.NewReader(strings.NewReader(body), int64(len(body))); err != nil || len(zr.File) < 3 || zr.File[0].Name != "kvartyra-sad/index.md" {
		t.Errorf("export zip: %v %d", err, len(zr.File))
	}
	if code, _, _ := get("/viewer/p/no-such-project", ck); code != http.StatusNotFound {
		t.Errorf("missing project: %d", code)
	}
	if after := len(a.ok("session_list", map[string]any{"project": "kvartyra-sad"})["items"].([]any)); after != sessionsBefore {
		t.Errorf("the viewer opened sessions: %d -> %d", sessionsBefore, after)
	}

	whoami := a.ok("system_whoami", nil)
	if whoami["service"] != "Litopys" {
		t.Errorf("whoami: %v", whoami)
	}
}
