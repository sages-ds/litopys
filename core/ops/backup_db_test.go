package ops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/journal"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestGateOpsBackup: a dump as a login under row-level security (litopys_app) is refused before
// anything is written; a dump of a sound database restores into a scratch copy that passes verify and is not behind
// the heads read before the dump; a dump of a database the projector has not caught up with fails its check and
// prunes nothing; restore puts a dump into a new database beside the live one, never over it. The dump steps need
// pg_dump / pg_restore of the server's major version (CI installs them; skipped where they are missing).
func TestGateOpsBackup(t *testing.T) {
	ctx := pg.WithScope(context.Background(), pg.ScopeSystem)
	db := pgtest.New(t)
	mig := db.MigratorConn(t)
	if _, err := migrate.Up(ctx, mig); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 2)
	var tenant uuid.UUID
	if err := mig.QueryRow(ctx, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	// a sound database: a record through the real verbs (projected in the same transaction)
	s := &verbs.Service{DB: app}
	ada, _, err := s.RegisterHuman(context.Background(), tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	if _, _, err := s.ProjectDeclare(context.Background(), me, verbs.ProjectDeclareIn{Name: "backup-gate"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StatementAssert(context.Background(), me, verbs.StatementAssertIn{Project: "backup-gate", Type: "note", Title: "backup gate"}); err != nil {
		t.Fatal(err)
	}
	// the journal alone, without the projector: a lagging projection (a finding of verify_report)
	appendEvent := func() {
		run := uuid.New()
		if err := pg.InTx(ctx, app, func(tx pgx.Tx) error {
			return journal.Append(ctx, tx, &journal.Event{TenantID: tenant, Type: "statement.asserted",
				ActorID: run, RunID: run, CausationID: run, CorrelationID: run,
				EntityType: "statement", EntityID: uuid.New(), Payload: json.RawMessage(`{"title":"backup gate","type":"note"}`)})
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 1. a login that RLS applies to: refused, nothing written
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	login, pw := "litopys_test_rls_"+hex.EncodeToString(b), hex.EncodeToString(b)+"x"
	su := db.SuConn(t, "postgres")
	if _, err := su.Exec(ctx, `CREATE ROLE `+login+` LOGIN PASSWORD '`+pw+`' IN ROLE litopys_app`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = su.Exec(context.Background(), `DROP ROLE IF EXISTS `+login) })
	dir := t.TempDir()
	rls := &Backuper{DSN: db.DSNAs(login, pw), Dir: dir, KeepDays: 14, Tools: ExecTools{}}
	if _, err := rls.Run(ctx); !errors.Is(err, ErrRLSLogin) {
		t.Fatalf("a dump as a member of litopys_app: %v, want ErrRLSLogin", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("a refused backup left %d file(s)", len(left))
	}

	var head int64
	if err := mig.QueryRow(ctx, `SELECT last_seq FROM journal_head JOIN tenant USING (tenant_id) WHERE name = 'default'`).Scan(&head); err != nil || head == 0 {
		t.Fatalf("journal head %d %v", head, err)
	}
	var found []string
	if err := mig.QueryRow(ctx, `SELECT findings FROM ops.verify_report(NULL) WHERE tenant = 'default'`).Scan(&found); err != nil || len(found) != 0 {
		t.Fatalf("the database to back up is not sound: %v %v", found, err)
	}
	if _, err := exec.LookPath("pg_dump"); err != nil {
		// CI sets LITOPYS_TEST_REQUIRE_PG_TOOLS: there a missing client is a failure, not a silent skip
		if os.Getenv("LITOPYS_TEST_REQUIRE_PG_TOOLS") != "" {
			t.Fatal("pg_dump not in PATH, and this run requires the dump and restore steps")
		}
		t.Skip("pg_dump not in PATH: the dump and restore steps run in CI")
	}

	// 2. a sound database: dump, restore check passes, the record says so
	bk := &Backuper{DSN: db.SuDSN(), Dir: dir, KeepDays: 14, Tools: ExecTools{}, Log: t.Logf}
	rec, err := bk.Run(ctx)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if !rec.OK || rec.LiveHeads["default"] != head || len(rec.Copy) == 0 || rec.VerifiedAt == nil {
		t.Fatalf("record %+v", rec)
	}
	dump := filepath.Join(dir, rec.Dump)
	if got := newestBackup(dir); !got.Checked || !got.Verified {
		t.Fatalf("doctor's view %+v", got)
	}

	// 3. --verify of that dump later: still sound (the dump holds the caught-up state)
	appendEvent() // the live projector now lags
	if v, err := bk.Verify(ctx, dump); err != nil || !v.OK {
		t.Fatalf("verify of an existing dump: %+v %v", v, err)
	}

	// 4. a dump of a database the projector lags on: the check fails, the record says why
	rec2, err := bk.Run(ctx)
	if err == nil || rec2 == nil || rec2.OK || len(rec2.Findings) == 0 {
		t.Fatalf("a lagging database must fail its restore check: %+v %v", rec2, err)
	}
	if got := newestBackup(dir); !got.Checked || got.Verified {
		t.Fatalf("doctor must see the failed check: %+v", got)
	}

	// 5. restore beside, never over the live database
	if _, _, err := Restore(ctx, db.SuDSN(), ExecTools{}, dump, db.Name, ""); err == nil {
		t.Fatal("a restore over the live database must be refused")
	}
	into := db.Name + "_restored"
	t.Cleanup(func() { dropDB(context.Background(), db.SuDSN(), into) })
	cp, found, err := Restore(ctx, db.SuDSN(), ExecTools{}, dump, into, pgtest.Migrator)
	if err != nil || len(found) != 0 || len(cp) == 0 {
		t.Fatalf("restore: %+v %v %v", cp, found, err)
	}
	var owner string
	if err := su.QueryRow(ctx, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1`, into).Scan(&owner); err != nil || owner != pgtest.Migrator {
		t.Fatalf("restored database owner %q %v", owner, err)
	}
}
