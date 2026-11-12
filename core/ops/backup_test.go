package ops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCompareHeads: the copy may be ahead of the heads read before the dump, never behind; a
// tenant missing from the copy and every finding of verify_report fail the check.
func TestCompareHeads(t *testing.T) {
	live := map[string]int64{"default": 10, "acme": 5}
	if f := CompareHeads(live, []CopyTenant{{Tenant: "default", JournalHead: 12}, {Tenant: "acme", JournalHead: 5}}); len(f) != 0 {
		t.Fatalf("a copy ahead or equal is sound: %v", f)
	}
	f := CompareHeads(live, []CopyTenant{{Tenant: "default", JournalHead: 9, Findings: []string{"projector lags"}}})
	want := []string{"default: projector lags", "default: copy head 9 is behind the live head 10", "acme: missing in the copy"}
	if strings.Join(f, "|") != strings.Join(want, "|") {
		t.Fatalf("findings %q, want %q", f, want)
	}
	if f := CompareHeads(nil, []CopyTenant{{Tenant: "default", JournalHead: 1}}); len(f) != 0 {
		t.Fatalf("without live heads only verify_report counts: %v", f)
	}
	if f := CompareHeads(nil, nil); len(f) != 1 {
		t.Fatalf("an empty copy must be a finding even without a record: %v", f)
	}
}

// TestSplitDSN: the password leaves the URL for PGPASSWORD of the child; the database can be swapped.
func TestSplitDSN(t *testing.T) {
	c, err := SplitDSN("postgres://litopys_backup:p%40ss@db.example:5433/litopys?sslmode=disable", "litopys_verify_1")
	if err != nil {
		t.Fatal(err)
	}
	if c.Password != "p@ss" || strings.Contains(c.URL, "p%40ss") || c.URL != "postgres://litopys_backup@db.example:5433/litopys_verify_1?sslmode=disable" {
		t.Fatalf("%+v", c)
	}
	if _, err := SplitDSN("host=x dbname=y", ""); err == nil {
		t.Error("a keyword DSN must be refused (the password could not be moved out)")
	}
	if n, err := DBName("postgres://u@h/litopys"); err != nil || n != "litopys" {
		t.Fatalf("%q %v", n, err)
	}
	if _, err := DBName("postgres://u@h/lit;drop"); err == nil {
		t.Error("a database name that is not a plain identifier must be refused")
	}
}

// TestBackupPrune: dumps older than keep_days go with their records, the newest stays whatever its age, an
// unfinished dump goes after ten hours; the record beside the newest is what doctor reads.
func TestBackupPrune(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 31, 3, 15, 0, 0, time.UTC)
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := mk("litopys_20261010-031500.dump", 21*24*time.Hour)
	mk("litopys_20261010-031500.dump.json", 21*24*time.Hour)
	recent := mk("litopys_20261030-031500.dump", 24*time.Hour)
	part := mk("litopys_20261030-120000.dump.part", 15*time.Hour)
	b := &Backuper{Dir: dir, KeepDays: 14, Now: func() time.Time { return now }}
	b.defaults()
	b.prune()
	for p, want := range map[string]bool{old: false, old + ".json": false, recent: true, part: false} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(p), err == nil, want)
		}
	}
	// only one dump, older than keep_days: it stays (never prune the last backup)
	b.Now = func() time.Time { return now.Add(30 * 24 * time.Hour) }
	b.prune()
	if _, err := os.Stat(recent); err != nil {
		t.Fatal("the newest dump was pruned")
	}
	// doctor's view of the record
	rec := &BackupRecord{Dump: filepath.Base(recent), OK: false, Findings: []string{"default: missing in the copy"}}
	if err := writeRecord(recent, rec); err != nil {
		t.Fatal(err)
	}
	if got := newestBackup(dir); !got.Checked || got.Verified || len(got.Findings) != 1 {
		t.Fatalf("newestBackup %+v", got)
	}
}
