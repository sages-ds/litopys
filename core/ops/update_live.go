package ops

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/projector"
	"litopys.smart-gw.co.ua/internal/pg"
)

// The live implementations of the update interfaces (update.go): the truenas adapter for the platform, the
// installation's PostgreSQL for the database, the core's HTTP endpoint for the health.

// AppCompose returns the compose an app runs now (app.config): what a rollback puts back. It holds the app's
// secrets: kept in memory, never written or logged.
func (t TrueNAS) AppCompose(ctx context.Context, name string) (any, error) {
	raw, err := t.C.Call(ctx, "app.config", name)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// SetAppCompose replaces the compose of a custom app and waits for the job: a string is a compose to render
// (custom_compose_config_string), what AppCompose returned goes back as it was (custom_compose_config).
func (t TrueNAS) SetAppCompose(ctx context.Context, name string, compose any) error {
	j, ok := t.C.(SecretJobCaller)
	if !ok {
		return errors.New("app.update: the transport cannot pass secrets off the command line")
	}
	var values map[string]any
	switch c := compose.(type) {
	case string:
		values = map[string]any{"custom_compose_config_string": c}
	case json.RawMessage:
		var m map[string]any
		if err := json.Unmarshal(c, &m); err != nil {
			return fmt.Errorf("app.update: compose: %w", err)
		}
		values = map[string]any{"custom_compose_config": m}
	default:
		return fmt.Errorf("app.update: compose of type %T", compose)
	}
	_, err := j.CallJobSecret(ctx, "app.update", name, values)
	return err
}

// SecretJobCaller runs a job method whose arguments hold secrets without putting them on a command line (where
// /proc/*/cmdline and ps show them for the length of the job): the compose with its passwords.
type SecretJobCaller interface {
	CallJobSecret(ctx context.Context, method string, args ...any) (json.RawMessage, error)
}

// CallJobSecret sends a short Python program over ssh's stdin (`python3 -`): the arguments travel inside it, the
// middleware client on the host calls the method and waits for the job — as deploy-facade.ps1 Invoke-Nas does.
func (s SSHMidclt) CallJobSecret(ctx context.Context, method string, args ...any) (json.RawMessage, error) {
	payload, err := json.Marshal(map[string]any{"m": method, "args": args})
	if err != nil {
		return nil, err
	}
	prog := "import base64, json\nfrom truenas_api_client import Client\n" +
		"a = json.loads(base64.b64decode('" + base64.StdEncoding.EncodeToString(payload) + "'))\n" +
		"with Client() as c:\n    r = c.call(a['m'], *a['args'], job=True)\nprint(json.dumps(r))\n"
	argv := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	if s.Key != "" {
		argv = append(argv, "-i", s.Key)
	}
	argv = append(argv, s.Host, "python3 -")
	cmd := exec.CommandContext(ctx, "ssh", argv...)
	cmd.Stdin = strings.NewReader(prog)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// stderr may echo a traceback with the arguments: only its last line, which names the error
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		return nil, fmt.Errorf("%s: %v: %s", method, err, lines[len(lines)-1])
	}
	return json.RawMessage(bytes.TrimSpace(stdout.Bytes())), nil
}

// LiveDatabase is the installation's PostgreSQL as an update needs it. Operator is a DSN of a role that owns the
// databases' maintenance (it creates and drops the copy, switches read-only, drains — the stage's superuser);
// Migrator is the DSN of the migrations' owner. Both name the live database; the copy is reached by replacing it.
type LiveDatabase struct {
	Operator, Migrator string
	AppRole            string   // the role projections are rebuilt and verified as (litopys_app)
	Accepted           []string // install.yaml accepted_findings: verify findings passed when they match word for word
	Log                func(format string, a ...any)
}

func (d LiveDatabase) conn(ctx context.Context, dsn, database string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if database != "" {
		cfg.Database = database
	}
	return pgx.ConnectConfig(ctx, cfg)
}

func (d LiveDatabase) live() (string, error) {
	cfg, err := pgx.ParseConfig(d.Operator)
	if err != nil {
		return "", err
	}
	if cfg.Database == "" {
		return "", errors.New("the operator DSN names no database")
	}
	return cfg.Database, nil
}

