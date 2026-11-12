package verbs_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestWorkPrivacy: tasks, the project summary and the notes of runs strip <private> and mask secrets before the
// journal, like statements.
func TestWorkPrivacy(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	s := &verbs.Service{DB: app, Admin: db.RolePool(t, "litopys_admin", 1)}
	var tenant uuid.UUID
	must(t, "tenant", app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant))
	ada, _, err := s.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must(t, "human", err)
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	agent, _, err := s.ActorRegister(ctx, me, verbs.ActorRegisterIn{Kind: "agent", DisplayName: "runner", ExternalIdentity: ptr("agent:runner")})
	must(t, "agent", err)
	bot := verbs.Principal{TenantID: tenant, ActorID: agent}

	const text = "password: hunter2 <private>my note</private>"
	const line = "deploy via postgres://u:hunter2@db/x <private>my note</private>"

	_, _, err = s.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "litopys-test", Title: ptr(line), Summary: ptr(text)})
	must(t, "project", err)
	_, err = s.ProjectAmend(ctx, me, verbs.ProjectAmendIn{Project: "litopys-test", Summary: ptr("v2 " + text)})
	must(t, "project amend", err)

	task, err := s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-test",
		Fields: map[string]any{"title": line, "body": text, "acceptance": text}})
	must(t, "task", err)
	again, err := s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-test", Fields: map[string]any{"title": line}})
	must(t, "task again", err)
	if !again.Existed || again.TaskID != task.TaskID {
		t.Fatalf("the same title after cleaning must find the task: %+v", again)
	}
	_, err = s.TaskAmend(ctx, me, verbs.TaskAmendIn{TaskID: task.TaskID,
		Fields: map[string]any{"title": "v2 " + line, "body": "v2 " + text, "acceptance": "v2 " + text}})
	must(t, "task amend", err)
	_, err = s.TaskClose(ctx, me, task.TaskID, "done", ptr(text))
	must(t, "task close", err)
	_, err = s.TaskReopen(ctx, me, task.TaskID, nil, ptr(text))
	must(t, "task reopen", err)
	_, err = s.TaskDeclare(ctx, me, verbs.TaskDeclareIn{Project: "litopys-test", Fields: map[string]any{"title": "<private>only private</private>"}})
	wantCode(t, "a title that is only a private block", err, "invalid_input")

	def := `{"steps": [{"key": "go", "title": "GO", "kind": "human"}, {"key": "do", "title": "Do", "kind": "model"}]}`
	_, err = s.WorkflowDeclare(ctx, me, verbs.WorkflowDeclareIn{Project: "litopys-test", Key: "p", Title: "P", Definition: json.RawMessage(def)})
	must(t, "workflow", err)
	run, err := s.ExecutionStart(ctx, me, verbs.ExecutionStartIn{Project: "litopys-test", Workflow: "p"})
	must(t, "start", err)
	_, err = s.ApprovalGrant(ctx, me, verbs.ApprovalGrantIn{ExecutionID: run.ExecutionID, StepKey: "go", Note: ptr(text)})
	must(t, "GO", err)
	a, err := s.StepStart(ctx, bot, verbs.StepStartIn{ExecutionID: run.ExecutionID, StepKey: "do"})
	must(t, "take", err)
	_, err = s.StepCheckpoint(ctx, bot, verbs.StepCheckpointIn{AttemptID: a.AttemptID, Note: ptr(text)})
	must(t, "checkpoint", err)
	_, err = s.StepFinish(ctx, bot, verbs.StepFinishIn{AttemptID: a.AttemptID, State: "failed", Error: ptr(text)})
	must(t, "fail", err)
	_, err = s.ExecutionClose(ctx, me, verbs.ExecutionCloseIn{ExecutionID: run.ExecutionID, Outcome: "canceled", Summary: ptr(text)})
	must(t, "close", err)

	rows, err := app.Query(sys, `SELECT type, payload::text FROM journal WHERE tenant_id = $1 ORDER BY seq`, tenant)
	must(t, "journal", err)
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var typ, payload string
		must(t, "scan", rows.Scan(&typ, &payload))
		if strings.Contains(payload, "hunter2") || strings.Contains(payload, "my note") || strings.Contains(payload, "only private") {
			t.Errorf("%s keeps a secret or a private block: %s", typ, payload)
		}
		if strings.Contains(payload, "password: ***") || strings.Contains(payload, "u:***@db") {
			seen[typ] = true
		}
	}
	must(t, "rows", rows.Err())
	for _, typ := range []string{"project.declared", "project.amended", "task.declared", "task.amended", "task.closed",
		"task.reopened", "approval.granted", "step.checkpointed", "step.finished", "execution.closed"} {
		if !seen[typ] {
			t.Errorf("%s: no masked value in the payload", typ)
		}
	}
}
