package ops

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/projector"
)

// Facts is everything status and doctor know about an installation, gathered once and read-only.
// A part that could not be read keeps its error instead of failing the whole run: doctor reports it.
type Facts struct {
	At     time.Time `json:"at"`
	Target string    `json:"target"`

	Manifest    *Manifest `json:"manifest,omitempty"`
	ManifestErr string    `json:"manifest_error,omitempty"`
	Expected    Expected  `json:"expected"`

	Apps    map[string]App `json:"apps,omitempty"` // service role (core, db, idp) -> app
	AppsErr string         `json:"apps_error,omitempty"`
	// Images: image tag -> registry digests, from the platform (what the running containers were created from).
	Images    map[string][]string `json:"-"`
	ImagesErr string              `json:"images_error,omitempty"`

	Health  Health    `json:"health"`
	DB      *DBFacts  `json:"db,omitempty"`
	DBErr   string    `json:"db_error,omitempty"`
	Backup  Backup    `json:"backup"`
	Pool    *Pool     `json:"pool,omitempty"`
	PoolErr string    `json:"pool_error,omitempty"`
	Cert    *Cert     `json:"cert,omitempty"`
	CertErr string    `json:"cert_error,omitempty"`
	IdP     *IdPFacts `json:"idp,omitempty"`
}

// Expected is the schema the installation should have: from the release manifest, or — without one —
// from the binary running the check (it is the release binary on an installation).
type Expected struct {
	MigrationHead    string `json:"migration_head"`
	ProjectorVersion string `json:"projector_version"`
	Source           string `json:"source"` // manifest | binary
}

// Health is GET /healthz and /version of the core service.
type Health struct {
	OK        bool   `json:"ok"`
	Err       string `json:"error,omitempty"`
	Commit    string `json:"commit,omitempty"`
	Migration string `json:"migration,omitempty"`
}

// DBFacts is read in one READ ONLY transaction.
type DBFacts struct {
	Applied   []Applied     `json:"applied"`
	Tenants   []Tenant      `json:"tenants"`
	Findings  []string      `json:"findings,omitempty"` // integrity findings of ops.verify_report (lag and version are checked apart)
	ServerNow time.Time     `json:"server_now"`
	ClockSkew time.Duration `json:"clock_skew_ns"` // database clock minus host clock, round trip halved out
}

// Applied is one row of schema_migrations.
type Applied struct {
	Version  int    `json:"version"`
	Name     string `json:"name"`
	Checksum string `json:"-"`
}

// Tenant is the journal and the projector of one tenant.
type Tenant struct {
	Name            string     `json:"name"`
	Head            int64      `json:"journal_head"`
	ProjectorSeq    int64      `json:"projector_seq"`
	ProjectorVer    string     `json:"projector_version"`
	OldestPendingAt *time.Time `json:"oldest_pending_at,omitempty"` // recorded_at of the first event the projector has not applied
}

// Backup is the newest dump in the backup directory and the restore check `litopys backup` recorded beside it.
type Backup struct {
	Dir    string     `json:"dir,omitempty"`
	Newest string     `json:"newest,omitempty"`
	At     *time.Time `json:"at,omitempty"`
	Err    string     `json:"error,omitempty"`
	// Checked: a record exists (<dump>.json); Verified: its restore check passed, else Findings say why
	Checked  bool     `json:"checked"`
	Verified bool     `json:"verified"`
	Findings []string `json:"findings,omitempty"`
}

// Cert is the leaf certificate the public address serves.
type Cert struct {
	Subject   string    `json:"subject"`
	NotAfter  time.Time `json:"not_after"`
	VerifyErr string    `json:"verify_error,omitempty"`
}

// IdPFacts is the OpenID discovery of the identity provider.
type IdPFacts struct {
	OK  bool   `json:"ok"`
	Err string `json:"error,omitempty"`
}

