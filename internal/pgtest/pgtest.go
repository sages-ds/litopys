// Package pgtest gives gate tests a throw-away database on the stand (the
// journal cannot be truncated, so every run gets a fresh database and drops it).
//
// Environment (tests are skipped when any is missing):
//
//	LITOPYS_TEST_PGHOST, LITOPYS_TEST_PGPORT            stand address (host, port)
//	LITOPYS_TEST_SU_USER, LITOPYS_TEST_SU_PASSWORD      superuser of the stand: CREATE/DROP DATABASE, SET ROLE
//	LITOPYS_TEST_MIGRATOR_PASSWORD                      litopys_migrator: owner of the test database, runs migrations
//
// Values come from OpenBao via bao.ps1 run -Map; nothing is written to disk.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"litopys.smart-gw.co.ua/internal/pg"
)

// Migrator is the role that owns test databases and applies migrations.
const Migrator = "litopys_migrator"

// Locale is how every Litopys database is created: libc C.UTF-8, so text ordering is by code point and does not depend on the
// cluster default (the stand's template1 is en_US.utf8).
const Locale = `TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER libc LOCALE 'C.UTF-8'`

// DB is one throw-away database.
type DB struct {
	Name string
	host string
	port string
	su   string
	suPw string
	mgPw string
}

func env(t testing.TB) map[string]string {
	t.Helper()
	keys := []string{"LITOPYS_TEST_PGHOST", "LITOPYS_TEST_PGPORT", "LITOPYS_TEST_SU_USER",
		"LITOPYS_TEST_SU_PASSWORD", "LITOPYS_TEST_MIGRATOR_PASSWORD"}
	out := map[string]string{}
	for _, k := range keys {
		v := os.Getenv(k)
		if v == "" {
			t.Skipf("gate test needs the stand: %s is not set", k)
		}
		out[k] = v
	}
	return out
}

// New creates litopys_test_<random> owned by litopys_migrator and drops it when the test ends.
func New(t testing.TB) *DB {
	t.Helper()
	e := env(t)
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	d := &DB{
		Name: "litopys_test_" + hex.EncodeToString(b),
		host: e["LITOPYS_TEST_PGHOST"], port: e["LITOPYS_TEST_PGPORT"],
		su: e["LITOPYS_TEST_SU_USER"], suPw: e["LITOPYS_TEST_SU_PASSWORD"], mgPw: e["LITOPYS_TEST_MIGRATOR_PASSWORD"],
	}
	ctx := context.Background()
	admin := d.conn(t, "postgres", d.su, d.suPw)
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s OWNER %s %s`, d.Name, Migrator, Locale)); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c := d.conn(t, "postgres", d.su, d.suPw)
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, d.Name)); err != nil {
			t.Errorf("drop test database %s: %v", d.Name, err)
		}
	})
	return d
}

func (d *DB) config(t testing.TB, dbname, user, password string) *pgx.ConnConfig {
	t.Helper()
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%s dbname=%s sslmode=disable connect_timeout=10", d.host, d.port, dbname))
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password = user, password
	return cfg
}

func (d *DB) conn(t testing.TB, dbname, user, password string) *pgx.Conn {
	t.Helper()
	c, err := pgx.ConnectConfig(context.Background(), d.config(t, dbname, user, password))
	if err != nil {
		t.Fatalf("connect %s@%s: %v", user, dbname, err)
	}
	return c
}

// SuConn connects as the stand superuser to another database of the stand (e.g. a restored
// source for the importer test: LITOPYS_TEST_SOURCE_DB). Read-only use is the caller's promise.
func (d *DB) SuConn(t testing.TB, dbname string) *pgx.Conn {
	t.Helper()
	c := d.conn(t, dbname, d.su, d.suPw)
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// SuDSN and MigratorDSN are connection strings of the test database for code that opens its own connections
// (the update of core/ops). Test-only: the credentials come from the stand's environment.
func (d *DB) SuDSN() string { return d.dsn(d.su, d.suPw) }

func (d *DB) MigratorDSN() string { return d.dsn(Migrator, d.mgPw) }

// DSNAs is the connection string of the test database for another login (a role the test created).
func (d *DB) DSNAs(user, password string) string { return d.dsn(user, password) }

func (d *DB) dsn(user, password string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: net.JoinHostPort(d.host, d.port), Path: "/" + d.Name,
		RawQuery: "sslmode=disable&connect_timeout=10"}
	return u.String()
}

// MigratorConn connects to the test database as litopys_migrator (owner; runs migrations).
func (d *DB) MigratorConn(t testing.TB) *pgx.Conn {
	t.Helper()
	c := d.conn(t, d.Name, Migrator, d.mgPw)
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// RolePool is a pool on the test database whose every connection runs as role (SET ROLE).
// litopys_app and litopys_admin are NOLOGIN, so the superuser logs in and switches.
func (d *DB) RolePool(t testing.TB, role string, maxConns int32) *pgxpool.Pool {
	t.Helper()
	pcfg, err := pgxpool.ParseConfig("")
	if err != nil {
		t.Fatal(err)
	}
	pcfg.ConnConfig = d.config(t, d.Name, d.su, d.suPw)
	pcfg.MaxConns = maxConns
	pcfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	pcfg.PrepareConn = pg.PrepareConn // litopys.owner / litopys.scope from the context (RLS of 0008)
	p, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}
