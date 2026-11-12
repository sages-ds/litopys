package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"litopys.smart-gw.co.ua/core/api"
	"litopys.smart-gw.co.ua/core/embed"
	"litopys.smart-gw.co.ua/core/executors"
	"litopys.smart-gw.co.ua/core/files"
	"litopys.smart-gw.co.ua/core/server"
	"litopys.smart-gw.co.ua/internal/pg"
)

// runServe: `litopys serve` is the HTTP server (listenAddr).
// Database as everywhere (LITOPYS_DATABASE_URL + LITOPYS_SET_ROLE=litopys_app); journal.purge needs
// LITOPYS_ADMIN_DATABASE_URL (a login that SET ROLEs litopys_admin) — without it purge is refused.
func runServe(ctx context.Context, _ []string) error {
	db, err := pg.Pool(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	var admin *pgxpool.Pool
	if url := os.Getenv("LITOPYS_ADMIN_DATABASE_URL"); url != "" {
		cfg, err := pgxpool.ParseConfig(url)
		if err != nil {
			return err
		}
		cfg.PrepareConn = pg.PrepareConn // litopys_admin is a member of litopys_app: same RLS (0008)
		if admin, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
			return err
		}
		defer admin.Close()
	}
	var tenant uuid.UUID
	if err := db.QueryRow(ctx, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		return err
	}
	f := server.New(db, admin, tenant)
	switch v := os.Getenv("LITOPYS_SEARCH"); v { // one engine; the variable stays readable for installed apps
	case "", "v2":
	default:
		return fmt.Errorf("serve: LITOPYS_SEARCH=%q: only v2 exists (the ported v1 was removed)", v)
	}
	// the compat ranking (parity with the previous service) is gone (owner 2026-10-19)
	if r := os.Getenv("LITOPYS_FACADE_RANKING"); r != "" && r != "native" {
		return fmt.Errorf("facade: LITOPYS_FACADE_RANKING=%q: only native exists (compat was removed)", r)
	}
	ttl := 14 // retention of execution events (0009): LITOPYS_EXECUTION_TTL_DAYS
	if v := os.Getenv("LITOPYS_EXECUTION_TTL_DAYS"); v != "" {
		if ttl, err = strconv.Atoi(v); err != nil || ttl < 1 {
			return fmt.Errorf("facade: LITOPYS_EXECUTION_TTL_DAYS=%q: want a positive number of days", v)
		}
	}
	if f.OAuth, err = server.OAuthFromEnv(); err != nil {
		return err
	}
	if f.Canary, err = api.CanaryFromEnv(); err != nil { // channel canary of /v1/mcp
		return err
	}
	if f.Canary != nil {
		fmt.Fprintf(os.Stderr, "litopys serve: channel canary on for %d actor(s)\n", len(f.Canary.Actors))
	}
	// provider keys entered on the settings page are sealed with the app key; without it the app stores none
	if f.Sealer, err = executors.NewSealer(os.Getenv("LITOPYS_SECRET_KEY")); err != nil && !errors.Is(err, executors.ErrNoAppKey) {
		return fmt.Errorf("facade: %w", err)
	}
	f.RunnerSecret = os.Getenv("LITOPYS_RUNNER_SECRET")
	f.Files = files.FromEnv() // attachments: LITOPYS_FILES_DIR
	// meaning search beside the lexical one: the worker embeds the records, the facade embeds the query
	if ec, err := embed.FromEnv(); err != nil {
		return fmt.Errorf("facade: %w", err)
	} else if ec != nil {
		f.Q.Embedder = ec
		fmt.Fprintf(os.Stderr, "litopys serve: meaning search on (%s)\n", ec.Model)
	}
	h, err := f.Handler()
	if err != nil {
		return err
	}
	f.StartSweeps(ctx, ttl)
	addr := listenAddr()
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	fmt.Fprintf(os.Stderr, "litopys serve on %s\n", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// listenAddr: LITOPYS_ADDR, then the old LITOPYS_FACADE_ADDR of installed apps, then the default.
func listenAddr() string {
	for _, name := range []string{"LITOPYS_ADDR", "LITOPYS_FACADE_ADDR"} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ":8766"
}
