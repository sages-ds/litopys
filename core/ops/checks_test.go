package ops

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/projector"
)

const (
	tag    = "registry.example/acme/litopys:357ccc634a72"
	digest = "registry.example/acme/litopys@sha256:a30d8a941adc8b846a56d7b7b7995e2b91ad4b97744abe3269c31419e0b034dd"
	other  = "registry.example/acme/litopys@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

// healthy is a sound installation; each provocation breaks one part of it.
func healthy(t *testing.T) *Facts {
	t.Helper()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	head, err := migrationHead()
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{Version: "357ccc634a72", Commit: "357ccc634a72aaaa", Images: map[string]string{"core": digest},
		Schema: ManifestSchema{MigrationHead: head, ProjectorVersion: projector.Version}}
	var applied []Applied
	for v, e := range embedded() {
		applied = append(applied, Applied{Version: v, Name: e.Name, Checksum: e.Checksum})
	}
	backupAt := now.Add(-5 * time.Hour)
	return &Facts{
		At: now, Target: "truenas", Manifest: m, Expected: expected(m),
		Apps: map[string]App{
			"core": {Name: "litopys-facade", State: "RUNNING", Containers: []Container{{Service: "facade", Image: tag, State: "running"}}},
			"db":   {Name: "litopys-pg", State: "RUNNING"},
		},
		Images: map[string][]string{tag: {digest}},
		Health: Health{OK: true, Commit: "357ccc634a72aaaa0000", Migration: head},
		DB: &DBFacts{Applied: applied, ServerNow: now, ClockSkew: 300 * time.Millisecond,
			Tenants: []Tenant{{Name: "default", Head: 2211, ProjectorSeq: 2211, ProjectorVer: projector.Version}}},
		Backup: Backup{Dir: "/var/backups/litopys", Newest: "litopys_20261029-031500.dump", At: &backupAt},
		Pool:   &Pool{Name: "fastpool", Size: 100, Free: 80},
		Cert:   &Cert{Subject: "mcp.example", NotAfter: now.Add(60 * 24 * time.Hour)},
		IdP:    &IdPFacts{OK: true},
	}
}

func find(t *testing.T, rs []Result, check string, want Level) Result {
	t.Helper()
	for _, r := range rs {
		if r.Check == check && r.Level == want {
			return r
		}
	}
	t.Fatalf("no %s line at level %s in %+v", check, want, rs)
	return Result{}
}

func TestHealthyIsClean(t *testing.T) {
	rs := Evaluate(healthy(t))
	if w := Worst(rs); w != OK {
		t.Fatalf("worst = %s, want ok: %+v", w, rs)
	}
}

// Each provocation: the level and the advice an operator acts on.
func TestProvocations(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(f *Facts)
		check  string
		level  Level
		advice string
	}{
		{"stopped app", func(f *Facts) { a := f.Apps["db"]; a.State = "STOPPED"; f.Apps["db"] = a }, "services", Crit, "update --resume"},
		{"missing app", func(f *Facts) { f.Apps["idp"] = App{Name: "keycloak", State: "MISSING"} }, "services", Crit, "logs"},
		{"healthz down", func(f *Facts) { f.Health = Health{Err: "connection refused"} }, "healthz", Crit, "update --resume"},
		{"other digest", func(f *Facts) { f.Images[tag] = []string{other} }, "image", Warn, "litopys update 357ccc634a72"},
		{"other commit", func(f *Facts) { f.Health.Commit = "1e95aeb0" }, "image", Warn, "litopys update"},
		{"no manifest", func(f *Facts) { f.Manifest, f.ManifestErr = nil, "open x: not found"; f.Expected = expected(nil) }, "image", Warn, "manifest"},
		{"database behind", func(f *Facts) { f.DB.Applied = f.DB.Applied[:0] }, "migrations", Crit, "not a manual migrate up"},
		{"database ahead", func(f *Facts) { f.DB.Applied = append(f.DB.Applied, Applied{Version: 9999, Name: "9999_x.sql"}) }, "migrations", Crit, "litopys update"},
		{"edited migration", func(f *Facts) {
			f.Manifest, f.Expected = nil, expected(nil)
			f.DB.Applied[0].Checksum = "x"
		}, "migrations", Crit, "litopys update"},
		{"projector lag for minutes", func(f *Facts) {
			old := f.At.Add(-6 * time.Minute)
			f.DB.Tenants[0].ProjectorSeq, f.DB.Tenants[0].OldestPendingAt = 2200, &old
		}, "projector", Crit, "runbook"},
		{"projector of another version", func(f *Facts) { f.DB.Tenants[0].ProjectorVer = "1" }, "projector", Crit, "runbook"},
		{"verify finding", func(f *Facts) { f.DB.Findings = []string{"default: statements without their asserted event"} }, "verify", Crit, "restore"},
		{"database unreachable", func(f *Facts) { f.DB, f.DBErr = nil, "connect: refused" }, "database", Crit, "credentials"},
		{"old backup", func(f *Facts) { old := f.At.Add(-27 * time.Hour); f.Backup.At = &old }, "backup", Crit, "litopys backup"},
		{"no backup", func(f *Facts) { f.Backup.At, f.Backup.Newest = nil, "" }, "backup", Crit, "litopys backup"},
		{"backup not visible", func(f *Facts) { f.Backup = Backup{Err: "no such directory"} }, "backup", Warn, "backup host"},
		{"backup failed its restore check", func(f *Facts) {
			f.Backup.Checked, f.Backup.Findings = true, []string{"default: copy head 3 is behind the live head 5"}
		}, "backup", Crit, "--verify"},
		{"pool almost full", func(f *Facts) { f.Pool.Free = 10 }, "pool", Warn, "free space"},
		{"certificate in 20 days", func(f *Facts) { f.Cert.NotAfter = f.At.Add(20 * 24 * time.Hour) }, "certificate", Warn, "cert install"},
		{"certificate in 3 days", func(f *Facts) { f.Cert.NotAfter = f.At.Add(3 * 24 * time.Hour) }, "certificate", Crit, "cert install"},
		{"certificate expired", func(f *Facts) {
			f.Cert.NotAfter, f.Cert.VerifyErr = f.At.Add(-time.Hour), "x509: certificate has expired"
		}, "certificate", Crit, "cert install"},
		{"clock skew", func(f *Facts) { f.DB.ClockSkew = -4500 * time.Millisecond }, "clock", Warn, "NTP"},
		{"idp down", func(f *Facts) { f.IdP = &IdPFacts{Err: "502"} }, "idp", Warn, "identity provider"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := healthy(t)
			c.break_(f)
			rs := Evaluate(f)
			r := find(t, rs, c.check, c.level)
			if !strings.Contains(r.Advice, c.advice) {
				t.Errorf("advice %q, want it to mention %q", r.Advice, c.advice)
			}
			if Worst(rs) != c.level {
				t.Errorf("worst = %s, want %s: %+v", Worst(rs), c.level, rs)
			}
		})
	}
}

