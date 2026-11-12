package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// `litopys backup` / `litopys restore`: the port of the scheduled backup's playbook into the
// binary. A backup reads the live journal heads, dumps the database (pg_dump -Fc) as a login that bypasses RLS,
// restores the dump into a scratch database beside it, runs ops.verify_report there, checks that the copy's heads are
// not behind the live ones read before the dump, drops the scratch database, records the result next to the dump and
// prunes old dumps. A restore puts a dump into a new database beside the live one and verifies it; switching the
// service to it is a person's step, not this command's.

// DumpPrefix is how dumps are named in the backup directory (litopys status / doctor look for it).
const DumpPrefix = "litopys_"

// dbLocale is how every Litopys database is created (= internal/pgtest.Locale).
const dbLocale = `TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER libc LOCALE 'C.UTF-8'`

// PGTools runs the PostgreSQL client programs. The password travels in PGPASSWORD of the child, never in argv.
type PGTools interface {
	Dump(ctx context.Context, conn PGConn, file string) error
	// Restore loads a custom-format dump; keepOwners keeps owners and privileges (a restore the service may run on),
	// otherwise everything is owned by the restoring login (a scratch copy for a check).
	Restore(ctx context.Context, conn PGConn, file string, keepOwners bool) error
}

// PGConn is a libpq connection URL without its password, and the password apart.
type PGConn struct {
	URL      string
	Password string
}

// SplitDSN takes a postgres:// URL apart for a child program: database replaced when db is not "", the password
// moved out of the URL.
func SplitDSN(dsn, db string) (PGConn, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return PGConn{}, errors.New("the database URL must be postgres://user:password@host:port/db")
	}
	var pw string
	if u.User != nil {
		pw, _ = u.User.Password()
		u.User = url.User(u.User.Username())
	}
	if db != "" {
		u.Path = "/" + db
	}
	return PGConn{URL: u.String(), Password: pw}, nil
}

// DBName is the database a URL names.
func DBName(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	name := strings.TrimPrefix(u.Path, "/")
	if !copyName.MatchString(name) {
		return "", fmt.Errorf("database name %q is not a plain identifier", name)
	}
	return name, nil
}

// ExecTools runs pg_dump and pg_restore from Bin ("" — from PATH).
type ExecTools struct{ Bin string }

