package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"litopys.smart-gw.co.ua/core/ops"
)

// runRollback: `litopys rollback --release <manifest of the release to run again> [--snapshot <id>] [--plan]
// [--accept-loss] [--config F]`. Back to the data of an update snapshot (the newest one by
// default) and to the given release. It prints what is lost first — the journal's events recorded after the snapshot
// and the dataset's snapshots taken after it — and refuses to lose events without --accept-loss; --plan only prints.
// Only a person runs it. The environment is the one of `litopys update`.
func runRollback(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	config := fs.String("config", "", "install.yaml")
	release := fs.String("release", "", "the manifest of the release to run again")
	snapshot := fs.String("snapshot", "", "the update snapshot to return to (default: the newest)")
	plan := fs.Bool("plan", false, "print what the rollback would do and lose, change nothing")
	accept := fs.Bool("accept-loss", false, "lose the events recorded after the snapshot")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *release == "" {
		return errors.New("rollback: --release <manifest> is required (the release to run again)")
	}
	cfg, err := ops.LoadConfig(*config)
	if err != nil {
		return err
	}
	m, err := ops.LoadManifest(*release)
	if err != nil {
		return err
	}
	tpl, err := os.ReadFile(cfg.CoreCompose)
	if err != nil {
		return fmt.Errorf("rollback: core compose: %w", err)
	}
	compose, err := ops.RenderCompose(string(tpl), m.Images["core"], nil)
	if err != nil {
		return err
	}
	operator, migrator := os.Getenv("LITOPYS_OPERATOR_URL"), os.Getenv("LITOPYS_MIGRATOR_URL")
	if operator == "" || migrator == "" {
		return errors.New("rollback: LITOPYS_OPERATOR_URL and LITOPYS_MIGRATOR_URL are required")
	}
	logf := func(format string, a ...any) { log.Printf("rollback: "+format, a...) }
	r := &ops.Rollbacker{
		P:    ops.TrueNAS{C: ops.SSHMidclt{Host: cfg.TrueNAS.SSH, Key: cfg.TrueNAS.SSHKey}},
		D:    ops.LiveDatabase{Operator: operator, Migrator: migrator, AppRole: "litopys_app", Accepted: cfg.AcceptedFindings, Log: logf},
		H:    ops.HTTPHealth{Endpoint: cfg.Endpoint},
		Core: cfg.Services.Core, DB: cfg.Services.DB, Dataset: cfg.DBDataset, Snapshot: *snapshot,
		Commit: m.Commit, Head: ops.MigrationNumber(m.Schema.MigrationHead), Compose: compose, Accept: *accept, Log: logf,
	}
	p, err := r.Plan(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("snapshot %s (taken %s)\n", p.Snapshot, p.TakenAt.UTC().Format(time.RFC3339))
	fmt.Printf("release to run: %s (running now: %s)\n", p.ReleaseCommit, p.RunningCommit)
	if p.SnapshotFrom != "" {
		fmt.Printf("the snapshot holds the data of release %s\n", p.SnapshotFrom)
	}
	if p.ReadOnly != "" {
		fmt.Printf("read-only is on now: %s\n", p.ReadOnly)
	}
	fmt.Printf("lost: %d journal event(s) recorded after the snapshot\n", p.LostEvents)
	if p.LostAuth > 0 {
		fmt.Printf("lost: %d token change(s) after the snapshot — a revocation lost makes that token valid again: "+
			"revoke it once more after the rollback\n", p.LostAuth)
	}
	if len(p.LostSnapshots) > 0 {
		fmt.Printf("lost: %d later snapshot(s) of the dataset (a recursive rollback destroys them; an incremental "+
			"replication may need a full run): %s\n", len(p.LostSnapshots), strings.Join(p.LostSnapshots, ", "))
	}
	if *plan {
		fmt.Println("plan only: nothing changed")
		return nil
	}
	return r.Run(ctx, p)
}