// TxBeginner is satisfied by *pgxpool.Pool and *pgx.Conn.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Gather reads everything. openDB opens the database pool (nil: the database is not checked).
func Gather(ctx context.Context, cfg *Config, t Target, openDB func(context.Context) (TxBeginner, func(), error)) *Facts {
	f := &Facts{At: time.Now().UTC(), Target: cfg.Target}
	if m, err := LoadManifest(cfg.Manifest); err != nil {
		f.ManifestErr = err.Error()
	} else {
		f.Manifest = m
	}
	f.Expected = expected(f.Manifest)

	roles := map[string]string{"core": cfg.Services.Core, "db": cfg.Services.DB, "idp": cfg.Services.IdP}
	var names []string
	for _, n := range roles {
		if n != "" {
			names = append(names, n)
		}
	}
	if apps, err := t.Apps(ctx, names); err != nil {
		f.AppsErr = err.Error()
	} else {
		f.Apps = map[string]App{}
		for role, n := range roles {
			if a, ok := apps[n]; ok {
				f.Apps[role] = a
			} else if n != "" {
				f.Apps[role] = App{Name: n, State: "MISSING"}
			}
		}
	}
	if imgs, err := t.Images(ctx); err != nil {
		f.ImagesErr = err.Error()
	} else {
		f.Images = imgs
	}
	f.Health = health(ctx, cfg.Endpoint)
	if openDB != nil {
		if db, closeDB, err := openDB(ctx); err != nil {
			f.DBErr = err.Error()
		} else {
			f.DB, err = GatherDB(ctx, db)
			closeDB()
			if err != nil {
				f.DB, f.DBErr = nil, err.Error()
			}
		}
	}
	f.Backup = newestBackup(cfg.Backup.Dir)
	if cfg.DataPool != "" {
		if p, err := t.Pool(ctx, cfg.DataPool); err != nil {
			f.PoolErr = err.Error()
		} else {
			f.Pool = &p
		}
	}
	if cfg.PublicURL != "" {
		if c, err := leafCert(ctx, cfg.PublicURL); err != nil {
			f.CertErr = err.Error()
		} else {
			f.Cert = c
		}
	}
	if cfg.IdPIssuer != "" {
		f.IdP = discovery(ctx, cfg.IdPIssuer)
	}
	return f
}

func expected(m *Manifest) Expected {
	if m != nil {
		return Expected{MigrationHead: m.Schema.MigrationHead, ProjectorVersion: m.Schema.ProjectorVersion, Source: "manifest"}
	}
	head, _ := migrationHead()
	return Expected{MigrationHead: head, ProjectorVersion: projector.Version, Source: "binary"}
}

// lagFinding is the verify_report finding doctor checks itself, with the age of the lag.
const lagFinding = "projector lags behind"

