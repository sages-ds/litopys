// Package migrate applies the embedded SQL migrations.
//
// Model: numbered files NNNN_name.sql, table schema_migrations(version, applied_at, checksum);
// an already applied file whose checksum changed is refused. No goose/tern. The checksum skips
// comment-only lines, so comments of an applied file may be edited; a row recorded with the
// older whole-file sum (legacy, up to 0049) is accepted while the file's SQL is unchanged.
//
// Limitation: each file carries its own BEGIN/COMMIT, so the schema_migrations row is written
// right after the file commits, not inside the same transaction. A crash in between leaves the
// schema applied but unrecorded; the next Up then fails on existing objects and the fix is Baseline.
package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/db/migrations"
)

// Migration is one embedded SQL file.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string // sha256 hex of the file without comment-only lines (Sum)
}

// Accepts reports whether a checksum recorded in schema_migrations belongs to this file: the current
// sum, or the whole-file sum of the original file (legacy) while the file's SQL is still that original's —
// its comment-free sum unchanged. An edit of the SQL is refused either way.
func (m Migration) Accepts(recorded string) bool {
	if recorded == "" {
		return false
	}
	if recorded == m.Checksum {
		return true
	}
	l, ok := legacy[m.Version]
	return ok && recorded == l.raw && m.Checksum == l.stripped
}

// Recorded is the checksum Up and Baseline write for this file: the original whole-file sum for a
// migration up to 0049 whose SQL is unchanged, so an older binary still recognizes the row, else Checksum.
func (m Migration) Recorded() string {
	if l, ok := legacy[m.Version]; ok && m.Checksum == l.stripped {
		return l.raw
	}
	return m.Checksum
}

// Sum is the checksum of a migration file: sha256 hex of its lines, CRLF -> LF, without the lines that
// hold only a "--" comment. Editing such a comment keeps the sum; any other change, a comment after
// SQL on the same line included, changes it. A "--" line inside a string or a function body is left
// out of the sum too, though it still runs.
func Sum(b []byte) string {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	h := sha256.New()
	for _, line := range bytes.SplitAfter(b, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("--")) {
			continue
		}
		h.Write(line)
	}
	return hex.EncodeToString(h.Sum(nil))
}

var fileRe = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// List returns the embedded migrations ordered by version.
func List() ([]Migration, error) { return listFS(migrations.FS) }

func listFS(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		m := fileRe.FindStringSubmatch(e.Name())
		if e.IsDir() || m == nil {
			continue
		}
		v, _ := strconv.Atoi(m[1])
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("duplicate migration version %04d: %s and %s", v, prev, e.Name())
		}
		seen[v] = e.Name()
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		// CRLF -> LF: a Windows checkout (core.autocrlf) must not look like an edited file.
		b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
		out = append(out, Migration{Version: v, Name: e.Name(), SQL: string(b), Checksum: Sum(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

const ensureTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version    integer PRIMARY KEY,
  name       text        NOT NULL,
  checksum   text        NOT NULL,
  applied_at timestamptz NOT NULL DEFAULT now()
)`

// Applied returns version -> checksum of what the database has recorded.
func Applied(ctx context.Context, conn *pgx.Conn) (map[int]string, error) {
	if _, err := conn.Exec(ctx, ensureTable); err != nil {
		return nil, fmt.Errorf("schema_migrations: %w", err)
	}
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (struct {
		v int
		c string
	}, error) {
		var x struct {
			v int
			c string
		}
		err := r.Scan(&x.v, &x.c)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	out := make(map[int]string, len(got))
	for _, x := range got {
		out[x.v] = x.c
	}
	return out, nil
}

// ErrChecksum means an applied migration file was edited after it ran.
var ErrChecksum = errors.New("applied migration changed (checksum mismatch)")

func verify(all []Migration, applied map[int]string) error {
	for _, m := range all {
		if c, ok := applied[m.Version]; ok && !m.Accepts(c) {
			return fmt.Errorf("%w: %s", ErrChecksum, m.Name)
		}
	}
	return nil
}

// Up applies every pending migration in order and returns the names applied.
func Up(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	applied, err := Applied(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err := verify(all, applied); err != nil {
		return nil, err
	}
	var done []string
	for _, m := range all {
		if _, ok := applied[m.Version]; ok {
			continue
		}
		// No arguments -> simple protocol, so the multi-statement file (with its own BEGIN/COMMIT) runs as is.
		if _, err := conn.Exec(ctx, m.SQL); err != nil {
			return done, fmt.Errorf("%s: %w", m.Name, err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			m.Version, m.Name, m.Recorded()); err != nil {
			return done, fmt.Errorf("%s applied but not recorded: %w", m.Name, err)
		}
		done = append(done, m.Name)
	}
	return done, nil
}

// ErrNotBaselinable guards Baseline against empty or already tracked databases.
var ErrNotBaselinable = errors.New("baseline refused")

// Baseline records every embedded migration as applied WITHOUT running it. Only for a database
// whose DDL was applied outside the migrator (stand litopys-pg, 2026-09-29): the journal must
// exist and schema_migrations must be empty.
func Baseline(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	var journal *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.journal')::text`).Scan(&journal); err != nil {
		return nil, err
	}
	if journal == nil {
		return nil, fmt.Errorf("%w: public.journal does not exist — run migrate up instead", ErrNotBaselinable)
	}
	applied, err := Applied(ctx, conn)
	if err != nil {
		return nil, err
	}
	if len(applied) > 0 {
		return nil, fmt.Errorf("%w: schema_migrations already has %d rows", ErrNotBaselinable, len(applied))
	}
	all, err := List()
	if err != nil {
		return nil, err
	}
	var done []string
	for _, m := range all {
		if _, err := conn.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			m.Version, m.Name, m.Recorded()); err != nil {
			return done, err
		}
		done = append(done, m.Name)
	}
	return done, nil
}

// Status reports each embedded migration as applied or pending.
func Status(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	applied, err := Applied(ctx, conn)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range all {
		state := "pending"
		if c, ok := applied[m.Version]; ok {
			state = "applied"
			if !m.Accepts(c) {
				state = "CHECKSUM MISMATCH"
			}
		}
		out = append(out, fmt.Sprintf("%s  %s", m.Name, state))
	}
	return out, nil
}
