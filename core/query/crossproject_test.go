package query_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestCrossProjectCheck: work.open_items of a project names what to fix in its mirrors and handoffs
// and which of its tasks other projects wait on — the owner closed or retracted the task a mirror points at, a
// mirror without a handoff link, the same Jira key open in two projects without a handoff between them.
func TestCrossProjectCheck(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	v := &verbs.Service{DB: app}
	q := &query.Service{DB: app}
	var tenant uuid.UUID
	must(t, "tenant", app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant))
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must(t, "human", err)
	sp := verbs.Principal{TenantID: tenant, ActorID: ada}
	me := query.Principal{TenantID: tenant, ActorID: ada}

	task := func(project, title string, fields map[string]any) verbs.TaskDeclareOut {
		t.Helper()
		_, _, _ = v.ProjectDeclare(ctx, sp, verbs.ProjectDeclareIn{Name: project})
		f := map[string]any{"title": title}
		for k, x := range fields {
			f[k] = x
		}
		out, err := v.TaskDeclare(ctx, sp, verbs.TaskDeclareIn{Project: project, Fields: f})
		must(t, "task "+title, err)
		return out
	}
	handoff := func(mirror, owner verbs.TaskDeclareOut) {
		t.Helper()
		_, _, err := v.LinkAssert(ctx, sp, verbs.EntityRef{EntityType: "task", EntityID: mirror.TaskID},
			verbs.EntityRef{EntityType: "task", EntityID: owner.TaskID}, "handoff", nil)
		must(t, "handoff", err)
	}
	check := func(project string) *query.CrossProject {
		t.Helper()
		open, err := q.WorkOpenItems(ctx, me, project)
		must(t, "open items "+project, err)
		return open.CrossProject
	}
	kinds := func(cp *query.CrossProject) string {
		if cp == nil {
			return "[]"
		}
		var out []string
		for _, f := range cp.Findings {
			out = append(out, f.Kind+" "+f.Task+" "+f.Other+f.Key)
		}
		return fmt.Sprint(out)
	}

	backup := task("workshop-infra", "Backup of the vault", map[string]any{"refs": []string{"jira:ACME-7"}})
	mirror := task("workshop", "Vault is backed up", map[string]any{"status": "waiting", "labels": []string{"mirror"}})
	handoff(mirror, backup)
	if cp := check("workshop"); cp != nil {
		t.Errorf("a linked mirror of an open task is sound: %v", kinds(cp))
	}
	if cp := check("workshop-infra"); cp == nil || fmt.Sprint(cp.WaitingOnUs) != "[{workshop-infra/T-1 Backup of the vault workshop/T-1}]" {
		t.Errorf("waiting on the owner: %+v", cp)
	}

	_, err = v.TaskClose(ctx, sp, backup.TaskID, "done", nil)
	must(t, "close owner task", err)
	if got := kinds(check("workshop")); got != "[handoff_closed workshop/T-1 workshop-infra/T-1]" {
		t.Errorf("owner closed: %s", got)
	}

	gone := task("truenas", "Snapshots", nil)
	mirror2 := task("workshop", "Snapshots exist", map[string]any{"status": "waiting", "labels": []string{"mirror"}})
	handoff(mirror2, gone)
	_, err = v.TaskRetract(ctx, sp, gone.TaskID, "wrong project")
	must(t, "retract owner task", err)
	unlinked := task("workshop", "Waits for someone", map[string]any{"status": "waiting", "labels": []string{"handoff:truenas"}})
	_ = unlinked
	got := kinds(check("workshop"))
	want := "[handoff_closed workshop/T-1 workshop-infra/T-1 handoff_gone workshop/T-2 truenas/T-1 mirror_unlinked workshop/T-3 ]"
	if got != want {
		t.Errorf("findings:\n got %s\nwant %s", got, want)
	}

	// the same Jira key open in two projects: a finding on both sides until a handoff joins them
	a := task("corp-a", "Migrate the reports", map[string]any{"refs": []string{"jira:ACME-9"}})
	b := task("corp-b", "Reports migration", map[string]any{"refs": []string{"JIRA:ACME-9", "pr:http://x/1"}})
	if got := kinds(check("corp-a")); got != "[duplicate_key corp-a/T-1 corp-b/T-1jira:acme-9]" {
		t.Errorf("duplicate key at a: %s", got)
	}
	if got := kinds(check("corp-b")); got != "[duplicate_key corp-b/T-1 corp-a/T-1jira:acme-9]" {
		t.Errorf("duplicate key at b: %s", got)
	}
	handoff(b, a)
	if cp := check("corp-a"); cp == nil || len(cp.Findings) != 0 || len(cp.WaitingOnUs) != 1 {
		t.Errorf("after the handoff a has no finding and one waiting mirror: %+v", cp)
	}
	if cp := check("corp-b"); cp != nil {
		t.Errorf("after the handoff b is sound: %v", kinds(cp))
	}
}
