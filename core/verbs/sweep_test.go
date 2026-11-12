package verbs_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestSweepIdleSessions: sessions idle past the cut are closed by the owner's automation with
// auto: true — a checkpoint summary is kept, a session without one gets AutoClosedSummary, the run finishes with
// timeout; a session with a later event stays open; a second pass closes nothing.
func TestSweepIdleSessions(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	s := &verbs.Service{DB: app}
	var tenant uuid.UUID
	must(t, "tenant", app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant))
	ada, _, err := s.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must(t, "human", err)
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	_, _, err = s.ProjectDeclare(ctx, me, verbs.ProjectDeclareIn{Name: "litopys-test"})
	must(t, "project", err)

	open := func() verbs.ContextOpenOut {
		t.Helper()
		o, err := s.ContextOpen(ctx, me, verbs.ContextOpenIn{Project: "litopys-test", Attach: "none"})
		must(t, "open", err)
		return o
	}
	bare, kept := open(), open()
	_, err = s.ContextCheckpoint(ctx, me, kept.ContextID, "half done", nil)
	must(t, "checkpoint", err)
	// the cut on the database clock: the host clock may be seconds off (doctor's clock check)
	var mark time.Time
	must(t, "clock", app.QueryRow(sys, `SELECT clock_timestamp()`).Scan(&mark))
	fresh := open()

	n, err := s.SweepIdleSessions(ctx, tenant, mark, time.Millisecond)
	must(t, "sweep", err)
	if n != 2 {
		t.Fatalf("swept %d sessions, want 2", n)
	}
	state := func(id uuid.UUID) (closed bool, auto *bool, summary *string, closer string) {
		t.Helper()
		must(t, "state", app.QueryRow(sys, `SELECT c.closed_at IS NOT NULL, c.closed_auto, c.summary,
		        COALESCE((SELECT a.external_identity FROM journal j JOIN projection.actor a ON a.actor_id = j.actor_id
		                   WHERE j.entity_id = c.context_id AND j.type = 'context.closed'), '')
		   FROM projection.context c WHERE c.context_id = $1`, id).Scan(&closed, &auto, &summary, &closer))
		return
	}
	if closed, auto, summary, closer := state(bare.ContextID); !closed || auto == nil || !*auto || summary == nil ||
		*summary != verbs.AutoClosedSummary || closer != "litopys:sessions" {
		t.Errorf("bare session: closed %v auto %v summary %v by %q", closed, auto, summary, closer)
	}
	if closed, _, summary, _ := state(kept.ContextID); !closed || summary == nil || *summary != "half done" {
		t.Errorf("checkpointed session: closed %v summary %v (the checkpoint summary must stay)", closed, summary)
	}
	if closed, _, _, _ := state(fresh.ContextID); closed {
		t.Errorf("a session with an event after the cut was closed")
	}
	var outcome *string
	must(t, "run", app.QueryRow(sys, `SELECT (SELECT j.payload->>'outcome' FROM journal j WHERE j.entity_id = $1 AND j.type = 'run.finished')`,
		bare.RunID).Scan(&outcome))
	if outcome == nil || *outcome != "timeout" {
		t.Errorf("run of the swept session: outcome %v, want timeout", outcome)
	}
	if n, err := s.SweepIdleSessions(ctx, tenant, mark, time.Millisecond); err != nil || n != 0 {
		t.Errorf("second pass: %d, %v", n, err)
	}
}