func (e ExecTools) run(ctx context.Context, prog string, conn PGConn, args ...string) error {
	path := prog
	if e.Bin != "" {
		path = filepath.Join(e.Bin, prog)
	}
	// -w: never prompt for a password (a scheduled run would hang); the URL's password, when it has one, wins over an
	// inherited PGPASSWORD, otherwise the inherited one (or ~/.pgpass) is used as by any libpq client
	cmd := exec.CommandContext(ctx, path, append([]string{"-w"}, args...)...)
	cmd.Env = os.Environ()
	if conn.Password != "" {
		cmd.Env = append(cmd.Env, "PGPASSWORD="+conn.Password)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", prog, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (e ExecTools) Dump(ctx context.Context, conn PGConn, file string) error {
	return e.run(ctx, "pg_dump", conn, "-Fc", "-d", conn.URL, "-f", file)
}

func (e ExecTools) Restore(ctx context.Context, conn PGConn, file string, keepOwners bool) error {
	args := []string{"--exit-on-error", "-d", conn.URL}
	if !keepOwners {
		args = append(args, "--no-owner", "--no-privileges")
	}
	return e.run(ctx, "pg_restore", conn, append(args, file)...)
}

// CopyTenant is one row of ops.verify_report on a restored copy.
type CopyTenant struct {
	Tenant       string   `json:"tenant"`
	JournalHead  int64    `json:"journal_head"`
	ProjectorSeq int64    `json:"projector_seq"`
	ProjectorVer string   `json:"projector_version"`
	Findings     []string `json:"findings,omitempty"`
}

// BackupRecord is written next to each dump (<dump>.json): what was dumped and how its restore check went.
// doctor reads it for the newest dump.
type BackupRecord struct {
	Dump       string           `json:"dump"`
	Size       int64            `json:"size"`
	Database   string           `json:"database"`
	Login      string           `json:"login"`
	StartedAt  time.Time        `json:"started_at"`
	LiveHeads  map[string]int64 `json:"live_heads"` // read before the dump: the copy may be ahead, never behind
	VerifiedAt *time.Time       `json:"verified_at,omitempty"`
	Copy       []CopyTenant     `json:"copy,omitempty"`
	Findings   []string         `json:"findings,omitempty"`
	OK         bool             `json:"ok"`
}

// CompareHeads: every live tenant is in the copy and not behind; every finding of verify_report counts.
func CompareHeads(live map[string]int64, restored []CopyTenant) []string {
	var out []string
	if len(restored) == 0 {
		return []string{"the copy holds no tenant (a dump that saw no rows?)"}
	}
	seen := map[string]bool{}
	for _, c := range restored {
		seen[c.Tenant] = true
		for _, f := range c.Findings {
			out = append(out, c.Tenant+": "+f)
		}
		if h, ok := live[c.Tenant]; ok && c.JournalHead < h {
			out = append(out, fmt.Sprintf("%s: copy head %d is behind the live head %d", c.Tenant, c.JournalHead, h))
		}
	}
	names := make([]string, 0, len(live))
	for n := range live {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !seen[n] {
			out = append(out, n+": missing in the copy")
		}
	}
	return out
}

// ErrRLSLogin: the login is subject to row-level security — its dump would be silently partial.
var ErrRLSLogin = errors.New("the login does not bypass row-level security: its dump would miss rows")

// Backuper runs one backup or one check of a dump.
type Backuper struct {
	DSN      string // the backup login (LOGIN CREATEDB BYPASSRLS, pg_read_all_data) on the live database
	Dir      string
	KeepDays int
	Tools    PGTools
	Log      func(format string, a ...any)
	Now      func() time.Time
}

func (b *Backuper) defaults() {
	if b.Log == nil {
		b.Log = func(string, ...any) {}
	}
	if b.Now == nil {
		b.Now = time.Now
	}
}

// Run dumps, checks the dump and prunes. The record is written whatever the check says; an error means no usable
// backup was made.
func (b *Backuper) Run(ctx context.Context) (*BackupRecord, error) {
	b.defaults()
	live, err := DBName(b.DSN)
	if err != nil {
		return nil, err
	}
	c, err := pgx.Connect(ctx, b.DSN)
	if err != nil {
		return nil, err
	}
	login, err := bypassesRLS(ctx, c)
	if err != nil {
		c.Close(ctx)
		return nil, err
	}
	heads, err := journalHeads(ctx, c)
	c.Close(ctx)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return nil, err
	}
	start := b.Now().UTC()
	name := DumpPrefix + start.Format("20060102-150405.000") + ".dump"
	file := filepath.Join(b.Dir, name)
	if _, err := os.Stat(file); err == nil {
		return nil, fmt.Errorf("a dump named %s exists already", name)
	}
	conn, err := SplitDSN(b.DSN, "")
	if err != nil {
		return nil, err
	}
	if err := b.Tools.Dump(ctx, conn, file+".part"); err != nil {
		_ = os.Remove(file + ".part")
		return nil, err
	}
	if err := os.Rename(file+".part", file); err != nil {
		return nil, err
	}
	st, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	rec := &BackupRecord{Dump: name, Size: st.Size(), Database: live, Login: login, StartedAt: start, LiveHeads: heads}
	b.Log("dump %s, %d bytes", name, rec.Size)
	err = b.check(ctx, file, rec)
	if werr := writeRecord(file, rec); werr != nil && err == nil {
		err = werr
	}
	if err != nil {
		return rec, err
	}
	b.prune()
	return rec, nil
}

// Verify checks an existing dump: restore into a scratch database, verify_report, heads against the record written at
// the dump (when there is one).
func (b *Backuper) Verify(ctx context.Context, file string) (*BackupRecord, error) {
	b.defaults()
	rec, err := readRecord(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("the record of %s is unreadable (%w): move it away to check the dump on its own", file, err)
	}
	if rec == nil {
		st, err := os.Stat(file)
		if err != nil {
			return nil, err
		}
		rec = &BackupRecord{Dump: filepath.Base(file), Size: st.Size()}
	}
	rec.VerifiedAt, rec.Copy, rec.Findings, rec.OK = nil, nil, nil, false
	err = b.check(ctx, file, rec)
	if werr := writeRecord(file, rec); werr != nil && err == nil {
		err = werr
	}
	return rec, err
}