// A database migrated before checksums skipped comment lines keeps the whole-file sums: not an edit.
func TestLegacyChecksumIsOK(t *testing.T) {
	f := healthy(t)
	f.Manifest, f.Expected = nil, expected(nil)
	for i, a := range f.DB.Applied {
		if s := migrate.LegacySum(a.Version); s != "" {
			f.DB.Applied[i].Checksum = s
		}
	}
	find(t, Evaluate(f), "migrations", OK)
}

func TestLagWithinGraceIsOK(t *testing.T) {
	f := healthy(t)
	recent := f.At.Add(-30 * time.Second)
	f.DB.Tenants[0].ProjectorSeq, f.DB.Tenants[0].OldestPendingAt = 2210, &recent
	r := find(t, Evaluate(f), "projector", OK)
	if !strings.Contains(r.Detail, "catching up") {
		t.Errorf("detail %q", r.Detail)
	}
}

func TestExitCodeIsWorstLevel(t *testing.T) {
	if int(OK) != 0 || int(Warn) != 1 || int(Crit) != 2 {
		t.Fatal("exit codes: 0 ok, 1 warning, 2 critical")
	}
	b, _ := json.Marshal(Result{Check: "x", Level: Crit})
	if !strings.Contains(string(b), `"level":"crit"`) {
		t.Errorf("json %s", b)
	}
}

type fakeCaller map[string]string

func (f fakeCaller) Call(_ context.Context, method string, _ ...any) (json.RawMessage, error) {
	return json.RawMessage(f[method]), nil
}

