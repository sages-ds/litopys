package verbs_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/projector"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// harvest is the shape of domain/examples/workflow-vault-harvest.yaml, shortened.
const harvest = `{
  "params": {"type": "object", "properties": {"limit": {"type": "integer"}}},
  "triggers": [{"kind": "manual"}, {"kind": "journal", "spec": "context.closed"}],
  "touches": ["vault", "git"],
  "steps": [
    {"key": "collect", "title": "Candidates", "kind": "model", "executor_class": "cheap", "write_mode": "candidate",
     "max_data_class": "internal", "verbs": ["recall.search"], "context_pack": {"budget_tokens": 2000}},
    {"key": "draft", "title": "Drafts", "kind": "model", "executor_class": "main", "delegable": false},
    {"key": "go", "title": "GO on the batch", "kind": "human", "who_may_run": "human_only"},
    {"key": "write", "title": "Write approved notes", "kind": "model", "requires_approval": true,
     "condition": "size(steps) > 0 && params.limit > 0"},
    {"key": "commit", "title": "Commit", "kind": "script", "timeout_s": 600, "heartbeat_s": 60}
  ]}`

// TestWorkflows: definitions of processes — validation of a definition, immutable versions,
// metadata, retirement, reads with the SKILL.md export, and a rebuild that reproduces the projections.
func TestWorkflows(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	s := &verbs.Service{DB: app, Admin: db.RolePool(t, "litopys_admin", 1)}
	q := &query.Service{DB: app}
	var tenant uuid.UUID
	must(t, "tenant", app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant))
	ada, _, err := s.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must(t, "human", err)
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	_, _, err = s.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "litopys-test"})
	must(t, "project", err)

	// invalid definitions: each refused with invalid_definition and a reason
	bad := map[string]string{
		"unknown field":      `{"steps": [{"key": "a", "title": "A", "kind": "script", "colour": "red"}]}`,
		"no steps":           `{"steps": []}`,
		"duplicate key":      `{"steps": [{"key": "a", "title": "A", "kind": "script"}, {"key": "a", "title": "B", "kind": "script"}]}`,
		"unknown kind":       `{"steps": [{"key": "a", "title": "A", "kind": "robot"}]}`,
		"next to nowhere":    `{"steps": [{"key": "a", "title": "A", "kind": "script", "next": ["z"]}]}`,
		"cycle":              `{"steps": [{"key": "a", "title": "A", "kind": "script"}, {"key": "b", "title": "B", "kind": "script", "next": ["a"]}]}`,
		"bad CEL":            `{"steps": [{"key": "a", "title": "A", "kind": "script", "condition": "params.x >"}]}`,
		"non-bool CEL":       `{"steps": [{"key": "a", "title": "A", "kind": "script", "condition": "1 + 2"}]}`,
		"unknown CEL var":    `{"steps": [{"key": "a", "title": "A", "kind": "script", "condition": "secrets.token == 1"}]}`,
		"class on script":    `{"steps": [{"key": "a", "title": "A", "kind": "script", "executor_class": "main"}]}`,
		"external no system": `{"steps": [{"key": "a", "title": "A", "kind": "external"}]}`,
		"human by assistant": `{"steps": [{"key": "a", "title": "A", "kind": "human", "who_may_run": "assistant"}]}`,
		"heartbeat >= lease": `{"steps": [{"key": "a", "title": "A", "kind": "script", "timeout_s": 60, "heartbeat_s": 60}]}`,
		"schedule no spec":   `{"triggers": [{"kind": "schedule"}], "steps": [{"key": "a", "title": "A", "kind": "script"}]}`,
	}
	for name, def := range bad {
		_, err := s.WorkflowDeclare(ctx, me, verbs.WorkflowDeclareIn{Project: "litopys-test", Key: "bad", Title: "x", Definition: json.RawMessage(def)})
		if verbs.Code(err) != "invalid_definition" {
			t.Errorf("%s: code %q (%v), want invalid_definition", name, verbs.Code(err), err)
		}
	}

	d1, err := s.WorkflowDeclare(ctx, me, verbs.WorkflowDeclareIn{Project: "litopys-test", Key: "vault-harvest", Title: "Vault harvest",
		Purpose: ptr("Memory records into the curated vault"), Definition: json.RawMessage(harvest)})
	must(t, "declare", err)
	if d1.Version != 1 || d1.Seq == 0 {
		t.Fatalf("declare: %+v", d1)
	}
	_, err = s.WorkflowDeclare(ctx, me, verbs.WorkflowDeclareIn{Project: "litopys-test", Key: "vault-harvest", Title: "again", Definition: json.RawMessage(harvest)})
	wantCode(t, "same key twice", err, "workflow_exists")

	// the same definition again is no version; a changed one is version 2; a stale if_version is refused
	same, err := s.WorkflowSupersede(ctx, me, verbs.WorkflowSupersedeIn{Project: "litopys-test", Key: "vault-harvest", Definition: json.RawMessage(harvest)})
	must(t, "supersede same", err)
	if !same.Unchanged || same.Version != 1 {
		t.Fatalf("supersede same: %+v", same)
	}
	v2def := strings.Replace(harvest, `"title": "Commit"`, `"title": "Commit what was written"`, 1)
	v2, err := s.WorkflowSupersede(ctx, me, verbs.WorkflowSupersedeIn{Project: "litopys-test", Key: "vault-harvest", IfVersion: ptr(1),
		Definition: json.RawMessage(v2def), Reason: ptr("05.10 lesson")})
	must(t, "supersede", err)
	if v2.Version != 2 || v2.Unchanged {
		t.Fatalf("supersede: %+v", v2)
	}
	_, err = s.WorkflowSupersede(ctx, me, verbs.WorkflowSupersedeIn{Project: "litopys-test", Key: "vault-harvest", IfVersion: ptr(1),
		Definition: json.RawMessage(harvest)})
	wantCode(t, "stale if_version", err, "version_conflict")
	_, err = s.WorkflowSupersede(ctx, me, verbs.WorkflowSupersedeIn{Project: "litopys-test", Key: "nope", Definition: json.RawMessage(harvest)})
	wantCode(t, "supersede unknown", err, "workflow_not_found")

	am, err := s.WorkflowAmend(ctx, me, verbs.WorkflowAmendIn{Project: "litopys-test", Key: "vault-harvest", Fields: map[string]any{"title": "Vault harvest"}})
	must(t, "amend same title", err)
	if !am.Unchanged {
		t.Fatal("amend with the same title must be unchanged")
	}
	_, err = s.WorkflowAmend(ctx, me, verbs.WorkflowAmendIn{Project: "litopys-test", Key: "vault-harvest", Fields: map[string]any{"title": "Harvest", "purpose": nil}})
	must(t, "amend", err)
	_, err = s.WorkflowAmend(ctx, me, verbs.WorkflowAmendIn{Project: "litopys-test", Key: "vault-harvest", Fields: map[string]any{"steps": 1}})
	wantCode(t, "amend steps", err, "invalid_input")

	// reads: the current version by default, a past one on request, SKILL.md on request
	list, err := q.WorkflowList(ctx, query.Principal{TenantID: tenant, ActorID: ada}, query.WorkflowListIn{Project: "litopys-test"})
	must(t, "list", err)
	if len(list.Items) != 1 || list.Items[0].Version != 2 || list.Items[0].Steps != 5 || list.Items[0].Title != "Harvest" || list.Items[0].Purpose != nil {
		t.Fatalf("list: %+v", list.Items)
	}
	qp := query.Principal{TenantID: tenant, ActorID: ada}
	r1, err := q.WorkflowRead(ctx, qp, query.WorkflowReadIn{Project: "litopys-test", Key: "vault-harvest", Version: ptr(1), Format: "skill"})
	must(t, "read v1", err)
	if r1.Version != 1 || !strings.Contains(string(r1.Definition), `"Commit"`) || strings.Contains(string(r1.Definition), "Commit what") || r1.SkillMD == nil {
		t.Fatalf("read v1: %d %s", r1.Version, r1.Definition)
	}
	for _, want := range []string{"name: vault-harvest", "wait for the user's explicit GO", "do not delegate", "results are candidates", "the user does this step"} {
		if !strings.Contains(*r1.SkillMD, want) {
			t.Errorf("SKILL.md lacks %q:\n%s", want, *r1.SkillMD)
		}
	}
	r2, err := q.WorkflowRead(ctx, qp, query.WorkflowReadIn{Project: "litopys-test", Key: "vault-harvest"})
	must(t, "read current", err)
	if r2.Version != 2 || r2.Reason == nil || *r2.Reason != "05.10 lesson" || r2.SkillMD != nil {
		t.Fatalf("read current: %+v", r2)
	}
	_, err = q.WorkflowRead(ctx, qp, query.WorkflowReadIn{Project: "litopys-test", Key: "vault-harvest", Version: ptr(9)})
	if query.Code(err) != "version_not_found" {
		t.Fatalf("read v9: %v", err)
	}

	// retire: idempotent, no new versions, hidden from the list unless asked
	r, err := s.WorkflowRetire(ctx, me, "litopys-test", "vault-harvest", ptr("replaced"))
	must(t, "retire", err)
	again, err := s.WorkflowRetire(ctx, me, "litopys-test", "vault-harvest", nil)
	must(t, "retire again", err)
	if r.Already || !again.Already {
		t.Fatal("retire idempotency")
	}
	_, err = s.WorkflowSupersede(ctx, me, verbs.WorkflowSupersedeIn{Project: "litopys-test", Key: "vault-harvest", Definition: json.RawMessage(v2def + " ")})
	wantCode(t, "supersede retired", err, "workflow_retired")
	hidden, err := q.WorkflowList(ctx, qp, query.WorkflowListIn{Project: "litopys-test"})
	must(t, "list without retired", err)
	shown, err := q.WorkflowList(ctx, qp, query.WorkflowListIn{Project: "litopys-test", IncludeRetired: true})
	must(t, "list with retired", err)
	if len(hidden.Items) != 0 || len(shown.Items) != 1 || !shown.Items[0].Retired {
		t.Fatalf("retired in lists: %d %d", len(hidden.Items), len(shown.Items))
	}

	// the projections survive a rebuild unchanged (E7)
	before := dumpTables(t, app, "workflow", "workflow_version")
	if _, err := projector.Rebuild(ctx, app, tenant); err != nil {
		t.Fatal(err)
	}
	if after := dumpTables(t, app, "workflow", "workflow_version"); !reflect.DeepEqual(before, after) {
		t.Errorf("rebuild changed the workflow projections:\n%v\n%v", before, after)
	}
}