// Maintenance switches read-only. A database from before 0042 has no such mode (the first update to a release with
// it, or a rollback to a snapshot from before): the running core would not honour it anyway, and writes stop when the
// update stops the core — so it is logged and skipped.
func (d LiveDatabase) Maintenance(ctx context.Context, readOnly bool, reason string) error {
	c, err := d.conn(ctx, d.Operator, "")
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	var present bool
	if err := c.QueryRow(ctx, `SELECT to_regclass('ops.maintenance') IS NOT NULL`).Scan(&present); err != nil {
		return err
	}
	if !present {
		d.log("read-only mode: not in this database yet (before 0042), skipped — writes stop with the core")
		return nil
	}
	return SetMaintenance(ctx, c, readOnly, reason)
}

// ReadOnly reads the mode; a database from before 0042 has none (false).
func (d LiveDatabase) ReadOnly(ctx context.Context) (bool, string, error) {
	c, err := d.conn(ctx, d.Operator, "")
	if err != nil {
		return false, "", err
	}
	defer c.Close(ctx)
	var present bool
	if err := c.QueryRow(ctx, `SELECT to_regclass('ops.maintenance') IS NOT NULL`).Scan(&present); err != nil || !present {
		return false, "", err
	}
	m, err := GetMaintenance(ctx, c)
	if err != nil || !m.ReadOnly {
		return false, "", err
	}
	why := ""
	if m.Reason != nil {
		why = *m.Reason
	}
	return true, why, nil
}

// EventsSince counts the journal's events recorded after t — what a rollback to a snapshot taken at t loses.
func (d LiveDatabase) EventsSince(ctx context.Context, t time.Time) (int64, error) {
	c, err := d.conn(ctx, d.Operator, "")
	if err != nil {
		return 0, err
	}
	defer c.Close(ctx)
	var n int64
	err = c.QueryRow(ctx, `SELECT count(*) FROM journal WHERE recorded_at > $1`, t).Scan(&n)
	return n, err
}

// AuthSince counts the tokens issued or revoked after t.
func (d LiveDatabase) AuthSince(ctx context.Context, t time.Time) (int64, error) {
	c, err := d.conn(ctx, d.Operator, "")
	if err != nil {
		return 0, err
	}
	defer c.Close(ctx)
	var n int64
	err = c.QueryRow(ctx, `SELECT count(*) FROM auth.token WHERE created_at > $1 OR revoked_at > $1`, t).Scan(&n)
	return n, err
}

