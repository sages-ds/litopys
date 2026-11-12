package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"runtime/debug"

	"litopys.smart-gw.co.ua/core/runner"
)

// runRunner: litopys runner --config <file> [--once]. The orchestrator token comes from LITOPYS_TOKEN, provider
// keys from the variables the configuration names (BYOK, secrets only through the environment).
func runRunner(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("runner", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/litopys/runner.json", "configuration file (JSON, no secrets)")
	once := fs.Bool("once", false, "poll each executor once and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := runner.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	token := os.Getenv("LITOPYS_TOKEN")
	if token == "" {
		return errors.New("LITOPYS_TOKEN is not set (the orchestrator token that delegates steps)")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	r := runner.New(cfg, token, log)
	// with the runner secret the executors and their keys come from the server, and the runner reports its state for
	// the status page; without it — the file, as before
	r.Secret = os.Getenv("LITOPYS_RUNNER_SECRET")
	if cfg.ServerExecutors && r.Secret == "" {
		return errors.New("server_executors needs LITOPYS_RUNNER_SECRET")
	}
	r.Name, _ = os.Hostname()
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, st := range bi.Settings {
			if st.Key == "vcs.revision" && len(st.Value) >= 12 {
				r.Version = st.Value[:12]
			}
		}
	}
	if *once {
		list := cfg.Executors
		if r.Secret != "" {
			if list, err = r.ServerExecutors(ctx); err != nil {
				return err
			}
			list = append(cfg.Executors, list...)
		}
		for _, e := range list {
			n, err := r.Once(ctx, e)
			if err != nil {
				return err
			}
			log.Info("poll done", "executor", e.Name, "finished", n)
		}
		return nil
	}
	log.Info("runner started", "executors", len(cfg.Executors), "from_server", r.Secret != "", "poll_s", cfg.PollS)
	return r.Run(ctx)
}