// check restores file into a scratch database beside the live one and fills the record's check.
func (b *Backuper) check(ctx context.Context, file string, rec *BackupRecord) error {
	live, err := DBName(b.DSN)
	if err != nil {
		return err
	}
	b.sweep(ctx, live)
	scratch := live + "_verify_" + b.Now().UTC().Format("20060102150405")
	cp, err := restoreAndReport(ctx, b.DSN, b.Tools, file, scratch, false, "")
	if err != nil {
		rec.Findings = []string{"restore: " + err.Error()}
		return fmt.Errorf("restore check of %s: %w", rec.Dump, err)
	}
	at := b.Now().UTC()
	rec.VerifiedAt, rec.Copy = &at, cp
	for _, c := range cp {
		b.Log("copy tenant %s: journal head %d, projector %d (v%s)", c.Tenant, c.JournalHead, c.ProjectorSeq, c.ProjectorVer)
	}
	if rec.LiveHeads == nil {
		b.Log("no record of the live heads at the dump: the copy is checked on its own")
	}
	rec.Findings = CompareHeads(rec.LiveHeads, cp)
	rec.OK = len(rec.Findings) == 0
	if !rec.OK {
		return fmt.Errorf("restore check of %s: %s", rec.Dump, strings.Join(rec.Findings, "; "))
	}
	b.Log("verify: OK")
	return nil
}

// sweep drops scratch databases an earlier check left behind (a run killed before its cleanup): those of this
// database older than an hour, by the time in their name. A failure is only logged.
func (b *Backuper) sweep(ctx context.Context, live string) {
	c, err := connectDB(ctx, b.DSN, "postgres")
	if err != nil {
		b.Log("sweep: %v", err)
		return
	}
	rows, err := c.Query(ctx, `SELECT datname FROM pg_database WHERE starts_with(datname, $1)`, live+"_verify_")
	var names []string
	if err == nil {
		names, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	c.Close(ctx)
	if err != nil {
		b.Log("sweep: %v", err)
		return
	}
	for _, n := range names {
		at, err := time.Parse("20060102150405", strings.TrimPrefix(n, live+"_verify_"))
		if err != nil || b.Now().Sub(at) < time.Hour || !copyName.MatchString(n) {
			continue
		}
		b.Log("dropping %s, left by an earlier check", n)
		dropDB(ctx, b.DSN, n)
	}
}

// restoreAndReport creates db (owned by owner, "" — the login), restores the dump into it, reads verify_report and,
// unless keep, drops it again; a kept database whose restore failed is dropped too.
func restoreAndReport(ctx context.Context, dsn string, tools PGTools, file, db string, keep bool, owner string) (out []CopyTenant, err error) {
	if !copyName.MatchString(db) {
		return nil, fmt.Errorf("database name %q is not a plain identifier", db)
	}
	admin, err := connectDB(ctx, dsn, "postgres")
	if err != nil {
		return nil, err
	}
	stmt := `CREATE DATABASE ` + db + ` ` + dbLocale
	if owner != "" {
		stmt = `CREATE DATABASE ` + db + ` OWNER ` + pgx.Identifier{owner}.Sanitize() + ` ` + dbLocale
	}
	_, err = admin.Exec(ctx, stmt)
	admin.Close(ctx)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", db, err)
	}
	defer func() {
		if !keep || err != nil {
			dropDB(ctx, dsn, db)
		}
	}()
	conn, err := SplitDSN(dsn, db)
	if err != nil {
		return nil, err
	}
	if err := tools.Restore(ctx, conn, file, keep); err != nil {
		return nil, err
	}
	c, err := connectDB(ctx, dsn, db)
	if err != nil {
		return nil, err
	}
	defer c.Close(ctx)
	rows, err := c.Query(ctx, `SELECT tenant, journal_head, projector_seq, projector_version, findings FROM ops.verify_report(NULL)`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CopyTenant, error) {
		var t CopyTenant
		err := r.Scan(&t.Tenant, &t.JournalHead, &t.ProjectorSeq, &t.ProjectorVer, &t.Findings)
		return t, err
	})
}

// dropDB drops a scratch database on a connection of its own (the caller's context may be cancelled).
func dropDB(ctx context.Context, dsn, db string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	c, err := connectDB(ctx, dsn, "postgres")
	if err == nil {
		_, err = c.Exec(ctx, `DROP DATABASE IF EXISTS `+db+` WITH (FORCE)`)
		c.Close(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "drop the scratch database %s: %v — drop it by hand\n", db, err)
	}
}