// GatherDB reads the schema, the journal heads, the projector and the integrity report in one READ ONLY
// transaction (schema_migrations is read directly: migrate.Applied would create the table). The caller's
// context carries the system scope, the role sees every tenant (as for `litopys verify`).
func GatherDB(ctx context.Context, db TxBeginner) (*DBFacts, error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out := &DBFacts{}

	sent := time.Now()
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&out.ServerNow); err != nil {
		return nil, fmt.Errorf("clock: %w", err)
	}
	back := time.Now()
	out.ClockSkew = out.ServerNow.Sub(sent.Add(back.Sub(sent) / 2))

	rows, err := tx.Query(ctx, `SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("schema_migrations: %w", err)
	}
	out.Applied, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Applied, error) {
		var a Applied
		return a, r.Scan(&a.Version, &a.Name, &a.Checksum)
	})
	if err != nil {
		return nil, fmt.Errorf("schema_migrations: %w", err)
	}

	rows, err = tx.Query(ctx, `SELECT t.name, h.last_seq, COALESCE(ps.last_seq, 0), COALESCE(ps.version, '-'),
	        (SELECT min(j.recorded_at) FROM journal j WHERE j.tenant_id = t.tenant_id AND j.seq > COALESCE(ps.last_seq, 0))
	   FROM tenant t JOIN journal_head h ON h.tenant_id = t.tenant_id
	   LEFT JOIN projection.projector_state ps ON ps.tenant_id = t.tenant_id
	  ORDER BY t.name`)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	out.Tenants, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tenant, error) {
		var t Tenant
		return t, r.Scan(&t.Name, &t.Head, &t.ProjectorSeq, &t.ProjectorVer, &t.OldestPendingAt)
	})
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}

	rows, err = tx.Query(ctx, `SELECT tenant, findings FROM ops.verify_report(NULL)`)
	if err != nil {
		return nil, fmt.Errorf("verify_report: %w", err)
	}
	type finding struct {
		tenant string
		found  []string
	}
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (finding, error) {
		var x finding
		return x, r.Scan(&x.tenant, &x.found)
	})
	if err != nil {
		return nil, fmt.Errorf("verify_report: %w", err)
	}
	for _, x := range found {
		for _, s := range x.found {
			if !strings.HasPrefix(s, lagFinding) {
				out.Findings = append(out.Findings, x.tenant+": "+s)
			}
		}
	}
	return out, nil
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

func health(ctx context.Context, endpoint string) Health {
	base := strings.TrimRight(endpoint, "/")
	body, err := get(ctx, base+"/healthz")
	if err != nil {
		return Health{Err: err.Error()}
	}
	h := Health{OK: strings.TrimSpace(string(body)) == "ok"}
	if !h.OK {
		h.Err = fmt.Sprintf("healthz answered %q", strings.TrimSpace(string(body)))
	}
	if body, err := get(ctx, base+"/version"); err == nil {
		var v struct {
			Commit    *string `json:"commit"`
			Migration *string `json:"migration"`
		}
		if json.Unmarshal(body, &v) == nil {
			if v.Commit != nil {
				h.Commit = *v.Commit
			}
			if v.Migration != nil {
				h.Migration = *v.Migration
			}
		}
	}
	return h
}

func get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return body, nil
}

// newestBackup finds the newest litopys_*.dump (the name the scheduled backup writes).
func newestBackup(dir string) Backup {
	b := Backup{Dir: dir}
	if dir == "" {
		b.Err = "no backup.dir in install config"
		return b
	}
	if _, err := os.Stat(dir); err != nil {
		b.Err = err.Error()
		return b
	}
	matches, err := filepath.Glob(filepath.Join(dir, "litopys_*.dump"))
	if err != nil {
		b.Err = err.Error()
		return b
	}
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil || st.IsDir() {
			continue
		}
		if t := st.ModTime().UTC(); b.At == nil || t.After(*b.At) {
			b.At, b.Newest = &t, filepath.Base(m)
		}
	}
	if b.Newest != "" {
		if rec, err := readRecord(filepath.Join(dir, b.Newest)); err == nil {
			b.Checked, b.Verified, b.Findings = true, rec.OK, rec.Findings
		}
	}
	return b
}

// leafCert reads the certificate the public address serves and verifies its chain and name separately,
// so an expired certificate is still reported with its date.
func leafCert(ctx context.Context, public string) (*Cert, error) {
	u, err := url.Parse(public)
	if err != nil {
		return nil, err
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
	}
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config: &tls.Config{ServerName: host, InsecureSkipVerify: true}} // verified below, after the date is read
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, errors.New("no certificate")
	}
	leaf := certs[0]
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	c := &Cert{Subject: leaf.Subject.CommonName, NotAfter: leaf.NotAfter.UTC()}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter}); err != nil {
		c.VerifyErr = err.Error()
	}
	return c, nil
}

func discovery(ctx context.Context, issuer string) *IdPFacts {
	body, err := get(ctx, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration")
	if err != nil {
		return &IdPFacts{Err: err.Error()}
	}
	var d struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return &IdPFacts{Err: "discovery is not JSON"}
	}
	if strings.TrimRight(d.Issuer, "/") != strings.TrimRight(issuer, "/") {
		return &IdPFacts{Err: fmt.Sprintf("discovery names issuer %q", d.Issuer)}
	}
	return &IdPFacts{OK: true}
}

// embedded returns the migrations of this binary by version (checksums are compared when the
// expectation comes from the binary itself).
func embedded() map[int]migrate.Migration {
	all, _ := migrate.List()
	out := make(map[int]migrate.Migration, len(all))
	for _, m := range all {
		out[m.Version] = m
	}
	return out
}
