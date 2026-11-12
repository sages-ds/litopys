// Package gates holds the acceptance gates as Go tests against the stand:
// immutability, E6 (changefeed exactness) and E7 (replay determinism).
// Each test gets a throw-away database (internal/pgtest) and is skipped without stand credentials.
package gates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"litopys.smart-gw.co.ua/core/journal"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/projector"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// stand creates a migrated throw-away database and returns it with the default tenant id.
func stand(t *testing.T) (*pgtest.DB, uuid.UUID) {
	t.Helper()
	ctx := pg.WithScope(context.Background(), pg.ScopeSystem) // gates read every owner (RLS of 0008)
	db := pgtest.New(t)
	mig := db.MigratorConn(t)
	if _, err := migrate.Up(ctx, mig); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	var tenant uuid.UUID
	if err := mig.QueryRow(ctx, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	return db, tenant
}

func sqlstate(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// ---------------------------------------------------------------------------
// Immutability
// ---------------------------------------------------------------------------

func TestGateImmutability(t *testing.T) {
	ctx := pg.WithScope(context.Background(), pg.ScopeSystem) // gates read every owner (RLS of 0008)
	db, tenant := stand(t)
	app := db.RolePool(t, "litopys_app", 2)
	admin := db.RolePool(t, "litopys_admin", 1)
	mig := db.MigratorConn(t)

	ent, run := uuid.New(), uuid.New()
	for i := 0; i < 2; i++ {
		err := pg.InTx(ctx, app, func(tx pgx.Tx) error {
			return journal.Append(ctx, tx, &journal.Event{TenantID: tenant, Type: "statement.asserted",
				ActorID: run, RunID: run, CausationID: run, CorrelationID: run,
				EntityType: "statement", EntityID: ent, Payload: json.RawMessage(`{"title":"secret"}`)})
		})
		if err != nil {
			t.Fatalf("app append: %v", err)
		}
	}

	refused := func(name string, want string, exec func() error) {
		t.Helper()
		err := exec()
		if err == nil {
			t.Errorf("%s: succeeded, want %s", name, want)
		} else if got := sqlstate(err); got != want {
			t.Errorf("%s: sqlstate %s (%v), want %s", name, got, err, want)
		}
	}
	appExec := func(sql string, args ...any) func() error {
		return func() error { _, err := app.Exec(ctx, sql, args...); return err }
	}
	migExec := func(sql string) func() error {
		return func() error { _, err := mig.Exec(ctx, sql); return err }
	}
	purge := `SELECT journal_purge($1,'statement',$2,'gate','test',$3,$3,$3,$3)`

	refused("app UPDATE journal", "42501", appExec(`UPDATE journal SET payload = '{}' WHERE seq = 1`))
	refused("app DELETE journal", "42501", appExec(`DELETE FROM journal WHERE seq = 1`))
	refused("app TRUNCATE journal", "42501", appExec(`TRUNCATE journal`))
	refused("app journal_purge", "42501", appExec(purge, tenant, ent, run))
	refused("owner UPDATE journal", "23000", migExec(`UPDATE journal SET payload = '{}' WHERE seq = 1`))
	refused("owner DELETE journal", "23000", migExec(`DELETE FROM journal WHERE seq = 1`))
	refused("owner TRUNCATE journal", "23000", migExec(`TRUNCATE journal`))
	refused("owner TRUNCATE journal_head", "23000", migExec(`TRUNCATE journal_head`))

	var pseq int64
	if err := admin.QueryRow(ctx, purge, tenant, ent, run).Scan(&pseq); err != nil {
		t.Fatalf("admin purge: %v", err)
	}
	events, err := journal.Read(ctx, admin, tenant, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].Type != "journal.purged" || events[2].Seq != pseq || pseq != 3 {
		t.Fatalf("purge event: seq %d, events %d", pseq, len(events))
	}
	for _, e := range events[:2] {
		var m map[string]any
		_ = json.Unmarshal(e.Payload, &m)
		if m["purged"] != true || m["title"] != nil {
			t.Errorf("seq %d not purged: %s", e.Seq, e.Payload)
		}
	}
	var flag *string
	if err := admin.QueryRow(ctx, `SELECT current_setting('litopys.purge', true)`).Scan(&flag); err != nil {
		t.Fatal(err)
	}
	if flag != nil && *flag == "on" {
		t.Error("litopys.purge left on after the call")
	}
}

// ---------------------------------------------------------------------------
// E6 · changefeed exactness
// ---------------------------------------------------------------------------

func TestGateE6ChangefeedExactness(t *testing.T) {
	const writers, perWriter, rollbackEvery, page = 8, 150, 7, 25
	ctx := pg.WithScope(context.Background(), pg.ScopeSystem) // gates read every owner (RLS of 0008)
	db, tenant := stand(t)
	app := db.RolePool(t, "litopys_app", writers+2)

	var (
		mu          sync.Mutex
		committed   int
		wg          sync.WaitGroup
		writersDone = make(chan struct{})
		done        = make(chan struct{})
		readErr     error
		seen        []int64
		pages       int
	)
	go func() { // reader: small pages while writers run
		defer close(done)
		var cursor int64
		for {
			finished := false
			select {
			case <-writersDone:
				finished = true // every commit happened before this point
			default:
			}
			events, err := journal.Read(ctx, app, tenant, cursor, page)
			if err != nil {
				readErr = err
				return
			}
			pages++
			for _, e := range events {
				seen = append(seen, e.Seq)
			}
			if len(events) == 0 {
				if finished {
					return
				}
				time.Sleep(2 * time.Millisecond)
				continue
			}
			cursor = events[len(events)-1].Seq
			for i, s := range seen { // "seen N => seen everything <= N"
				if s != int64(i+1) {
					readErr = fmt.Errorf("page %d: hole or dup at position %d (seq %d)", pages, i, s)
					return
				}
			}
		}
	}()

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			run := uuid.New()
			for i := 0; i < perWriter; i++ {
				tx, err := app.Begin(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				err = journal.Append(ctx, tx, &journal.Event{TenantID: tenant, Type: "statement.asserted",
					ActorID: run, RunID: run, CausationID: run, CorrelationID: run,
					EntityType: "statement", EntityID: uuid.New(), Payload: json.RawMessage(`{"title":"e6"}`)})
				if err != nil {
					_ = tx.Rollback(ctx)
					t.Error(err)
					return
				}
				if (i+w)%rollbackEvery == 0 {
					_ = tx.Rollback(ctx) // the number must be given back
					continue
				}
				if err := tx.Commit(ctx); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				committed++
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	close(writersDone)
	<-done
	if readErr != nil {
		t.Fatal(readErr)
	}
	want := writers*perWriter - rollbacks(writers, perWriter, rollbackEvery)
	if committed != want || len(seen) != want {
		t.Fatalf("committed %d, reader saw %d, want %d", committed, len(seen), want)
	}
	var head, maxSeq, rows int64
	if err := app.QueryRow(ctx, `SELECT (SELECT last_seq FROM journal_head WHERE tenant_id = $1),
	        (SELECT coalesce(max(seq), 0) FROM journal), (SELECT count(*) FROM journal)`, tenant).Scan(&head, &maxSeq, &rows); err != nil {
		t.Fatal(err)
	}
	if head != int64(want) || maxSeq != int64(want) || rows != int64(want) {
		t.Fatalf("rolled-back numbers not given back: head %d, max %d, rows %d, want %d", head, maxSeq, rows, want)
	}
	t.Logf("E6: %d committed, %d rolled back, %d reader pages", want, writers*perWriter-want, pages)
}

func rollbacks(writers, perWriter, every int) int {
	n := 0
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			if (i+w)%every == 0 {
				n++
			}
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// E7 · replay determinism
// ---------------------------------------------------------------------------

type scenario struct {
	t      *testing.T
	ctx    context.Context
	app    *pgxpool.Pool
	tenant uuid.UUID
	actor  uuid.UUID
	run    uuid.UUID
	types  map[string]int
}

// emit is the live write path of a command verb: journal_append -> apply -> commit, one tx.
func (s *scenario) emit(typ, entityType string, entity uuid.UUID, ctxID *uuid.UUID, payload any) {
	s.t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		s.t.Fatal(err)
	}
	e := &journal.Event{TenantID: s.tenant, Type: typ, ActorID: s.actor, RunID: s.run,
		CausationID: s.run, CorrelationID: s.run, EntityType: entityType, EntityID: entity,
		ContextID: ctxID, Payload: b}
	err = pg.InTx(s.ctx, s.app, func(tx pgx.Tx) error {
		if err := journal.Append(s.ctx, tx, e); err != nil {
			return err
		}
		return projector.Apply(s.ctx, tx, e)
	})
	if err != nil {
		s.t.Fatalf("%s: %v", typ, err)
	}
	s.types[typ]++
}

type m = map[string]any

func TestGateE7ReplayDeterminism(t *testing.T) {
	ctx := pg.WithScope(context.Background(), pg.ScopeSystem) // gates read every owner (RLS of 0008)
	db, tenant := stand(t)
	app := db.RolePool(t, "litopys_app", 2)
	admin := db.RolePool(t, "litopys_admin", 1)
	s := &scenario{t: t, ctx: ctx, app: app, tenant: tenant, actor: uuid.New(), run: uuid.New(), types: map[string]int{}}

	project, cx := uuid.New(), uuid.New()
	stA, stB, stC, stD := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	task, fact, cred, agent := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	confirm, related := uuid.New(), uuid.New()
	ref := func(typ string, id uuid.UUID) m { return m{"entity_type": typ, "entity_id": id} }

	s.emit("actor.registered", "actor", s.actor, nil, m{"kind": "human", "display_name": "Ada", "external_identity": "ada@example"})
	s.emit("run.started", "run", s.run, nil, m{"client": m{"runtime": "claude-code", "client_type": "cli"}, "purpose": "E7"})
	s.emit("project.declared", "project", project, nil, m{"name": "e7-project", "summary": "gate", "auto_created": false})
	s.emit("context.opened", "context", cx, nil, m{"project_id": project})
	s.emit("statement.asserted", "statement", stA, &cx, m{"project_id": project, "type": "decision", "title": "A v1",
		"body": "alpha body", "concepts": []string{"journal", "seq"}, "files": []string{"notes/journal.md"}, "external_ref": "e7/a"})
	s.emit("statement.asserted", "statement", stB, &cx, m{"project_id": project, "type": "discovery", "title": "B", "body": "beta"})
	s.emit("statement.amended", "statement", stA, &cx, m{"title": "A v2", "body": "alpha body amended", "reason": "typo"})
	s.emit("statement.retracted", "statement", stB, &cx, m{"reason": "wrong"})
	s.emit("statement.reinstated", "statement", stB, &cx, m{"reason": "was right"})
	s.emit("statement.asserted", "statement", stC, &cx, m{"project_id": project, "type": "decision", "title": "C replaces A",
		"part": m{"index": 1, "total": 2}})
	s.emit("statement.superseded", "statement", stA, &cx, m{"superseded_by": stC, "reason": "new decision"})
	s.emit("link.asserted", "link", confirm, nil, m{"from": ref("statement", stC), "to": ref("statement", stB), "relation": "confirms"})
	s.emit("link.asserted", "link", related, nil, m{"from": ref("statement", stA), "to": ref("statement", stB), "relation": "related", "note": "n"})
	s.emit("link.retired", "link", related, nil, m{"reason": "noise"})
	s.emit("task.declared", "task", task, nil, m{"project_id": project, "num": 1, "title": "T", "status": "open", "priority": "high",
		"body": "first", "kind": "bug", "severity": "major", "labels": []string{"gate"}, "due": "2026-10-15", "role": "dba"})
	s.emit("task.amended", "task", task, nil, m{"priority": "low", "status": "blocked", "owner": "Ada", "refs": []string{"jira:X-465"}})
	s.emit("task.amended", "task", task, nil, m{"severity": nil, "labels": nil, "status": "in_progress"})
	s.emit("task.closed", "task", task, nil, m{"outcome": "done", "resolution": "closed"})
	s.emit("task.reopened", "task", task, nil, m{"status": "review", "reason": "regression"})
	dupe := uuid.New()
	s.emit("task.declared", "task", dupe, nil, m{"project_id": project, "num": 2, "title": "dupe", "status": "open", "parent": 1})
	s.emit("task.retracted", "task", dupe, nil, m{"reason": "duplicate"})
	s.emit("fact.recorded", "fact", fact, nil, m{"project_id": project, "kind": "note", "key": "stand", "value": "5433"})
	s.emit("fact.recorded", "fact", fact, nil, m{"project_id": project, "kind": "note", "key": "stand", "value": "5433/litopys"})
	s.emit("fact.recorded", "fact", cred, nil, m{"project_id": project, "kind": "credential", "key": "pg", "secret_ref": "vault:app/db"})
	s.emit("fact.retired", "fact", fact, nil, m{"reason": "moved"})
	s.emit("project.amended", "project", project, nil, m{"summary": "gate E7", "repo_url": "http://example/repo"})
	s.emit("project.transitioned", "project", project, nil, m{"status": "paused", "reason": "gate"})
	s.emit("project.amended", "project", project, nil, m{"labels": []string{"corp-only", "gate"}})
	s.emit("policy.set", "policy", uuid.New(), nil, m{"name": "no-openai-corp", "scope": "tenant", "active": true,
		"rule": m{"type": "project_label_deny", "labels": []string{"corp-only"}, "vendors": []string{"openai"}}})
	pid := uuid.New()
	s.emit("policy.set", "policy", pid, nil, m{"name": "redact-values", "scope": "owner", "active": true,
		"rule": m{"type": "field_redaction", "fields": []string{"fact.value"}}})
	s.emit("policy.set", "policy", pid, nil, m{"name": "redact-values", "scope": "owner", "active": false, "reason": "off",
		"rule": m{"type": "field_redaction", "fields": []string{"fact.value"}}})
	s.emit("context.checkpointed", "context", cx, nil, m{"summary": "midway", "current": "writing E7"})
	s.emit("consumer.cursor_set", "consumer", uuid.New(), nil, m{"consumer": "vault-harvest", "seq": 5})
	s.emit("statement.asserted", "statement", stD, &cx, m{"project_id": project, "type": "note", "title": "D to purge",
		"body": "personal data", "concepts": []string{"pii"}, "external_ref": "e7/d"})
	s.emit("statement.amended", "statement", stD, &cx, m{"body": "more personal data", "reason": "add"})

	tp := uuid.New() // a task to purge (0033): its structure survives the markers, its content does not
	s.emit("task.declared", "task", tp, nil, m{"project_id": project, "num": 3, "title": "rotate key K", "status": "open",
		"body": "the key is K", "kind": "risk", "priority": "high", "labels": []string{"secret"}, "owner": "Ada", "parent": 1})
	s.emit("task.amended", "task", tp, nil, m{"status": "in_progress", "acceptance": "K gone", "refs": []string{"jira:X-1"}, "rank": 5})
	s.emit("task.closed", "task", tp, nil, m{"outcome": "dropped", "resolution": "K rotated"})

	// quarantine (0034): B goes and comes back (search rows rebuilt from its current content), Q stays hidden with its
	// words out of the dictionaries; a quarantined task stays hidden
	stQ, tq := uuid.New(), uuid.New()
	s.emit("statement.asserted", "statement", stQ, &cx, m{"project_id": project, "type": "note", "title": "Q secret",
		"body": "quokkapass token", "concepts": []string{"quokka"}})
	s.emit("journal.quarantined", "statement", stQ, nil, m{"target": ref("statement", stQ), "reason": "secret", "secret": true})
	s.emit("journal.quarantined", "statement", stB, nil, m{"target": ref("statement", stB), "reason": "check"})
	s.emit("statement.amended", "statement", stB, &cx, m{"body": "beta amended in quarantine", "reason": "fix"})
	s.emit("journal.released", "statement", stB, nil, m{"target": ref("statement", stB), "reason": "fine"})
	s.emit("task.declared", "task", tq, nil, m{"project_id": project, "num": 4, "title": "quokka task", "status": "open"})
	s.emit("journal.quarantined", "task", tq, nil, m{"target": ref("task", tq), "reason": "secret"})
	stZ := uuid.New() // quarantined, then erased (purged below): the purge marker replaces this quarantine's payload too
	s.emit("statement.asserted", "statement", stZ, &cx, m{"project_id": project, "type": "note", "title": "Z zebrakey",
		"body": "zebrakey value"})
	s.emit("journal.quarantined", "statement", stZ, nil, m{"target": ref("statement", stZ), "reason": "secret", "secret": true})

	// deletion proposals (0035): one approved and done by a retract, one rejected, one approved and executed
	d1, d2, d3 := uuid.New(), uuid.New(), uuid.New()
	s.emit("deletion.proposed", "deletion", d1, nil, m{"project_id": project, "num": 1, "target": ref("statement", stC),
		"action": "retract", "reason": "old", "impact": m{"confirms": 1}})
	s.emit("deletion.approved", "deletion", d1, nil, m{"note": "go"})
	s.emit("statement.retracted", "statement", stC, &cx, m{"reason": "deletion e7-project/D-1"})
	s.emit("deletion.proposed", "deletion", d2, nil, m{"project_id": project, "num": 2, "target": ref("task", task),
		"action": "purge", "reason": "dupe", "impact": m{}})
	s.emit("deletion.rejected", "deletion", d2, nil, m{"reason": "keep"})
	s.emit("deletion.proposed", "deletion", d3, nil, m{"project_id": project, "target": ref("statement", stQ),
		"action": "purge", "reason": "secret", "secret": true, "impact": m{}})
	s.emit("deletion.approved", "deletion", d3, nil, m{})
	s.emit("deletion.executed", "deletion", d3, nil, m{"purge_seq": 1})

	// a second person with a project of the same name (0008): owners must replay identically too
	me, myRun := s.actor, s.run
	s.actor, s.run = uuid.New(), uuid.New()
	otherProject := uuid.New()
	s.emit("actor.registered", "actor", s.actor, nil, m{"kind": "human", "display_name": "Other", "external_identity": "other@example"})
	s.emit("run.started", "run", s.run, nil, m{"purpose": "E7 second owner"})
	s.emit("project.declared", "project", otherProject, nil, m{"name": "e7-project", "summary": "same name, other owner"})
	s.emit("statement.asserted", "statement", uuid.New(), nil, m{"project_id": otherProject, "type": "note", "title": "Other's",
		"body": "other words"})
	s.emit("consumer.cursor_set", "consumer", uuid.New(), nil, m{"consumer": "vault-harvest", "seq": 3})
	s.actor, s.run = me, myRun

	// purge: the admin command calls journal_purge, then projects the journal.purged event it appended.
	err := pg.InTx(ctx, admin, func(tx pgx.Tx) error {
		var seq int64
		if err := tx.QueryRow(ctx, `SELECT journal_purge($1,'statement',$2,'gdpr','art.17',$3,$4,$4,$4)`,
			tenant, stD, s.actor, s.run).Scan(&seq); err != nil {
			return err
		}
		events, err := journal.Read(ctx, tx, tenant, seq-1, 1)
		if err != nil || len(events) != 1 {
			return fmt.Errorf("read purge event: %v (%d)", err, len(events))
		}
		return projector.Apply(ctx, tx, &events[0])
	})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	s.types["journal.purged"]++
	err = pg.InTx(ctx, admin, func(tx pgx.Tx) error {
		var seq int64
		if err := tx.QueryRow(ctx, `SELECT journal_purge($1,'task',$2,'secret','',$3,$4,$4,$4)`,
			tenant, tp, s.actor, s.run).Scan(&seq); err != nil {
			return err
		}
		events, err := journal.Read(ctx, tx, tenant, seq-1, 1)
		if err != nil || len(events) != 1 {
			return fmt.Errorf("read purge event: %v (%d)", err, len(events))
		}
		return projector.Apply(ctx, tx, &events[0])
	})
	if err != nil {
		t.Fatalf("purge task: %v", err)
	}
	err = pg.InTx(ctx, admin, func(tx pgx.Tx) error {
		var seq int64
		if err := tx.QueryRow(ctx, `SELECT journal_purge($1,'statement',$2,'secret','',$3,$4,$4,$4)`,
			tenant, stZ, s.actor, s.run).Scan(&seq); err != nil {
			return err
		}
		events, err := journal.Read(ctx, tx, tenant, seq-1, 1)
		if err != nil || len(events) != 1 {
			return fmt.Errorf("read purge event: %v (%d)", err, len(events))
		}
		return projector.Apply(ctx, tx, &events[0])
	})
	if err != nil {
		t.Fatalf("purge quarantined: %v", err)
	}

	// an outcome of the task: a statement with its structure and an about link, and next steps
	stO := uuid.New()
	s.emit("statement.asserted", "statement", stO, &cx, m{"project_id": project, "type": "outcome", "title": "T reviewed",
		"body": "state: partial\nnext: merge\nsubject: T-1", "outcome": m{"ref": "T-1", "subject_type": "task", "subject_id": task,
			"state": "partial", "next": "merge", "evidence": []string{"PR 7"}}})
	s.emit("link.asserted", "link", uuid.New(), nil, m{"from": ref("statement", stO), "to": ref("task", task), "relation": "about"})
	s.emit("context.closed", "context", cx, nil, m{"summary": "E7 done", "auto": false, "next_steps": []string{"release"}})
	s.emit("run.finished", "run", s.run, nil, m{"outcome": "ok"})
	s.emit("actor.registered", "actor", agent, nil, m{"kind": "agent", "display_name": "sub", "on_behalf_of": s.actor})
	s.emit("actor.amended", "actor", agent, nil, m{"display_name": "sub-local", "executor": m{"provider": "ollama", "model": "qwen3",
		"key_kind": "local", "max_data_class": "local"}})
	s.emit("actor.revoked", "actor", agent, nil, m{"reason": "done"})

	for typ := range s.types {
		if !projector.Known(typ) {
			t.Errorf("scenario used unknown type %s", typ)
		}
	}
	if missing := missingTypes(s.types); len(missing) > 0 {
		t.Fatalf("scenario does not cover slice-1 types: %v", missing)
	}

	live := dump(t, app)
	for table, rows := range live { // a comparison of empty tables would prove nothing
		if len(rows) == 0 {
			t.Errorf("E7: live %s is empty — the scenario does not exercise it", table)
		}
	}
	assertPurgedClean(t, app, stD)
	assertTaskPurged(t, app, tp)
	assertQuarantined(t, app, stQ, stB, tq)
	assertPurgedClean(t, app, stZ)
	n, err := projector.Rebuild(ctx, app, tenant)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuilt := dump(t, app)
	for table, rows := range live {
		if !reflect.DeepEqual(rows, rebuilt[table]) {
			t.Errorf("E7: %s differs after rebuild\n live:    %v\n rebuilt: %v", table, rows, rebuilt[table])
		}
	}
	t.Logf("E7: %d events replayed, %d tables compared, %d event types", n, len(live), len(s.types))

	// the checks of `litopys verify` live in the database (ops.verify_report, 0018): a sound replay has
	// no findings; a stale projector version is one — and psql without a version does not check it
	findings := func(version any) []string {
		var all []string
		rows, err := app.Query(ctx, `SELECT unnest(findings) FROM ops.verify_report($1)`, version)
		if err != nil {
			t.Fatalf("verify_report: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var f string
			if err := rows.Scan(&f); err != nil {
				t.Fatal(err)
			}
			all = append(all, f)
		}
		return all
	}
	if f := findings(projector.Version); len(f) != 0 {
		t.Fatalf("verify_report after the replay: %v", f)
	}
	if f := findings("0"); len(f) != 1 || f[0] != "projections were built by another projector version — litopys projector rebuild" {
		t.Fatalf("verify_report with another version: %v", f)
	}
	if f := findings(nil); len(f) != 0 {
		t.Fatalf("verify_report without a version: %v", f)
	}

	// only a finished rebuild says the projections match this code. A live write of a new binary over
	// projections of another version leaves them stale; so does a rebuild that did not reach its last batch.
	setVersion := func(v string) {
		t.Helper()
		if err := pg.InTx(ctx, app, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('litopys.scope', 'projector', true)`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE projection.projector_state SET version = $2 WHERE tenant_id = $1`, tenant, v)
			return err
		}); err != nil {
			t.Fatalf("set version: %v", err)
		}
	}
	stale := []string{"projections were built by another projector version — litopys projector rebuild"}
	setVersion("17")
	s.emit("statement.asserted", "statement", uuid.New(), nil, m{"project_id": project, "type": "note", "title": "after an upgrade"})
	if f := findings(projector.Version); !reflect.DeepEqual(f, stale) {
		t.Errorf("a live write marked projections of version 17 current: %v", f)
	}
	setVersion(projector.Rebuilding)
	if f := findings(projector.Version); !reflect.DeepEqual(f, stale) {
		t.Errorf("an unfinished rebuild looks finished: %v", f)
	}
	if _, err := projector.Rebuild(ctx, app, tenant); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if f := findings(projector.Version); len(f) != 0 {
		t.Errorf("a finished rebuild: %v", f)
	}
}

var sliceOne = []string{"actor.registered", "actor.amended", "actor.revoked", "run.started", "run.finished", "context.opened",
	"context.closed", "context.checkpointed", "statement.asserted", "statement.amended", "statement.retracted",
	"statement.reinstated", "statement.superseded", "link.asserted", "link.retired", "project.declared",
	"project.amended", "project.transitioned", "task.declared", "task.amended", "task.closed", "task.reopened", "task.retracted",
	"fact.recorded", "fact.retired", "consumer.cursor_set", "journal.purged", "journal.quarantined", "journal.released", "policy.set",
	"deletion.proposed", "deletion.approved", "deletion.rejected", "deletion.executed"}

func missingTypes(used map[string]int) []string {
	var out []string
	for _, typ := range sliceOne {
		if used[typ] == 0 {
			out = append(out, typ)
		}
	}
	return out
}

// dump returns every derived table as ordered JSON rows; projector_state without rebuilt_at.
func dump(t *testing.T, q *pgxpool.Pool) map[string][]string {
	t.Helper()
	tables := []string{"actor", "run", "context", "project", "task", "fact", "statement", "statement_version",
		"statement_lexical", "statement_search", "link", "consumer", "search_vocab", "stem_vocab", "policy", "deletion"}
	out := map[string][]string{}
	for _, tbl := range tables {
		out[tbl] = rows(t, q, `SELECT row_to_json(t)::text FROM projection.`+tbl+` t ORDER BY 1`)
	}
	out["projector_state"] = rows(t, q, `SELECT row_to_json(t)::text FROM
		(SELECT tenant_id, last_seq, version FROM projection.projector_state) t ORDER BY 1`)
	return out
}

func rows(t *testing.T, q *pgxpool.Pool, sql string) []string {
	t.Helper()
	r, err := q.Query(pg.WithScope(context.Background(), pg.ScopeSystem), sql)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(r, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assertPurgedClean: nothing of the purged statement's content survives in the projections.
func assertPurgedClean(t *testing.T, q *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	var leaks int
	err := q.QueryRow(pg.WithScope(context.Background(), pg.ScopeSystem), `
		SELECT (SELECT count(*) FROM projection.statement WHERE statement_id = $1
		          AND (NOT purged OR body IS NOT NULL OR external_ref IS NOT NULL OR concepts <> '{}' OR title <> '[purged]'))
		     + (SELECT count(*) FROM projection.statement_version WHERE statement_id = $1
		          AND (body IS NOT NULL OR reason IS NOT NULL OR title <> '[purged]'))
		     + (SELECT count(*) FROM projection.statement_lexical WHERE statement_id = $1)
		     + (SELECT count(*) FROM projection.statement_search WHERE statement_id = $1)
		     + (SELECT count(*) FROM projection.search_vocab WHERE word IN ('pii', 'personal'))
		     + (SELECT count(*) FROM projection.stem_vocab v WHERE NOT EXISTS (SELECT 1 FROM projection.statement_search x
		          WHERE x.tenant_id = v.tenant_id AND x.owner_id = v.owner_id AND v.stem = ANY(tsvector_to_array(x.stems))))`, id).Scan(&leaks)
	if err != nil {
		t.Fatal(err)
	}
	if leaks != 0 {
		t.Errorf("purged statement leaks content in %d projection rows", leaks)
	}
}

// assertTaskPurged: the purged task keeps its number and structure (0033), not a word of its content.
func assertTaskPurged(t *testing.T, q *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	var got string
	err := q.QueryRow(pg.WithScope(context.Background(), pg.ScopeSystem), `
		SELECT concat_ws('|', num, title, body, kind, status, priority, rank, parent, owner, acceptance, resolution,
		       labels::text, refs::text, purged::text, hidden::text)
		  FROM projection.task WHERE task_id = $1`, id).Scan(&got)
	if err != nil {
		t.Fatal(err)
	}
	if want := "3|[purged]|risk|dropped|high|5|1|{}|{}|true|true"; got != want {
		t.Errorf("purged task row: %s, want %s", got, want)
	}
}

// assertQuarantined: the quarantined record keeps its content but nothing of it in search; the released one is back.
func assertQuarantined(t *testing.T, q *pgxpool.Pool, hidden, released, task uuid.UUID) {
	t.Helper()
	var got string
	err := q.QueryRow(pg.WithScope(context.Background(), pg.ScopeSystem), `
		SELECT concat_ws('|',
		  (SELECT quarantined::text || hidden::text || (body IS NOT NULL)::text FROM projection.statement WHERE statement_id = $1),
		  (SELECT count(*) FROM projection.statement_lexical WHERE statement_id = $1),
		  (SELECT count(*) FROM projection.statement_search WHERE statement_id = $1),
		  (SELECT count(*) FROM projection.search_vocab WHERE word IN ('quokkapass', 'quokka')),
		  (SELECT count(*) FROM projection.stem_vocab WHERE stem LIKE 'quokka%'),
		  (SELECT hidden::text FROM projection.statement WHERE statement_id = $2),
		  (SELECT count(*) FROM projection.statement_search WHERE statement_id = $2),
		  (SELECT hidden::text FROM projection.task WHERE task_id = $3))`, hidden, released, task).Scan(&got)
	if err != nil {
		t.Fatal(err)
	}
	if want := "truetruetrue|0|0|0|0|false|1|true"; got != want {
		t.Errorf("quarantine in the projections: %s, want %s", got, want)
	}
}
