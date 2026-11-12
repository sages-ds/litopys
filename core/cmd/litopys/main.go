// Command litopys is the single binary of the core: serve | worker | migrate and the operations.
//
// Database settings come from the environment only (see internal/pg).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/internal/pg"
)

const usage = `usage: litopys <command>

  api       HTTP/MCP API                        (not implemented yet)
  worker    [--interval 5s] [--once] [--session-idle 24h]  executes approved purges of deletion proposals (admin
            login that may SET ROLE litopys_admin, as purge), applies retention labels daily, closes idle sessions
  serve     the HTTP server on LITOPYS_ADDR (default :8766; old names: command facade, LITOPYS_FACADE_ADDR)
  migrate   up | status | baseline | list
  auth      bootstrap --email E --name N   first admin token (printed ONCE; run it yourself)
  projector rebuild  replay the journal into freshly truncated projections (after projection migrations)
  verify    integrity of a database or a restored backup (gapless journal, projector caught up)
  purge     --type T --id ID --reason R --email E [--legal-basis B] [--yes]  journal.purge by a person (admin
            login that may SET ROLE litopys_admin); without --yes only a preview
  runner    --config <file> [--once]  execute ready model steps with registered executors (native API client;
            orchestrator token: LITOPYS_TOKEN; provider keys: the variables the config names)
  migrate-from vault-curated-v1 --dir <vault> --owner <e-mail> [--dry-run] [--report <file>]
            the curated zone of an Obsidian vault as documents of the owner (target role: LITOPYS_SET_ROLE=litopys_app);
            a migration from another provider is a separate tool

Operations (on the host of an installation; install.yaml: --config, LITOPYS_INSTALL_CONFIG or
/etc/litopys/install.yaml; database as above with LITOPYS_SET_ROLE=litopys_app):
  status    [--json]  release, services and their digests, schema, projector, backup age, certificate
  doctor    [--json]  every check with a level and an advice; exit 0 ok, 1 warning, 2 critical
  maintenance status | on [--reason R] [--drain D] | off   read-only mode for an update (writes refuse, reads go on)
  update    --release F [--dry-run] [--json]   drain, rehearse on a copy, snapshot, upgrade, health; rolls back on failure
  rollback  --release F [--snapshot ID] [--plan] [--accept-loss]   back to an update snapshot and an earlier release
  backup    [--verify DUMP] [--json]   dump as LITOPYS_BACKUP_URL (bypasses RLS), restore into a scratch copy, verify,
            record beside the dump, prune after backup.keep_days; pg_dump/pg_restore from PATH or LITOPYS_PG_BIN
  restore   DUMP --into DB [--owner R]   into a new database beside the live one and verify (LITOPYS_OPERATOR_URL)
  release   manifest --version V --commit C --image repo@sha256:… [--out F]  the release manifest of this binary

Database: LITOPYS_DATABASE_URL, or PGHOST/PGPORT/PGUSER/PGPASSWORD/PGDATABASE.`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM) // SIGTERM: a scheduler stopping a run lets its cleanup go
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "litopys:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "api":
		return fmt.Errorf("%s: not implemented yet", args[0])
	case "worker":
		return runWorker(ctx, args[1:])
	case "serve", "facade": // facade: the old name, installed apps still run it
		return runServe(ctx, args[1:])
	case "migrate":
		return runMigrate(ctx, args[1:])
	case "migrate-from":
		return runMigrateFrom(ctx, args[1:])
	case "projector":
		return runProjector(ctx, args[1:])
	case "auth":
		return runAuth(ctx, args[1:])
	case "verify":
		return runVerify(ctx, args[1:])
	case "purge":
		return runPurge(ctx, args[1:])
	case "runner":
		return runRunner(ctx, args[1:])
	case "status":
		return runStatus(ctx, args[1:])
	case "doctor":
		return runDoctor(ctx, args[1:])
	case "release":
		return runRelease(ctx, args[1:])
	case "maintenance":
		return runMaintenance(ctx, args[1:])
	case "update":
		return runUpdate(ctx, args[1:])
	case "rollback":
		return runRollback(ctx, args[1:])
	case "backup":
		return runBackup(ctx, args[1:])
	case "restore":
		return runRestore(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Println(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

func runMigrate(ctx context.Context, args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "list" { // no database needed
		all, err := migrate.List()
		if err != nil {
			return err
		}
		for _, m := range all {
			fmt.Printf("%s  %s\n", m.Name, m.Recorded()[:12])
		}
		return nil
	}
	conn, err := pg.Connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	var lines []string
	switch sub {
	case "status":
		lines, err = migrate.Status(ctx, conn)
	case "up":
		lines, err = migrate.Up(ctx, conn)
		if len(lines) == 0 && err == nil {
			lines = []string{"nothing to apply"}
		}
	case "baseline":
		lines, err = migrate.Baseline(ctx, conn)
	default:
		return fmt.Errorf("migrate: unknown subcommand %q (up | status | baseline | list)", sub)
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return err
}