// The shapes TrueNAS 25.10 returns (recorded from the stage, trimmed).
func TestTrueNASAdapter(t *testing.T) {
	tn := TrueNAS{C: fakeCaller{
		"app.query": `[{"name":"litopys-facade","state":"RUNNING","active_workloads":{"container_details":[
			{"service_name":"facade","image":"` + tag + `","state":"running"},
			{"service_name":"migrate","image":"` + tag + `","state":"exited"}]}}]`,
		"app.image.query": `[{"repo_tags":["` + tag + `"],"repo_digests":["` + digest + `"]}]`,
		"pool.query":      `[{"name":"fastpool","size":493921239040,"free":422360514560}]`,
	}}
	ctx := context.Background()
	apps, err := tn.Apps(ctx, []string{"litopys-facade", "litopys-pg"})
	if err != nil {
		t.Fatal(err)
	}
	if a := apps["litopys-facade"]; a.State != "RUNNING" || len(a.Containers) != 2 || a.Containers[1].State != "exited" {
		t.Errorf("app %+v", a)
	}
	imgs, err := tn.Images(ctx)
	if err != nil || !hasDigest(imgs[tag], digest) {
		t.Errorf("images %v %v", imgs, err)
	}
	p, err := tn.Pool(ctx, "fastpool")
	if err != nil || p.Free != 422360514560 {
		t.Errorf("pool %+v %v", p, err)
	}
	cfg := &Config{Target: "truenas"}
	cfg.Services.Core, cfg.Services.DB = "litopys-facade", "litopys-pg"
	f := Gather(ctx, cfg, tn, nil)
	if f.Apps["db"].State != "MISSING" {
		t.Errorf("an app the platform does not know must be MISSING: %+v", f.Apps)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote(`[["name","=","it's"]]`); got != `'[["name","=","it'\''s"]]'` {
		t.Errorf("got %s", got)
	}
}

func TestNewestBackup(t *testing.T) {
	dir := t.TempDir()
	for i, name := range []string{"litopys_20261027-031500.dump", "litopys_20261028-031500.dump", "litopys_20261029-031500.dump.part", "other.dump"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 10, 27+i, 3, 15, 0, 0, time.UTC)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	b := newestBackup(dir)
	if b.Err != "" || b.Newest != "litopys_20261028-031500.dump" {
		t.Errorf("newest %+v (a .part is not a dump)", b)
	}
	if b := newestBackup(filepath.Join(dir, "absent")); b.Err == "" {
		t.Error("an absent directory must be an error, not 'no dump'")
	}
}

func TestManifestRoundTrip(t *testing.T) {
	m, err := BuildManifest("357ccc634a72", "357ccc634a72aaaa", digest)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "litopys-release.yaml")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Images["core"] != digest || got.Schema.ProjectorVersion != projector.Version || got.Schema.MigrationHead == "" {
		t.Errorf("manifest %+v", got)
	}
	if _, err := BuildManifest("v", "c", tag); err == nil {
		t.Error("a tag without a digest must be refused")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "install.yaml")
	body := "target: truenas\nmanifest: rel.yaml\nservices: { core: litopys-facade }\nendpoint: http://x:8798\ntruenas: { ssh: u@h }\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Manifest != filepath.Join(dir, "rel.yaml") {
		t.Errorf("manifest path %q is not relative to the config", c.Manifest)
	}
	if err := os.WriteFile(p, []byte(body+"password: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil {
		t.Error("an unknown key (here: a secret) must be refused")
	}
}

// The stage config of the repository must stay loadable.
func TestStageConfig(t *testing.T) {
	if _, err := LoadConfig(installConfig(t, "install.stage.yaml")); err != nil {
		t.Fatal(err)
	}
}

// installConfig is the path of one of the repository's own installation configs (deploy/truenas). They describe
// one installation and stay out of the public snapshot (scripts/public-allowlist.txt): without deploy/ at all the
// test is skipped; with deploy/ present a missing file still fails in LoadConfig.
func installConfig(t *testing.T, name string) string {
	t.Helper()
	deploy := filepath.Join("..", "..", "deploy")
	if _, err := os.Stat(deploy); errors.Is(err, fs.ErrNotExist) {
		t.Skip("no deploy/: the installation configs are not part of this tree")
	}
	return filepath.Join(deploy, "truenas", name)
}
