package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"litopys.smart-gw.co.ua/core/ops"
)

// runBackup: `litopys backup [--config F] [--verify <dump>] [--json]` — dump the live database
// into backup.dir as the backup login (LITOPYS_BACKUP_URL: LOGIN CREATEDB BYPASSRLS, pg_read_all_data; a login under
// row-level security is refused), restore the dump into a scratch database beside it, ops.verify_report there, the
// copy's heads not behind the live ones read before the dump, record the result in <dump>.json (doctor reads it),
// prune dumps older than backup.keep_days. --verify checks an existing dump the same way. pg_dump / pg_restore of the
// server's major version from PATH or LITOPYS_PG_BIN. Exit 1 when no verified backup was made.
func runBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	config := fs.String("config", "", "install.yaml (default: LITOPYS_INSTALL_CONFIG, then "+ops.DefaultConfig+")")
	verify := fs.String("verify", "", "check this dump instead of making a new one")
	asJSON := fs.Bool("json", false, "machine-readable result (the record written beside the dump)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := ops.LoadConfig(*config)
	if err != nil {
		return err
	}
	dsn := os.Getenv("LITOPYS_BACKUP_URL")
	if dsn == "" {
		return errors.New("backup: LITOPYS_BACKUP_URL (the backup login) is required")
	}
	if cfg.Backup.Dir == "" {
		return errors.New("backup: install config needs backup.dir")
	}
	b := &ops.Backuper{DSN: dsn, Dir: cfg.Backup.Dir, KeepDays: cfg.Backup.KeepDays,
		Tools: ops.ExecTools{Bin: os.Getenv("LITOPYS_PG_BIN")}, Log: func(f string, a ...any) { log.Printf("backup: "+f, a...) }}
	var rec *ops.BackupRecord
	if *verify != "" {
		rec, err = b.Verify(ctx, *verify)
	} else {
		rec, err = b.Run(ctx)
	}
	if *asJSON && rec != nil {
		_ = writeJSONOut(rec)
	}
	return err
}

// runRestore: `litopys restore <dump> --into <database> [--owner R]` — the dump into a NEW database
// beside the live one, owners and privileges as dumped, then verified. Never over the live database; switching the
// service to the restored one is a person's step. LITOPYS_OPERATOR_URL: a login that creates databases
// and restores owners.
func runRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	into := fs.String("into", "", "the new database to restore into (must not exist)")
	owner := fs.String("owner", "litopys_migrator", "owner of the new database")
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		return errors.New("restore: usage: litopys restore <dump> --into <database> [--owner R]")
	}
	file := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *into == "" {
		return errors.New("restore: --into <database> is required (a restore never goes over the live database)")
	}
	dsn := os.Getenv("LITOPYS_OPERATOR_URL")
	if dsn == "" {
		return errors.New("restore: LITOPYS_OPERATOR_URL is required")
	}
	cp, found, err := ops.Restore(ctx, dsn, ops.ExecTools{Bin: os.Getenv("LITOPYS_PG_BIN")}, file, *into, *owner)
	if err != nil {
		return err
	}
	for _, c := range cp {
		fmt.Printf("%s: tenant %s, journal head %d, projector %d (v%s)\n", *into, c.Tenant, c.JournalHead, c.ProjectorSeq, c.ProjectorVer)
	}
	for _, f := range found {
		fmt.Println("  FINDING:", f)
	}
	if len(found) > 0 {
		return fmt.Errorf("restore: %s restored with %d finding(s); it is kept for a look", *into, len(found))
	}
	fmt.Printf("restored and verified: %s (the service still runs on the live database)\n", *into)
	return nil
}
