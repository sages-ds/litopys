package verbs_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/projector"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

func ptr[T any](v T) *T { return &v }

func wantCode(t *testing.T, what string, err error, code string) {
	t.Helper()
	if got := verbs.Code(err); got != code {
		t.Fatalf("%s: code %q (%v), want %q", what, got, err, code)
	}
}

func must(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// TestCommands walks every slice-1 command through its happy path, idempotency and error codes
// on a throw-away database, then checks that a rebuild reproduces what the commands projected.
func TestCommands(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem) // direct checks read every owner (RLS of 0008)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	s := &verbs.Service{DB: app, Admin: db.RolePool(t, "litopys_admin", 1)}
	var tenant uuid.UUID
	must(t, "tenant", app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant))

	// --- identity -------------------------------------------------------------------------
	ada, created, err := s.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must(t, "register human", err)
	again, created2, err := s.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must(t, "register human again", err)
	if !created || created2 || again != ada {
		t.Fatalf("RegisterHuman idempotency: %v %v %v", created, created2, again == ada)
	}
	other, _, err := s.RegisterHuman(ctx, tenant, "Other", "other@example")
	must(t, "register other", err)
	me := verbs.Principal{TenantID: tenant, ActorID: ada}

	agent, a, err := s.ActorRegister(ctx, me, verbs.ActorRegisterIn{Kind: "agent", DisplayName: "sub", ExternalIdentity: ptr("agent:sub")})
	must(t, "register agent", err)
	agent2, a2, err := s.ActorRegister(ctx, me, verbs.ActorRegisterIn{Kind: "agent", DisplayName: "sub", ExternalIdentity: ptr("agent:sub")})
	must(t, "register agent again", err)
	if a.Seq == 0 || !a2.Already || agent2 != agent {
		t.Fatalf("ActorRegister idempotency: %+v %+v", a, a2)
	}
	_, _, err = s.ActorRegister(ctx, me, verbs.ActorRegisterIn{Kind: "human", DisplayName: "x"})
	wantCode(t, "agent cannot register a human", err, "invalid_input")

	// --- context.open ---------------------------------------------------------------------
	_, err = s.ContextOpen(ctx, me, verbs.ContextOpenIn{Project: "litopys-test", Attach: "none"})
	wantCode(t, "open without project", err, "project_not_found")
	open, err := s.ContextOpen(ctx, me, verbs.ContextOpenIn{Project: "litopys-test", Attach: "none", AutoCreate: true,
		Client: []byte(`{"runtime":"claude-code","client_type":"cli"}`)})
	must(t, "open auto-create", err)
	if !open.ProjectCreated || open.Attached || open.RunID == uuid.Nil {
		t.Fatalf("open: %+v", open)
	}
	me.RunID = open.RunID
	re, err := s.ContextOpen(ctx, me, verbs.ContextOpenIn{Project: "litopys-test", Attach: open.ContextID.String()})
	must(t, "re-attach", err)
	if !re.Attached || re.ContextID != open.ContextID {
		t.Fatalf("attach: %+v", re)
	}
	miss, err := s.ContextOpen(ctx, me, verbs.ContextOpenIn{Project: "litopys-test", Attach: uuid.NewString()})
	must(t, "attach miss", err)
	if miss.Attached || miss.AttachMiss != "not_found" || miss.ContextID == open.ContextID {
		t.Fatalf("attach miss: %+v", miss)
	}
	cx := &open.ContextID

	// --- statements -----------------------------------------------------------------------
	st, err := s.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "litopys-test", Type: "decision",
		Title: "Journal is the source of truth", Body: ptr("connect with postgres://u:hunter2@db/x <private>my note</private>"),
		Concepts: []string{"journal"}, ExternalRef: ptr("t/1"), ContextID: cx})
	must(t, "assert", err)
	if !st.Masked || st.Seq == 0 {
		t.Fatalf("assert: %+v", st)
	}
	var body string
	must(t, "body", app.QueryRow(sys, `SELECT body FROM projection.statement WHERE statement_id = $1`, st.StatementID).Scan(&body))
	if strings.Contains(body, "hunter2") || strings.Contains(body, "my note") {
		t.Fatalf("secret or private block stored: %q", body)
	}
	dup, err := s.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "litopys-test", Type: "decision", Title: "again", ExternalRef: ptr("t/1")})
	must(t, "assert dup", err)
	if !dup.Deduped || dup.StatementID != st.StatementID {
		t.Fatalf("external_ref dedup: %+v", dup)
	}
	p1, err := s.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "litopys-test", Type: "note", Title: "long (1/2)",
		ExternalRef: ptr("t/long"), Part: &verbs.Part{Index: 1, Total: 2}, ContextID: cx})
	must(t, "part 1", err)
	p2, err := s.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "litopys-test", Type: "note", Title: "long (2/2)",
		ExternalRef: ptr("t/long"), Part: &verbs.Part{Index: 2, Total: 2}, ContextID: cx})
	must(t, "part 2", err)
	if p1.StatementID == p2.StatementID || p2.Deduped {
		t.Fatal("parts of one external_ref were deduplicated")
	}
	_, err = s.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "litopys-test", Type: "nonsense", Title: "x"})
	wantCode(t, "bad type", err, "type_unknown")
	_, err = s.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "litopys-test", Type: "Not A Type", Title: "x"})
	wantCode(t, "malformed type", err, "invalid_input")

	_, err = s.StatementAmend(ctx, me, verbs.StatementAmendIn{StatementID: st.StatementID, IfRevision: ptr(7), Title: ptr("x")})
	wantCode(t, "amend with stale revision", err, "revision_conflict")
	agentP := verbs.Principal{TenantID: tenant, ActorID: agent, RunID: open.RunID}
	_, err = s.StatementAmend(ctx, agentP, verbs.StatementAmendIn{StatementID: st.StatementID, IfRevision: ptr(1),
		Body: ptr("amended by my agent"), Reason: ptr("agent acts for its human")})
	must(t, "agent amends its human's statement", err)
	otherP := verbs.Principal{TenantID: tenant, ActorID: other}
	_, err = s.StatementRetract(ctx, otherP, st.StatementID, nil)
	wantCode(t, "another human retracts", err, "statement_not_found")

	r1, err := s.StatementRetract(ctx, me, p2.StatementID, ptr("dup"))
	must(t, "retract", err)
	r2, err := s.StatementRetract(ctx, me, p2.StatementID, nil)
	must(t, "retract again", err)
	if r1.Already || !r2.Already {
		t.Fatalf("retract idempotency: %+v %+v", r1, r2)
	}
	_, err = s.StatementAmend(ctx, me, verbs.StatementAmendIn{StatementID: p2.StatementID, Title: ptr("x")})
	wantCode(t, "amend retracted", err, "statement_retracted")
	_, err = s.StatementReinstate(ctx, me, p2.StatementID, nil)
	must(t, "reinstate", err)

	repl, err := s.StatementSupersede(ctx, me, st.StatementID, verbs.StatementAssertIn{Project: "litopys-test", Type: "decision",
		Title: "Journal + projector are the source of truth", ContextID: cx}, ptr("refined"))
	must(t, "supersede", err)
	var status string
	var by *uuid.UUID
	must(t, "superseded", app.QueryRow(sys, `SELECT status, superseded_by FROM projection.statement WHERE statement_id = $1`,
		st.StatementID).Scan(&status, &by))
	if status != "superseded" || by == nil || *by != repl.StatementID {
		t.Fatalf("supersede: %s %v", status, by)
	}

	// --- tasks · facts · links · consumers ------------------------------------------------
	td, err := s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-test", Fields: map[string]any{
		"title": "write verbs", "priority": "high", "status": "blocked", "kind": "bug", "severity": "major",
		"labels": []any{"v2"}, "due": "2026-10-15", "owner": "Ada", "role": "dev"}})
	must(t, "task declare", err)
	again3, err := s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-test", Fields: map[string]any{"title": "write verbs"}})
	must(t, "task declare same title", err)
	if td.Num != 1 || !again3.Existed || again3.Num != 1 {
		t.Fatalf("task num / existed: %+v %+v", td, again3)
	}
	_, err = s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-test", Fields: map[string]any{"title": "orphan", "parent": 99}})
	wantCode(t, "missing parent", err, "parent_not_found")
	child, err := s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-test", Fields: map[string]any{"title": "child", "parent": 1}})
	must(t, "child task", err)
	task := td.TaskID
	_, err = s.TaskAmend(ctx, me, verbs.TaskAmendIn{TaskID: task, IfRevision: ptr(1), Fields: map[string]any{"status": "in_progress", "severity": nil}})
	must(t, "task unblock", err)
	_, err = s.TaskAmend(ctx, me, verbs.TaskAmendIn{TaskID: task, IfRevision: ptr(1), Fields: map[string]any{"priority": "low"}})
	wantCode(t, "task stale revision", err, "revision_conflict")
	_, err = s.TaskAmend(ctx, me, verbs.TaskAmendIn{TaskID: task, Fields: map[string]any{"status": "done"}})
	wantCode(t, "amend cannot close", err, "invalid_input")
	var sev *string
	var st2 string
	must(t, "task row", app.QueryRow(sys, `SELECT status, severity FROM projection.task WHERE task_id = $1`, task).Scan(&st2, &sev))
	if st2 != "in_progress" || sev != nil {
		t.Fatalf("amend: status %s severity %v", st2, sev)
	}
	c1, err := s.TaskClose(ctx, me, task, "done", ptr("shipped"))
	must(t, "task close", err)
	c2, err := s.TaskClose(ctx, me, task, "dropped", nil)
	must(t, "task close again", err)
	if c1.Already || !c2.Already {
		t.Fatal("task close idempotency")
	}
	_, err = s.TaskReopen(ctx, me, task, ptr("review"), ptr("regression"))
	must(t, "task reopen", err)
	_, err = s.TaskRetract(ctx, me, child.TaskID, "duplicate")
	must(t, "task retract", err)
	if _, err = s.TaskByNum(ctx, me, "litopys-test", child.Num); verbs.Code(err) != "task_not_found" {
		t.Fatalf("retracted task still addressable: %v", err)
	}
	next, err := s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-test", Fields: map[string]any{"title": "after retract"}})
	must(t, "task after retract", err)
	if next.Num != 3 {
		t.Fatalf("num reused after retract: %d", next.Num)
	}

	_, err = s.FactRecord(ctx, me, verbs.FactRecordIn{Project: "litopys-test", Kind: "credential", Key: "pg", Value: ptr("hunter2")})
	wantCode(t, "credential with value", err, "credential_value_forbidden")
	_, err = s.FactRecord(ctx, me, verbs.FactRecordIn{Project: "litopys-test", Kind: "credential", Key: "pg", SecretRef: ptr("vault:app/db")})
	must(t, "credential ref", err)
	f1, err := s.FactRecord(ctx, me, verbs.FactRecordIn{Project: "litopys-test", Kind: "note", Key: "stand", Value: ptr("5433")})
	must(t, "fact", err)
	f2, err := s.FactRecord(ctx, me, verbs.FactRecordIn{Project: "litopys-test", Kind: "note", Key: "stand", Value: ptr("5433")})
	must(t, "fact again", err)
	if f1.Unchanged || !f2.Unchanged {
		t.Fatal("fact unchanged")
	}
	_, err = s.FactRetire(ctx, me, "litopys-test", "note", "stand", ptr("moved"))
	must(t, "fact retire", err)
	fr, err := s.FactRetire(ctx, me, "litopys-test", "note", "stand", nil)
	must(t, "fact retire again", err)
	if !fr.Already {
		t.Fatal("fact retire idempotency")
	}

	from := verbs.EntityRef{EntityType: "statement", EntityID: repl.StatementID}
	to := verbs.EntityRef{EntityType: "statement", EntityID: p1.StatementID}
	link, l1, err := s.LinkAssert(ctx, me, from, to, "confirms", nil)
	must(t, "link", err)
	link2, l2, err := s.LinkAssert(ctx, me, from, to, "confirms", nil)
	must(t, "link again", err)
	if l1.Already || !l2.Already || link2 != link {
		t.Fatal("link idempotency")
	}
	_, err = s.LinkRetire(ctx, me, link, nil)
	must(t, "link retire", err)

	// work links between tasks of two projects, owner_project and assignee
	_, _, err = s.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "litopys-other"})
	must(t, "second project", err)
	exec, err := s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-other", Fields: map[string]any{
		"title": "do the work", "assignee": map[string]any{"role": "ops"}}})
	must(t, "executing task", err)
	mirror, err := s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-test", Fields: map[string]any{
		"title": "wait for the work", "status": "waiting", "owner_project": "litopys-other",
		"assignee": map[string]any{"actor_id": ada.String()}}})
	must(t, "mirror task", err)
	_, err = s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-test", Fields: map[string]any{
		"title": "own project as owner", "owner_project": "litopys-test"}})
	wantCode(t, "owner_project = own", err, "invalid_input")
	_, err = s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-test", Fields: map[string]any{
		"title": "two assignees", "assignee": map[string]any{"role": "ops", "agent": "planner"}}})
	wantCode(t, "assignee with two keys", err, "invalid_input")
	_, err = s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-test", Fields: map[string]any{
		"title": "ghost assignee", "assignee": map[string]any{"actor_id": uuid.NewString()}}})
	wantCode(t, "unknown assignee actor", err, "invalid_input")

	tEnd := func(id uuid.UUID) verbs.EntityRef { return verbs.EntityRef{EntityType: "task", EntityID: id} }
	pEnd := func(id uuid.UUID) verbs.EntityRef { return verbs.EntityRef{EntityType: "project", EntityID: id} }
	_, _, err = s.LinkAssert(ctx, me, tEnd(mirror.TaskID), tEnd(exec.TaskID), "handoff", nil)
	must(t, "handoff between projects", err)
	_, _, err = s.LinkAssert(ctx, me, tEnd(mirror.TaskID), tEnd(next.TaskID), "handoff", nil)
	wantCode(t, "handoff within one project", err, "invalid_input")
	_, _, err = s.LinkAssert(ctx, me, tEnd(mirror.TaskID), tEnd(task), "predecessor_of", nil)
	wantCode(t, "project relation on tasks", err, "invalid_input")
	_, _, err = s.LinkAssert(ctx, me, tEnd(next.TaskID), tEnd(mirror.TaskID), "depends_on", nil)
	must(t, "depends_on", err)
	_, _, err = s.LinkAssert(ctx, me, tEnd(mirror.TaskID), tEnd(task), "depends_on", nil)
	must(t, "depends_on chain", err)
	_, _, err = s.LinkAssert(ctx, me, tEnd(task), tEnd(next.TaskID), "depends_on", nil)
	wantCode(t, "depends_on cycle over two hops", err, "invalid_input")
	d1, _, err := s.LinkAssert(ctx, me, tEnd(next.TaskID), tEnd(exec.TaskID), "duplicates", nil)
	must(t, "duplicates", err)
	d2, dAck, err := s.LinkAssert(ctx, me, tEnd(exec.TaskID), tEnd(next.TaskID), "duplicates", nil)
	must(t, "duplicates reversed", err)
	if !dAck.Already || d2 != d1 {
		t.Fatalf("symmetric duplicates reversed: %v %v", dAck.Already, d2 == d1)
	}
	var testID, otherID uuid.UUID
	must(t, "project ids", app.QueryRow(sys, `SELECT (SELECT project_id FROM projection.project WHERE name = 'litopys-test'),
	        (SELECT project_id FROM projection.project WHERE name = 'litopys-other')`).Scan(&testID, &otherID))
	_, _, err = s.LinkAssert(ctx, me, pEnd(testID), pEnd(otherID), "related", nil)
	must(t, "related between projects", err)
	_, _, err = s.LinkAssert(ctx, me, tEnd(mirror.TaskID), tEnd(mirror.TaskID), "related", nil)
	wantCode(t, "self link", err, "invalid_input")

	var op uuid.UUID
	var asg string
	must(t, "mirror row", app.QueryRow(sys, `SELECT owner_project, assignee::text FROM projection.task WHERE task_id = $1`, mirror.TaskID).Scan(&op, &asg))
	if op != otherID || !strings.Contains(asg, ada.String()) {
		t.Fatalf("mirror: owner_project %v assignee %s", op, asg)
	}
	_, err = s.TaskAmend(ctx, me, verbs.TaskAmendIn{TaskID: mirror.TaskID, Fields: map[string]any{"owner_project": nil, "assignee": nil}})
	must(t, "clear owner_project and assignee", err)
	var opNull, asgNull bool
	must(t, "mirror cleared", app.QueryRow(sys, `SELECT owner_project IS NULL, assignee IS NULL FROM projection.task WHERE task_id = $1`, mirror.TaskID).Scan(&opNull, &asgNull))
	if !opNull || !asgNull {
		t.Fatalf("clear: owner_project null %v assignee null %v", opNull, asgNull)
	}

	_, err = s.ConsumerCheckpoint(ctx, me, "vault-harvest", 1_000_000)
	wantCode(t, "cursor beyond head", err, "seq_out_of_range")
	_, err = s.ConsumerCheckpoint(ctx, me, "vault-harvest", 5)
	must(t, "cursor", err)
	back, err := s.ConsumerCheckpoint(ctx, me, "vault-harvest", 3)
	must(t, "cursor back", err)
	if !back.Unchanged {
		t.Fatal("cursor moved backwards")
	}

	// --- checkpoint · purge · close -------------------------------------------------------
	k1, err := s.ContextCheckpoint(ctx, me, open.ContextID, "halfway", ptr("writing verbs"))
	must(t, "checkpoint", err)
	k2, err := s.ContextCheckpoint(ctx, me, open.ContextID, "halfway", nil)
	must(t, "checkpoint again", err)
	if k1.Deduped || !k2.Deduped {
		t.Fatal("checkpoint dedup")
	}
	_, err = s.JournalPurge(ctx, me, "statement", p1.StatementID, "test purge", ptr("art.17"))
	must(t, "purge", err)

	closed, err := s.ContextClose(ctx, me, open.ContextID, "done for today", nil)
	must(t, "close", err)
	if !closed.Closed || closed.Checkpoint == nil || len(closed.Checkpoint.ContextEventIDs) != 4 || closed.Checkpoint.Project != "litopys-test" {
		t.Fatalf("close: %+v %+v", closed, closed.Checkpoint)
	}
	again2, err := s.ContextClose(ctx, me, open.ContextID, "one more thought", nil)
	must(t, "close again", err)
	if !again2.AlreadyClosed {
		t.Fatal("close idempotency")
	}
	_, err = s.ContextClose(ctx, me, miss.ContextID, "second context", nil)
	must(t, "close second context", err)
	var finished bool
	must(t, "run", app.QueryRow(sys, `SELECT finished_at IS NOT NULL FROM projection.run WHERE run_id = $1`, open.RunID).Scan(&finished))
	if !finished {
		t.Fatal("run not finished after its last context closed")
	}

	// --- the commands' projections survive a rebuild unchanged (E7 for the verb layer) -----
	before := dump(t, app)
	if _, err := projector.Rebuild(ctx, app, tenant); err != nil {
		t.Fatal(err)
	}
	if after := dump(t, app); !reflect.DeepEqual(before, after) {
		for k := range before {
			if !reflect.DeepEqual(before[k], after[k]) {
				t.Errorf("rebuild changed %s", k)
			}
		}
	}
}

func dump(t *testing.T, q *pgxpool.Pool) map[string][]string {
	t.Helper()
	return dumpTables(t, q, "actor", "run", "context", "project", "task", "fact", "statement", "statement_version",
		"statement_lexical", "link", "consumer", "search_vocab")
}

func dumpTables(t *testing.T, q *pgxpool.Pool, tables ...string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, tbl := range tables {
		rows, err := q.Query(context.Background(), `SELECT row_to_json(t)::text FROM projection.`+tbl+` t ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		v, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		out[tbl] = v
	}
	return out
}
