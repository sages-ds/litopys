// Package pg holds the PostgreSQL plumbing shared by core and enterprise.
//
// Connection settings come only from the environment: LITOPYS_DATABASE_URL if set,
// otherwise the libpq variables (PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE).
// Secrets never travel through flags or files.
package pg

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DSN returns the connection string taken from the environment ("" means libpq variables).
func DSN() string { return os.Getenv("LITOPYS_DATABASE_URL") }

// Connect opens a single connection (CLI commands such as migrate).
func Connect(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, DSN())
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return conn, nil
}

// Pool opens a connection pool (long-running processes: api, worker, facade, import).
// LITOPYS_SET_ROLE (e.g. litopys_app) makes every connection SET ROLE after login — the
// application roles are NOLOGIN, a login role that is a member switches into them.
func Pool(ctx context.Context) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(DSN())
	if err != nil {
		return nil, fmt.Errorf("pool config: %w", err)
	}
	if role := os.Getenv("LITOPYS_SET_ROLE"); role != "" {
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
			return err
		}
	}
	cfg.PrepareConn = PrepareConn
	// a slow query must not hold a pool connection forever (LITOPYS_STATEMENT_TIMEOUT,
	// a PostgreSQL duration; 0 disables). Maintenance runs (rebuild, import) use it too: their
	// statements are batches, not one long query.
	timeout := os.Getenv("LITOPYS_STATEMENT_TIMEOUT")
	if timeout == "" {
		timeout = "30s"
	}
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = timeout
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	return pool, nil
}

// Beginner is satisfied by *pgx.Conn, *pgxpool.Pool and pgx.Tx.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// InTx runs fn in one transaction: commit on nil, rollback on error or panic.
// This is the shape of every write verb: journal_append -> projector.Apply -> commit.
func InTx(ctx context.Context, db Beginner, fn func(pgx.Tx) error) (err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