// MigrationHead is the newest applied migration's number.
func (d LiveDatabase) MigrationHead(ctx context.Context) (int, error) {
	c, err := d.conn(ctx, d.Operator, "")
	if err != nil {
		return 0, err
	}
	defer c.Close(ctx)
	var n int
	err = c.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM schema_migrations`).Scan(&n)
	return n, err
}

// WaitReady waits until the live database accepts a connection and a query (after a start PostgreSQL may still be
// recovering its WAL).
func (d LiveDatabase) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c, err := d.conn(ctx, d.Operator, "")
		if err == nil {
			_, err = c.Exec(ctx, `SELECT 1`)
			c.Close(ctx)
			if err == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the database is not ready after %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (d LiveDatabase) Drain(ctx context.Context, timeout time.Duration) error {
	c, err := d.conn(ctx, d.Operator, "")
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	return Drain(ctx, c, timeout)
}

var copyName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Rehearse copies the live database by its template (nobody may be connected), upgrades the copy and drops it.
func (d LiveDatabase) Rehearse(ctx context.Context) error {
	live, err := d.live()
	if err != nil {
		return err
	}
	copyDB := live + "_rehearse_" + time.Now().UTC().Format("20060102150405")
	if !copyName.MatchString(live) || !copyName.MatchString(copyDB) {
		return fmt.Errorf("database name %q is not a plain identifier", live)
	}
	admin, err := d.conn(ctx, d.Operator, "postgres")
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+copyDB+` TEMPLATE `+live+` OWNER litopys_migrator`); err != nil {
		return fmt.Errorf("copy %s (is anything still connected to it?): %w", live, err)
	}
	defer func() { // a connection of its own: the one above dies with a cancelled context
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		c, err := d.conn(ctx, d.Operator, "postgres")
		if err == nil {
			_, err = c.Exec(ctx, `DROP DATABASE IF EXISTS `+copyDB+` WITH (FORCE)`)
			c.Close(ctx)
		}
		if err != nil {
			d.log("drop the copy %s: %v — drop it by hand", copyDB, err)
		}
	}()
	return d.upgrade(ctx, copyDB)
}

// Upgrade migrates the live database, rebuilds the projections if the code's projector version differs, verifies.
func (d LiveDatabase) Upgrade(ctx context.Context) error {
	live, err := d.live()
	if err != nil {
		return err
	}
	return d.upgrade(ctx, live)
}

func (d LiveDatabase) upgrade(ctx context.Context, database string) error {
	mig, err := d.conn(ctx, d.Migrator, database)
	if err != nil {
		return err
	}
	applied, err := migrate.Up(ctx, mig)
	mig.Close(ctx)
	if err != nil {
		return fmt.Errorf("%s: migrate: %w", database, err)
	}
	d.log("%s: %d migration(s) applied", database, len(applied))
	cfg, err := pgxpool.ParseConfig(d.Operator)
	if err != nil {
		return err
	}
	cfg.ConnConfig.Database = database
	role := d.AppRole
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	cfg.PrepareConn = pg.PrepareConn
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	var tenant uuid.UUID
	var stored *string
	if err := pool.QueryRow(sys, `SELECT t.tenant_id, s.version FROM tenant t
	        LEFT JOIN projection.projector_state s ON s.tenant_id = t.tenant_id WHERE t.name = 'default'`).Scan(&tenant, &stored); err != nil {
		return err
	}
	if stored == nil || *stored != projector.Version {
		n, err := projector.Rebuild(ctx, pool, tenant)
		if err != nil {
			return fmt.Errorf("%s: rebuild: %w", database, err)
		}
		d.log("%s: rebuilt, %d events replayed with projector version %s", database, n, projector.Version)
	}
	rows, err := pool.Query(sys, `SELECT findings FROM ops.verify_report($1)`, projector.Version)
	if err != nil {
		return err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[[]string])
	if err != nil {
		return err
	}
	var all []string
	for _, f := range found {
		all = append(all, f...)
	}
	all, passed := SplitAccepted(all, d.Accepted)
	for _, f := range passed {
		d.log("%s: verify: accepted by install.yaml: %s", database, f)
	}
	if len(all) > 0 {
		return fmt.Errorf("%s: verify: %s", database, strings.Join(all, "; "))
	}
	d.log("%s: verify ok", database)
	return nil
}

func (d LiveDatabase) log(format string, a ...any) {
	if d.Log != nil {
		d.Log(format, a...)
	}
}

// HTTPHealth checks the core at its endpoint (http://host:port).
type HTTPHealth struct {
	Endpoint string
	Client   *http.Client
}

func (h HTTPHealth) client() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (h HTTPHealth) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(h.Endpoint, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	return h.client().Do(req)
}

func (h HTTPHealth) Commit(ctx context.Context) string {
	r, err := h.get(ctx, "/version")
	if err != nil {
		return ""
	}
	defer r.Body.Close()
	var v struct {
		Commit string `json:"commit"`
	}
	if r.StatusCode != http.StatusOK || json.NewDecoder(r.Body).Decode(&v) != nil {
		return ""
	}
	return v.Commit
}

// Check: /healthz answers ok, /version reports the commit, the metrics show no projector lag and the projector
// version of the code (litopys_projector_version_ok 1).
func (h HTTPHealth) Check(ctx context.Context, commit string) error {
	r, err := h.get(ctx, "/healthz")
	if err != nil {
		return fmt.Errorf("healthz: %w", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: HTTP %d", r.StatusCode)
	}
	if got := h.Commit(ctx); got != commit {
		return fmt.Errorf("version: commit %q, want %q", got, commit)
	}
	r, err = h.get(ctx, "/metrics")
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	defer r.Body.Close()
	want := map[string]string{"litopys_projector_lag": "0", "litopys_projector_version_ok": "1"}
	seen := map[string]string{}
	sc := bufio.NewScanner(r.Body)
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), " ")
		if _, wanted := want[name]; ok && wanted {
			seen[name] = strings.TrimSpace(value)
		}
	}
	for name, v := range want {
		if seen[name] != v {
			return fmt.Errorf("metrics: %s = %q, want %s", name, seen[name], v)
		}
	}
	return nil
}
