package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/ops"
)

func pgxConnect(ctx context.Context, dsn string) (*pgx.Conn, error) { return pgx.Connect(ctx, dsn) }

// runUpdate: `litopys update --release <manifest> [--config F] [--dry-run] [--json]` — read-only
// and drain, stop the core, rehearse the migrations and the rebuild on a copy, snapshot the database's dataset, upgrade
// the live database, start the core on the release's image, check its health; a failure before the snapshot leaves
// the data untouched, after it rolls back to the snapshot and the old compose. Secrets only from the environment:
// LITOPYS_OPERATOR_URL (the role that copies databases and switches read-only), LITOPYS_MIGRATOR_URL, and the
// compose's LITOPYS_MIGRATOR_PASSWORD / LITOPYS_APP_PASSWORD / LITOPYS_WORKER_PASSWORD / LITOPYS_DIRECTORY_SECRET.
// --dry-run checks everything an update needs and changes nothing.
func runUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	config := fs.String("config", "", "install.yaml (default: LITOPYS_INSTALL_CONFIG, then "+ops.DefaultConfig+")")
	release := fs.String("release", "", "the release manifest (litopys-release.yaml)")
	dry := fs.Bool("dry-run", false, "check what an update needs, change nothing")
	asJSON := fs.Bool("json", false, "machine-readable result")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *release == "" {
		return errors.New("update: --release <manifest> is required")
	}
	cfg, err := ops.LoadConfig(*config)
	if err != nil {
		return err
	}
	m, err := ops.LoadManifest(*release)
	if err != nil {
		return err
	}
	image := m.Images["core"]
	if m.Commit == "" || image == "" {
		return errors.New("update: the manifest names no commit or no core image")
	}
	if cfg.DBDataset == "" || cfg.CoreCompose == "" || cfg.Services.Core == "" || cfg.Services.DB == "" || cfg.Endpoint == "" {
		return errors.New("update: install config needs services.core, services.db, endpoint, db_dataset and core_compose")
	}
	tpl, err := os.ReadFile(cfg.CoreCompose)
	if err != nil {
		return fmt.Errorf("update: core compose: %w", err)
	}
	compose, err := ops.RenderCompose(string(tpl), image, nil)
	if err != nil {
		return err
	}
	operator, migrator := os.Getenv("LITOPYS_OPERATOR_URL"), os.Getenv("LITOPYS_MIGRATOR_URL")
	if operator == "" || migrator == "" {
		return errors.New("update: LITOPYS_OPERATOR_URL and LITOPYS_MIGRATOR_URL are required")
	}
	logf := func(format string, a ...any) { log.Printf("update: "+format, a...) }
	nas := ops.TrueNAS{C: ops.SSHMidclt{Host: cfg.TrueNAS.SSH, Key: cfg.TrueNAS.SSHKey}}
	db := ops.LiveDatabase{Operator: operator, Migrator: migrator, AppRole: "litopys_app", Accepted: cfg.AcceptedFindings, Log: logf}
	health := ops.HTTPHealth{Endpoint: cfg.Endpoint}
	if *dry {
		return updateDryRun(ctx, cfg, m, nas, db, health)
	}
	u := &ops.Updater{P: nas, D: db, H: health, Core: cfg.Services.Core, DB: cfg.Services.DB, Dataset: cfg.DBDataset,
		Commit: m.Commit, Compose: compose, Log: logf}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	res, err := u.Run(ctx)
	if *asJSON {
		_ = writeJSONOut(res)
	} else {
		fmt.Printf("update %s: %s\n", m.Commit, res.Outcome)
	}
	return err
}

// updateDryRun: what an update needs, read only — the core answers, the apps exist, the dataset has its snapshots
// readable, both database roles connect, the release differs from what runs.
func updateDryRun(ctx context.Context, cfg *ops.Config, m *ops.Manifest, nas ops.TrueNAS, db ops.LiveDatabase, h ops.HTTPHealth) error {
	running := h.Commit(ctx)
	fmt.Printf("core: %s runs %q; release %s\n", cfg.Services.Core, running, m.Commit)
	apps, err := nas.Apps(ctx, []string{cfg.Services.Core, cfg.Services.DB})
	if err != nil {
		return err
	}
	for _, n := range []string{cfg.Services.Core, cfg.Services.DB} {
		a, ok := apps[n]
		if !ok {
			return fmt.Errorf("app %s not found", n)
		}
		fmt.Printf("app %s: %s\n", n, a.State)
	}
	snaps, err := nas.Snapshots(ctx, cfg.DBDataset)
	if err != nil {
		return fmt.Errorf("snapshots of %s: %w", cfg.DBDataset, err)
	}
	fmt.Printf("dataset %s: %d update snapshot(s)\n", cfg.DBDataset, len(snaps))
	for _, dsn := range []struct{ name, url string }{{"operator", db.Operator}, {"migrator", db.Migrator}} {
		c, err := pgxConnect(ctx, dsn.url)
		if err != nil {
			return fmt.Errorf("%s: %w", dsn.name, err)
		}
		c.Close(ctx)
		fmt.Printf("database: %s connects\n", dsn.name)
	}
	if running == m.Commit {
		fmt.Println("unchanged: the release already runs (an update would only check its health)")
	}
	fmt.Println("dry run: nothing changed")
	return nil
}
