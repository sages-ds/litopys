package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"litopys.smart-gw.co.ua/core/ops"
	"litopys.smart-gw.co.ua/internal/pg"
)

// runStatus: `litopys status [--config F] [--json]` — what the installation runs. Read-only.
func runStatus(ctx context.Context, args []string) error {
	f, asJSON, err := gatherFacts(ctx, "status", args)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSONOut(f)
	}
	ops.PrintStatus(os.Stdout, f)
	return nil
}

// runDoctor: `litopys doctor [--config F] [--json]` — every check with a level and an advice.
// Read-only, fixes nothing. Exit code: 0 ok, 1 warning, 2 critical — for Semaphore.
func runDoctor(ctx context.Context, args []string) error {
	f, asJSON, err := gatherFacts(ctx, "doctor", args)
	if err != nil {
		return err
	}
	rs := ops.Evaluate(f)
	if asJSON {
		if err := writeJSONOut(map[string]any{"at": f.At, "level": ops.Worst(rs), "results": rs}); err != nil {
			return err
		}
	} else {
		ops.PrintDoctor(os.Stdout, rs)
	}
	os.Exit(int(ops.Worst(rs)))
	return nil
}

// runRelease: `litopys release manifest --version V --commit C --image repo@sha256:… [--out F]` — the release
// manifest of this binary; the deploy writes it after the image push.
func runRelease(_ context.Context, args []string) error {
	if len(args) == 0 || args[0] != "manifest" {
		return fmt.Errorf("release: usage: litopys release manifest --version V --commit C --image repo@sha256:… [--out F]")
	}
	fs := flag.NewFlagSet("release manifest", flag.ContinueOnError)
	version := fs.String("version", "", "release name (the image tag)")
	commit := fs.String("commit", "", "full commit hash")
	image := fs.String("image", "", "core image as repo@sha256:<digest>")
	out := fs.String("out", "", "file to write (default: stdout)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *version == "" || *commit == "" {
		return fmt.Errorf("release manifest: --version and --commit are required")
	}
	m, err := ops.BuildManifest(*version, *commit, *image)
	if err != nil {
		return err
	}
	b, err := m.Marshal()
	if err != nil {
		return err
	}
	if *out == "" {
		_, err = os.Stdout.Write(b)
		return err
	}
	return os.WriteFile(*out, b, 0o644)
}

func gatherFacts(ctx context.Context, name string, args []string) (*ops.Facts, bool, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	config := fs.String("config", "", "install.yaml (default: LITOPYS_INSTALL_CONFIG, then "+ops.DefaultConfig+")")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return nil, false, err
	}
	cfg, err := ops.LoadConfig(*config)
	if err != nil {
		return nil, false, err
	}
	target := ops.TrueNAS{C: ops.SSHMidclt{Host: cfg.TrueNAS.SSH, Key: cfg.TrueNAS.SSHKey}}
	ctx, cancel := context.WithTimeout(pg.WithScope(ctx, pg.ScopeSystem), 2*time.Minute)
	defer cancel()
	openDB := func(ctx context.Context) (ops.TxBeginner, func(), error) {
		db, err := pg.Pool(ctx)
		if err != nil {
			return nil, nil, err
		}
		return db, db.Close, nil
	}
	return ops.Gather(ctx, cfg, target, openDB), *asJSON, nil
}

func writeJSONOut(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
