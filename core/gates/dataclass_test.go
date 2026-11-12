package gates

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
)

// TestGateDataClass is the acceptance of Д9 part 1 (0023): a project's class hides it
// from a grant with a lower max_data_class, a record raised above the grant's class is hidden inside a visible
// project (search, read, task list, direct SQL), records are raise-only, the owner may lower a project's class,
// and a grant without a class sees everything as before.
func TestGateDataClass(t *testing.T) {
	ctx := context.Background()
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
	owner := verbs.Principal{TenantID: tenant, ActorID: alice, Role: "admin"}
	_, _, err = v.ProjectDeclare(ctx, owner, verbs.ProjectDeclareIn{Name: "bad-class", DataClass: str("secret")})
	if verbs.Code(err) != "invalid_input" {
		t.Fatalf("unknown class accepted: %v", err)
	}
	for name, class := range map[string]*string{"pub": str("public"), "int": nil, "loc": str("local")} {
		_, _, err := v.ProjectDeclare(ctx, owner, verbs.ProjectDeclareIn{Name: name, DataClass: class})
		must("declare "+name, err)
	}
	stmts := map[string]uuid.UUID{}
	for _, name := range []string{"pub", "int", "loc"} {
		st, err := v.StatementAssert(ctx, owner, verbs.StatementAssertIn{Project: name, Type: "note", Title: "marker of " + name, Body: str("marker")})
		must("assert "+name, err)
		stmts[name] = st.StatementID
	}
	raised, err := v.StatementAssert(ctx, owner, verbs.StatementAssertIn{Project: "int", Type: "note", Title: "marker raised",
		Body: str("marker"), DataClass: str("local")})
	must("assert raised", err)
	_, err = v.TaskDeclare(ctx, owner, verbs.TaskDeclareIn{Project: "int", Fields: map[string]any{"title": "plain task"}})
	must("task plain", err)
	rt, err := v.TaskDeclare(ctx, owner, verbs.TaskDeclareIn{Project: "int", Fields: map[string]any{"title": "raised task", "data_class": "local"}})
	must("task raised", err)

	// raise only: a record never goes below its own class, and the class is never cleared
	if _, err := v.StatementAmend(ctx, owner, verbs.StatementAmendIn{StatementID: raised.StatementID, DataClass: str("internal")}); verbs.Code(err) != "data_class_lowered" {
		t.Errorf("statement lowered: %v", err)
	}
	if _, err := v.TaskAmend(ctx, owner, verbs.TaskAmendIn{TaskID: rt.TaskID, Fields: map[string]any{"data_class": "public"}}); verbs.Code(err) != "data_class_lowered" {
		t.Errorf("task lowered: %v", err)
	}
	if _, err := v.TaskAmend(ctx, owner, verbs.TaskAmendIn{TaskID: rt.TaskID, Fields: map[string]any{"data_class": nil}}); verbs.Code(err) != "invalid_input" {
		t.Errorf("task class cleared: %v", err)
	}
	if _, err := v.StatementAmend(ctx, owner, verbs.StatementAmendIn{StatementID: stmts["pub"], DataClass: str("internal")}); err != nil {
		t.Errorf("raise a statement: %v", err)
	}

	as := func(class string) query.Principal {
		return query.Principal{TenantID: tenant, ActorID: alice, Grant: policy.Grant{Profile: policy.ProfileFull, Vendor: "anthropic", MaxDataClass: class}}
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
	found := func(qp query.Principal) []uuid.UUID {
		t.Helper()
		res, err := q.Search(ctx, qp, query.SearchIn{Query: "marker"})
		must("search", err)
		var ids []uuid.UUID
		for _, r := range res.Items {
			ids = append(ids, r.StatementID)
		}
		return ids
	}
	taskTitles := func(qp query.Principal, project string) []string {
		t.Helper()
		out, err := q.TaskList(ctx, qp, query.TaskListIn{Project: project})
		must("task list", err)
		var titles []string
		for _, r := range out.Items {
			titles = append(titles, r.Title)
		}
		slices.Sort(titles)
		return titles
	}

	// --- no class on the grant: everything, as before 0023 ------------------------------------------
	if got := visible(as("")); !slices.Equal(got, []string{"int", "loc", "pub"}) {
		t.Errorf("unclassed grant sees %v", got)
	}
	if got := found(as("")); len(got) != 4 {
		t.Errorf("unclassed grant finds %d statements, want 4", len(got))
	}

	// --- internal: no local project, no raised record inside int -----------------------------------
	qi := as("internal")
	if got := visible(qi); !slices.Equal(got, []string{"int", "pub"}) {
		t.Errorf("internal grant sees %v", got)
	}
	got := found(qi)
	if slices.Contains(got, raised.StatementID) || slices.Contains(got, stmts["loc"]) || len(got) != 2 {
		t.Errorf("internal grant finds %v (raised %s, loc %s)", got, raised.StatementID, stmts["loc"])
	}
	read, err := q.Read(ctx, qi, []uuid.UUID{raised.StatementID, stmts["int"]}, false)
	must("read", err)
	if len(read.Items) != 1 || read.Items[0].StatementID != stmts["int"] {
		t.Errorf("read through the class: %d items", len(read.Items))
	}
	changes, err := q.Changes(ctx, qi, query.ChangesIn{Limit: 500})
	must("changes", err)
	for _, e := range changes.Events {
		if e.EntityID == raised.StatementID || e.EntityID == rt.TaskID || e.EntityID == stmts["loc"] {
			t.Errorf("changefeed of the internal grant carries %s of a local record", e.Type)
		}
	}
	if titles := taskTitles(qi, "int"); !slices.Equal(titles, []string{"plain task"}) {
		t.Errorf("internal grant lists tasks %v", titles)
	}
	d, err := policy.Decide(pg.WithOwner(ctx, alice), app, tenant, alice, qi.Grant)
	must("decide", err)
	scoped := policy.Scoped(pg.WithOwner(ctx, alice), d)
	for _, tbl := range []string{"statement", "task"} {
		var n int
		must(tbl, app.QueryRow(scoped, `SELECT count(*) FROM projection.`+tbl+` x
		        JOIN projection.project p ON p.project_id = x.project_id
		       WHERE p.data_class = 'local' OR x.data_class = 'local'`).Scan(&n))
		if n != 0 {
			t.Errorf("%d local rows of projection.%s reach an internal grant", n, tbl)
		}
	}
	hidden := false
	for _, h := range d.Hidden {
		hidden = hidden || (h.HiddenBy == "grant.max_data_class" && h.Count == 1)
	}
	if !hidden {
		t.Errorf("hidden[] does not explain the class: %+v", d.Hidden)
	}

	// --- public: only pub, and its statement was raised to internal above ---------------------------
	qp := as("public")
	if got := visible(qp); !slices.Equal(got, []string{"pub"}) {
		t.Errorf("public grant sees %v", got)
	}
	if got := found(qp); len(got) != 0 {
		t.Errorf("public grant finds %v (the pub marker is raised to internal)", got)
	}

	// --- the owner lowers a project: it becomes visible ----------------------------------------------
	_, err = v.ProjectAmend(ctx, owner, verbs.ProjectAmendIn{Project: "loc", DataClass: str("internal")})
	must("lower loc", err)
	if got := visible(qi); !slices.Equal(got, []string{"int", "loc", "pub"}) {
		t.Errorf("after lowering loc the internal grant sees %v", got)
	}
}
