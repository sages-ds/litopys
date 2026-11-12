package verbs_test

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/projector"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

const runDef = `{"steps": [
  {"key": "collect", "title": "Candidates", "kind": "model", "executor_class": "cheap"},
  {"key": "go", "title": "GO on the batch", "kind": "human"},
  {"key": "write", "title": "Write", "kind": "model", "requires_approval": true,
   "context_pack": {"selectors": ["steps.collect.output", "task:litopys-test#1", "recall:harvest"], "budget_tokens": 1500}},
  {"key": "restore", "title": "Verify restore", "kind": "external", "external": {"system": "ci", "ref": "job:verify-restore"},
   "condition": "params.migration == true"},
  {"key": "commit", "title": "Commit", "kind": "script"}]}`

// TestExecutions walks a run through every rule of executions: ready steps, leases, a human step and an approval
// point that only a person passes, a skipped conditional step, a failed attempt and its retry, the automatic close
// on the last step, reads (run, queue, step package) and a rebuild that reproduces the projections.
func TestExecutions(t *testing.T) {
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
	qp := query.Principal{TenantID: tenant, ActorID: ada}
	agent, _, err := s.ActorRegister(ctx, me, verbs.ActorRegisterIn{Kind: "agent", DisplayName: "runner", ExternalIdentity: ptr("agent:runner")})
	must(t, "agent", err)
	bot := verbs.Principal{TenantID: tenant, ActorID: agent}
	_, _, err = s.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "litopys-test"})
	must(t, "project", err)
	_, err = s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-test", Fields: map[string]any{"title": "harvest the vault"}})
	must(t, "task", err)
	_, err = s.WorkflowDeclare(ctx, me, verbs.WorkflowDeclareIn{Project: "litopys-test", Key: "harvest", Title: "Harvest", Definition: json.RawMessage(runDef)})
	must(t, "workflow", err)

	run, err := s.ExecutionStart(ctx, me, verbs.ExecutionStartIn{Project: "litopys-test", Workflow: "harvest",
		Params: json.RawMessage(`{"migration": false}`), TaskNum: ptr(1)})
	must(t, "start", err)
	if run.Version != 1 || !slices.Equal(run.Ready, []string{"collect"}) {
		t.Fatalf("start: %+v", run)
	}
	x := run.ExecutionID
	_, err = s.StepStart(ctx, bot, verbs.StepStartIn{ExecutionID: x, StepKey: "go"})
	wantCode(t, "a pending step", err, "step_not_ready")

	// the runner takes collect; a second taker is refused while the lease holds
	a1, err := s.StepStart(ctx, bot, verbs.StepStartIn{ExecutionID: x, StepKey: "collect"})
	must(t, "take collect", err)
	_, err = s.StepStart(ctx, me, verbs.StepStartIn{ExecutionID: x, StepKey: "collect"})
	wantCode(t, "busy", err, "step_busy")
	open, err := q.StepOpenItems(ctx, qp, query.StepOpenItemsIn{})
	must(t, "queue while working", err)
	if len(open.Items) != 0 {
		t.Fatalf("queue while collect is held: %+v", open.Items)
	}
	_, err = s.StepCheckpoint(ctx, bot, verbs.StepCheckpointIn{AttemptID: a1.AttemptID, Output: json.RawMessage(`{"partial": 1}`)})
	must(t, "heartbeat", err)
	f1, err := s.StepFinish(ctx, bot, verbs.StepFinishIn{AttemptID: a1.AttemptID, State: "completed", Output: json.RawMessage(`{"candidates": [3, 5]}`)})
	must(t, "finish collect", err)
	if len(f1.Ready) != 0 || f1.ExecutionClosed {
		t.Fatalf("after collect the run waits for a person: %+v", f1)
	}
	_, err = s.StepFinish(ctx, bot, verbs.StepFinishIn{AttemptID: a1.AttemptID, State: "completed"})
	wantCode(t, "finish twice", err, "attempt_finished")

	// the human step: nobody takes it; only a person's GO passes it
	_, err = s.StepStart(ctx, bot, verbs.StepStartIn{ExecutionID: x, StepKey: "go"})
	wantCode(t, "take a human step", err, "person_required")
	_, err = s.ApprovalGrant(ctx, bot, verbs.ApprovalGrantIn{ExecutionID: x, StepKey: "go"})
	wantCode(t, "an agent gives a GO", err, "approval_requires_human")
	_, err = s.ApprovalGrant(ctx, me, verbs.ApprovalGrantIn{ExecutionID: x, StepKey: "collect"})
	wantCode(t, "GO where none is needed", err, "approval_not_needed")
	g1, err := s.ApprovalGrant(ctx, me, verbs.ApprovalGrantIn{ExecutionID: x, StepKey: "go", Items: []string{"3"}, Note: ptr("only note 3")})
	must(t, "GO on the human step", err)
	if len(g1.Ready) != 0 {
		t.Fatalf("write still waits for its own GO: %+v", g1.Ready)
	}
	_, err = s.StepStart(ctx, bot, verbs.StepStartIn{ExecutionID: x, StepKey: "write"})
	wantCode(t, "write without GO", err, "approval_required")
	g2, err := s.ApprovalGrant(ctx, me, verbs.ApprovalGrantIn{ExecutionID: x, StepKey: "write"})
	must(t, "GO on write", err)
	g2again, err := s.ApprovalGrant(ctx, me, verbs.ApprovalGrantIn{ExecutionID: x, StepKey: "write"})
	must(t, "GO on write again", err)
	if !slices.Equal(g2.Ready, []string{"write"}) || !g2again.Already || g2again.ApprovalID != g2.ApprovalID {
		t.Fatalf("GO on write: %+v / %+v", g2, g2again)
	}

	// the step package of write: its definition, inputs, resolved and unresolved selectors
	pack, err := q.StepPack(ctx, qp, query.StepPackIn{ExecutionID: x, StepKey: "write"})
	must(t, "pack", err)
	if pack.State != "submitted" || pack.BudgetTokens == nil || *pack.BudgetTokens != 1500 || pack.Inputs["collect"] == nil || len(pack.Context) != 3 ||
		pack.Context[0].Value == nil || pack.Context[1].Value == nil || pack.Context[2].Unresolved != "no statements match" {
		t.Fatalf("pack: %+v", pack)
	}
	// recall: finds a statement of the run's project by its query
	_, err = s.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "litopys-test", Type: "decision", Title: "harvest notes into the vault weekly"})
	must(t, "statement", err)
	pack, err = q.StepPack(ctx, qp, query.StepPackIn{ExecutionID: x, StepKey: "write"})
	must(t, "pack with recall", err)
	if got, _ := pack.Context[2].Value.([]query.PackStatement); len(got) != 1 || got[0].Title != "harvest notes into the vault weekly" {
		t.Fatalf("recall selector: %+v", pack.Context[2])
	}

	a2, err := s.StepStart(ctx, bot, verbs.StepStartIn{ExecutionID: x, StepKey: "write"})
	must(t, "take write", err)
	_, err = s.ApprovalRevoke(ctx, me, g2.ApprovalID, nil)
	wantCode(t, "revoke a used GO", err, "approval_used")
	f2, err := s.StepFinish(ctx, bot, verbs.StepFinishIn{AttemptID: a2.AttemptID, State: "completed"})
	must(t, "finish write", err)
	if !slices.Equal(f2.Ready, []string{"commit"}) {
		t.Fatalf("restore is skipped by params.migration = false, commit is next: %+v", f2.Ready)
	}

	// a failed attempt, its retry, and the run closes itself on the last step
	a3, err := s.StepStart(ctx, bot, verbs.StepStartIn{ExecutionID: x, StepKey: "commit"})
	must(t, "take commit", err)
	_, err = s.StepFinish(ctx, bot, verbs.StepFinishIn{AttemptID: a3.AttemptID, State: "failed", Error: ptr("push rejected")})
	must(t, "commit failed", err)
	a4, err := s.StepStart(ctx, bot, verbs.StepStartIn{ExecutionID: x, StepKey: "commit"})
	must(t, "retry commit", err)
	if a4.Attempt != 2 {
		t.Fatalf("retry attempt number: %d", a4.Attempt)
	}
	f4, err := s.StepFinish(ctx, bot, verbs.StepFinishIn{AttemptID: a4.AttemptID, State: "completed"})
	must(t, "commit done", err)
	if !f4.ExecutionClosed {
		t.Fatal("the last step must close the run")
	}
	cl, err := s.ExecutionClose(ctx, me, verbs.ExecutionCloseIn{ExecutionID: x, Outcome: "canceled"})
	must(t, "close a closed run", err)
	if !cl.Already {
		t.Fatal("close idempotency")
	}
	_, err = s.StepStart(ctx, bot, verbs.StepStartIn{ExecutionID: x, StepKey: "commit"})
	wantCode(t, "a step of a closed run", err, "execution_closed")

	read, err := q.ExecutionRead(ctx, qp, x)
	must(t, "read", err)
	states := map[string]string{}
	for _, st := range read.Steps {
		states[st.Key] = st.State
	}
	want := map[string]string{"collect": "completed", "go": "completed", "write": "completed", "restore": "skipped", "commit": "completed"}
	if !reflect.DeepEqual(states, want) || read.Execution.State != "completed" || read.Execution.StepsDone != 5 || read.Execution.TaskNum == nil {
		t.Fatalf("read: %v %+v", states, read.Execution)
	}
	all, err := q.ExecutionList(ctx, qp, query.ExecutionListIn{State: "all"})
	must(t, "list all", err)
	openRuns, err := q.ExecutionList(ctx, qp, query.ExecutionListIn{})
	must(t, "list open", err)
	if len(all.Items) != 1 || len(openRuns.Items) != 0 {
		t.Fatalf("lists: all %d open %d", len(all.Items), len(openRuns.Items))
	}

	// a second run waits for the person at go: the list shows input_required
	r2, err := s.ExecutionStart(ctx, me, verbs.ExecutionStartIn{Project: "litopys-test", Workflow: "harvest"})
	must(t, "second run", err)
	b1, err := s.StepStart(ctx, bot, verbs.StepStartIn{ExecutionID: r2.ExecutionID, StepKey: "collect"})
	must(t, "second collect", err)
	_, err = s.StepFinish(ctx, bot, verbs.StepFinishIn{AttemptID: b1.AttemptID, State: "completed"})
	must(t, "second collect done", err)
	openRuns, err = q.ExecutionList(ctx, qp, query.ExecutionListIn{})
	must(t, "list open 2", err)
	if len(openRuns.Items) != 1 || openRuns.Items[0].State != "input_required" || openRuns.Items[0].WaitingPerson != 1 {
		t.Fatalf("waiting run: %+v", openRuns.Items)
	}

	_, err = s.WorkflowRetire(ctx, me, "litopys-test", "harvest", nil)
	must(t, "retire", err)
	_, err = s.ExecutionStart(ctx, me, verbs.ExecutionStartIn{Project: "litopys-test", Workflow: "harvest"})
	wantCode(t, "start a retired process", err, "workflow_retired")

	tables := []string{"execution", "step_attempt", "approval", "workflow", "workflow_version"}
	before := dumpTables(t, app, tables...)
	if _, err := projector.Rebuild(ctx, app, tenant); err != nil {
		t.Fatal(err)
	}
	if after := dumpTables(t, app, tables...); !reflect.DeepEqual(before, after) {
		for _, tbl := range tables {
			if !reflect.DeepEqual(before[tbl], after[tbl]) {
				t.Errorf("rebuild changed %s:\n%v\n%v", tbl, before[tbl], after[tbl])
			}
		}
	}
}
