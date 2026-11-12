package gates

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
)

// TestGatePolicy is the acceptance of the policy layer: a grant's allowlist, a tenant rule by label
// and vendor, value redaction, the readonly profile; hidden[] without names; nothing hidden reaches
// any answer or direct SQL (E15 contamination = 0); policy.explain agrees with the filtering (E9).
func TestGatePolicy(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db, tenant := stand(t)
	app := db.RolePool(t, "litopys_app", 4)
	v := &verbs.Service{DB: app}
	q := &query.Service{DB: app}
	str := func(s string) *string { return &s }
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	alice, _, err := v.RegisterHuman(ctx, tenant, "Alice", "alice@example")
	must("register", err)
	bob, _, err := v.RegisterHuman(ctx, tenant, "Bob", "bob@example")
	must("register bob", err)
	full := verbs.Principal{TenantID: tenant, ActorID: alice, Role: "admin"}
	stmts := map[string]uuid.UUID{}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		open, err := v.ContextOpen(ctx, full, verbs.ContextOpenIn{Project: name, Attach: "none", AutoCreate: true})
		must("open "+name, err)
		p := full
		p.RunID = open.RunID
		st, err := v.StatementAssert(ctx, p, verbs.StatementAssertIn{Project: name, Type: "note",
			Title: "marker of " + name, Body: str("marker body " + name), ContextID: &open.ContextID})
		must("assert "+name, err)
		stmts[name] = st.StatementID
		_, err = v.FactRecord(ctx, p, verbs.FactRecordIn{Project: name, Kind: "note", Key: "endpoint", Value: str("value-of-" + name)})
		must("fact "+name, err)
	}
	_, err = v.ProjectAmend(ctx, full, verbs.ProjectAmendIn{Project: "beta", Labels: &[]string{"corp-only"}})
	must("label beta", err)

	// tenant rule: no openai client sees corp-only projects; only an admin may set it
	rule := policy.Rule{Type: policy.ProjectLabelDeny, Labels: []string{"corp-only"}, Vendors: []string{"openai"}}
	_, _, err = v.PolicySet(ctx, verbs.Principal{TenantID: tenant, ActorID: bob}, verbs.PolicySetIn{Name: "no-openai-corp", Scope: "tenant", Rule: rule})
	if verbs.Code(err) != "forbidden" {
		t.Fatalf("a non-admin set a tenant rule: %v", err)
	}
	_, a, err := v.PolicySet(ctx, full, verbs.PolicySetIn{Name: "no-openai-corp", Scope: "tenant", Rule: rule, Reason: str("corp data")})
	must("tenant rule", err)
	_, again, err := v.PolicySet(ctx, full, verbs.PolicySetIn{Name: "no-openai-corp", Scope: "tenant", Rule: rule, Reason: str("corp data")})
	must("tenant rule again", err)
	if a.Seq == 0 || !again.Unchanged {
		t.Fatalf("policy.set idempotency: %+v %+v", a, again)
	}
	_, _, err = v.PolicySet(ctx, full, verbs.PolicySetIn{Name: "redact-openai", Scope: "owner",
		Rule: policy.Rule{Type: policy.FieldRedaction, Fields: []string{"fact.value"}, Vendors: []string{"openai"}}})
	must("owner redaction rule", err)

	grant := func(g policy.Grant) (verbs.Principal, query.Principal) {
		_, id, err := auth.IssueGrant(ctx, app, tenant, alice, "owner", "policy-test", nil, g, 0)
		must("issue grant", err)
		g.TokenID = id
		if g.Profile == "" {
			g.Profile = policy.ProfileFull
		}
		return verbs.Principal{TenantID: tenant, ActorID: alice, Grant: g}, query.Principal{TenantID: tenant, ActorID: alice, Grant: g}
	}
	visible := func(qp query.Principal) []string {
		t.Helper()
		list, err := q.ProjectList(ctx, qp, "")
		must("project list", err)
		var names []string
		for _, p := range list {
			names = append(names, p.Name)
		}
		slices.Sort(names)
		return names
	}
	searchProjects := func(qp query.Principal) ([]string, []query.Hidden) {
		t.Helper()
		res, err := q.Search(ctx, qp, query.SearchIn{Query: "marker"})
		must("search", err)
		var names []string
		for _, r := range res.Items {
			if !slices.Contains(names, r.Project) {
				names = append(names, r.Project)
			}
		}
		slices.Sort(names)
		return names, res.Hidden
	}

	// --- full grant of the owner: nothing hidden ------------------------------------------------
	_, qFull := grant(policy.Grant{Vendor: "anthropic"})
	if got := visible(qFull); !slices.Equal(got, []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("full grant sees %v", got)
	}

	// --- grant allowlist [alpha] ------------------------------------------------------------------
	vAlpha, qAlpha := grant(policy.Grant{ProjectAllowlist: []string{"alpha"}, Vendor: "anthropic"})
	if got := visible(qAlpha); !slices.Equal(got, []string{"alpha"}) {
		t.Errorf("allowlist grant sees projects %v", got)
	}
	names, hidden := searchProjects(qAlpha)
	if !slices.Equal(names, []string{"alpha"}) || len(hidden) != 1 || hidden[0].HiddenBy != "grant.project_allowlist" || hidden[0].Count != 2 {
		t.Errorf("allowlist search: projects %v, hidden %+v", names, hidden)
	}
	if b, _ := json.Marshal(hidden); strings.Contains(string(b), "beta") || strings.Contains(string(b), "gamma") {
		t.Errorf("hidden[] names hidden projects: %s", b)
	}
	read, err := q.Read(ctx, qAlpha, []uuid.UUID{stmts["beta"], stmts["alpha"]}, false)
	must("read", err)
	if len(read.Items) != 1 || read.Items[0].StatementID != stmts["alpha"] {
		t.Errorf("read through the allowlist: %d items", len(read.Items))
	}
	changes, err := q.Changes(ctx, qAlpha, query.ChangesIn{Limit: 500})
	must("changes", err)
	for _, e := range changes.Events {
		if e.EntityID == stmts["beta"] || e.EntityID == stmts["gamma"] {
			t.Errorf("changefeed of the allowlist grant carries %s of a hidden project", e.Type)
		}
	}
	// E15 on direct SQL: the decision travels to the database (litopys.projects, RLS of 0010)
	d, err := policy.Decide(pg.WithOwner(ctx, alice), app, tenant, alice, qAlpha.Grant)
	must("decide", err)
	scoped := policy.Scoped(pg.WithOwner(ctx, alice), d)
	for _, tbl := range []string{"project", "statement", "fact", "context"} {
		var n int
		must(tbl, app.QueryRow(scoped, `SELECT count(*) FROM projection.`+tbl+` x
		        JOIN projection.project p ON p.project_id = x.project_id
		       WHERE p.name <> 'alpha'`).Scan(&n))
		if n != 0 {
			t.Errorf("E15: %d rows of hidden projects in projection.%s", n, tbl)
		}
	}
	_, err = v.StatementAssert(ctx, vAlpha, verbs.StatementAssertIn{Project: "beta", Type: "note", Title: "into a hidden project"})
	if verbs.Code(err) != "project_not_found" {
		t.Errorf("write into a hidden project: %v", err)
	}
	_, err = v.ContextOpen(ctx, vAlpha, verbs.ContextOpenIn{Project: "delta", Attach: "none", AutoCreate: true})
	if verbs.Code(err) != "policy_denied" {
		t.Errorf("create a project outside the allowlist: %v", err)
	}
	if _, err := v.ContextOpen(ctx, vAlpha, verbs.ContextOpenIn{Project: "alpha", Attach: "none"}); err != nil {
		t.Errorf("open an allowed project: %v", err)
	}
	if _, err := q.PolicyExplain(ctx, qAlpha, "", nil); query.Code(err) != "forbidden" {
		t.Errorf("a restricted grant asked policy.explain: %v", err)
	}

	// --- openai grant: the tenant label rule and the owner's redaction ------------------------------
	_, qOpenAI := grant(policy.Grant{Vendor: "openai"})
	if got := visible(qOpenAI); !slices.Equal(got, []string{"alpha", "gamma"}) {
		t.Errorf("openai grant sees %v (corp-only beta must be hidden)", got)
	}
	_, hidden = searchProjects(qOpenAI)
	if len(hidden) != 1 || hidden[0].HiddenBy != "policy.tenant.no-openai-corp" || hidden[0].Count != 1 {
		t.Errorf("openai hidden: %+v", hidden)
	}
	facts, err := q.FactList(ctx, qOpenAI, "", "", "")
	must("facts", err)
	for _, f := range facts {
		if f.Value != nil {
			t.Errorf("openai grant reads fact value of %s", f.Project)
		}
	}
	if found, _ := q.FactList(ctx, qOpenAI, "", "", "value-of"); len(found) != 0 {
		t.Errorf("a redacted value is guessable by matching: %d facts", len(found))
	}
	if got := visible(qFull); !slices.Equal(got, []string{"alpha", "beta", "gamma"}) {
		t.Errorf("anthropic grant after the openai rule sees %v", got)
	}

	// --- E9: explain agrees with the filtering --------------------------------------------------
	ex, err := q.PolicyExplain(ctx, qFull, "", &qOpenAI.Grant.TokenID)
	must("explain", err)
	shown := visible(qOpenAI)
	for _, p := range ex.Projects {
		if p.Visible != slices.Contains(shown, p.Project) {
			t.Errorf("E9: explain says %s visible=%v, the grant sees %v", p.Project, p.Visible, shown)
		}
		if p.Project == "beta" && (p.HiddenBy != "policy.tenant.no-openai-corp" || len(p.Rules) == 0 || p.Rules[0].SetSeq == 0) {
			t.Errorf("explain beta: %+v", p)
		}
	}

	// --- readonly profile: queries yes, commands no ------------------------------------------------
	vRO, qRO := grant(policy.Grant{Profile: policy.ProfileReadonly, Vendor: "anthropic"})
	if got := visible(qRO); len(got) != 3 {
		t.Errorf("readonly grant sees %v", got)
	}
	_, err = v.StatementAssert(ctx, vRO, verbs.StatementAssertIn{Project: "alpha", Type: "note", Title: "readonly write"})
	if verbs.Code(err) != "policy_denied" {
		t.Errorf("readonly write: %v", err)
	}

	// rules replay: the projection is rebuilt from policy.set events like everything else
	var rules int
	must("rules", app.QueryRow(sys, `SELECT count(*) FROM projection.policy WHERE active`).Scan(&rules))
	if rules != 2 {
		t.Errorf("active rules: %d, want 2", rules)
	}
}
