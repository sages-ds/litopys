package migrate

import (
	"errors"
	"testing"
	"testing/fstest"
)

func TestListEmbedded(t *testing.T) {
	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 2 || all[0].Name != "0001_journal.sql" || all[1].Name != "0002_projections.sql" {
		t.Fatalf("unexpected embedded migrations: %+v", names(all))
	}
	for i, m := range all {
		if m.Version != i+1 {
			t.Errorf("gap or disorder at %s: version %d, want %d", m.Name, m.Version, i+1)
		}
		if len(m.Checksum) != 64 {
			t.Errorf("%s: checksum %q is not sha256 hex", m.Name, m.Checksum)
		}
	}
}

func TestListRejectsDuplicateVersion(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_a.sql": {Data: []byte("select 1")},
		"0001_b.sql": {Data: []byte("select 2")},
	}
	if _, err := listFS(fsys); err == nil {
		t.Fatal("duplicate version accepted")
	}
}

func TestListIgnoresNonMigrations(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_a.sql": {Data: []byte("select 1")},
		"embed.go":   {Data: []byte("package x")},
		"README.md":  {Data: []byte("x")},
		"1_bad.sql":  {Data: []byte("x")},
	}
	all, err := listFS(fsys)
	if err != nil || len(all) != 1 {
		t.Fatalf("got %v, %v", names(all), err)
	}
}

func TestChecksumIgnoresLineEndings(t *testing.T) {
	lf, err := listFS(fstest.MapFS{"0001_a.sql": {Data: []byte("select 1;\nselect 2;\n")}})
	if err != nil {
		t.Fatal(err)
	}
	crlf, err := listFS(fstest.MapFS{"0001_a.sql": {Data: []byte("select 1;\r\nselect 2;\r\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if lf[0].Checksum != crlf[0].Checksum {
		t.Fatal("CRLF checkout changes the checksum")
	}
}

func TestVerifyDetectsEditedMigration(t *testing.T) {
	all := []Migration{{Version: 1, Name: "0001_a.sql", Checksum: "new"}}
	if err := verify(all, map[int]string{1: "old"}); !errors.Is(err, ErrChecksum) {
		t.Fatalf("want ErrChecksum, got %v", err)
	}
	if err := verify(all, map[int]string{1: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := verify(all, map[int]string{}); err != nil {
		t.Fatal(err)
	}
}

func names(ms []Migration) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name
	}
	return out
}