func connectDB(ctx context.Context, dsn, db string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.Database = db
	return pgx.ConnectConfig(ctx, cfg)
}

// bypassesRLS refuses a login that row-level security applies to (litopys_app, the facade's login): pg_dump as it
// would dump only what the policies show. Returns the login's name.
func bypassesRLS(ctx context.Context, c *pgx.Conn) (string, error) {
	var login string
	var bypass bool
	if err := c.QueryRow(ctx, `SELECT current_user, rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&login, &bypass); err != nil {
		return "", err
	}
	if !bypass {
		return login, fmt.Errorf("%w (%s): use the backup login (stand backup-login litopys_backup)", ErrRLSLogin, login)
	}
	return login, nil
}

func journalHeads(ctx context.Context, c *pgx.Conn) (map[string]int64, error) {
	rows, err := c.Query(ctx, `SELECT t.name, h.last_seq FROM tenant t JOIN journal_head h USING (tenant_id)`)
	if err != nil {
		return nil, err
	}
	heads := map[string]int64{}
	var name string
	var seq int64
	_, err = pgx.ForEachRow(rows, []any{&name, &seq}, func() error { heads[name] = seq; return nil })
	return heads, err
}

func writeRecord(dump string, rec *BackupRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dump+".json", append(b, '\n'), 0o600)
}

func readRecord(dump string) (*BackupRecord, error) {
	b, err := os.ReadFile(dump + ".json")
	if err != nil {
		return nil, err
	}
	var rec BackupRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// prune removes dumps (and their records) older than KeepDays, never the newest one, and unfinished dumps left by a
// crash more than ten hours ago. A failure is only logged.
func (b *Backuper) prune() {
	if b.KeepDays < 1 {
		return
	}
	type dump struct {
		path string
		at   time.Time
	}
	var dumps []dump
	names, _ := filepath.Glob(filepath.Join(b.Dir, DumpPrefix+"*.dump"))
	for _, n := range names {
		if st, err := os.Stat(n); err == nil && !st.IsDir() {
			dumps = append(dumps, dump{n, st.ModTime()})
		}
	}
	// by modification time, as doctor finds the newest
	sort.Slice(dumps, func(i, j int) bool { return dumps[i].at.Before(dumps[j].at) })
	now := b.Now()
	for i, dd := range dumps {
		d := dd.path
		if i == len(dumps)-1 || now.Sub(dd.at) <= time.Duration(b.KeepDays)*24*time.Hour {
			continue
		}
		if err := os.Remove(d); err != nil {
			b.Log("prune %s: %v", filepath.Base(d), err)
			continue
		}
		_ = os.Remove(d + ".json")
		b.Log("pruned %s", filepath.Base(d))
	}
	parts, _ := filepath.Glob(filepath.Join(b.Dir, DumpPrefix+"*.dump.part"))
	for _, p := range parts {
		if st, err := os.Stat(p); err == nil && now.Sub(st.ModTime()) > 10*time.Hour {
			_ = os.Remove(p)
		}
	}
}

// Restore puts a dump into a new database beside the live one (owners and privileges as dumped) and verifies it.
// The database must not exist; it is kept for a person to look at and switch to. dsn is a login that may create
// databases and restore owners (the operator).
func Restore(ctx context.Context, dsn string, tools PGTools, file, into, owner string) ([]CopyTenant, []string, error) {
	live, err := DBName(dsn)
	if err != nil {
		return nil, nil, err
	}
	if into == live {
		return nil, nil, fmt.Errorf("restore never goes over the live database %s: name a new one", live)
	}
	// owners and privileges as dumped, rows under RLS, verify_report (granted to litopys_app only): a superuser's work
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	var super bool
	err = c.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super)
	c.Close(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !super {
		return nil, nil, errors.New("restore keeps the dump's owners and privileges: LITOPYS_OPERATOR_URL must be a superuser login")
	}
	cp, err := restoreAndReport(ctx, dsn, tools, file, into, true, owner)
	if err != nil {
		return nil, nil, err
	}
	var heads map[string]int64
	if rec, _ := readRecord(file); rec != nil {
		heads = rec.LiveHeads
	}
	return cp, CompareHeads(heads, cp), nil
}
