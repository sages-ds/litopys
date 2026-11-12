package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func TestSumSkipsCommentLines(t *testing.T) {
	base := Sum([]byte("-- header one\nselect 1;\n  -- indented note\nselect 2;\n"))
	same := []string{
		"-- header rewritten, longer than before\nselect 1;\n  -- another note\nselect 2;\n",
		"select 1;\nselect 2;\n",
		"-- header\r\nselect 1;\r\nselect 2;\r\n",
		"\t-- tab-indented\nselect 1;\nselect 2;\n-- last comment without a newline",
	}
	for _, s := range same {
		if got := Sum([]byte(s)); got != base {
			t.Errorf("comment-only edit changed the sum: %q", s)
		}
	}
	differ := []string{
		"-- header one\nselect 1;\nselect 3;\n",         // SQL changed
		"-- header one\nselect 1; -- tail\nselect 2;\n", // comment after SQL on the same line
		"-- header one\nselect 1;\n\nselect 2;\n",       // a blank line is part of the file
		"-- header one\nselect 1;\n   \nselect 2;\n",    // so is a line of spaces
		"-- header one\nselect 1;\nselect 2;",           // the last line without its newline
	}
	for _, s := range differ {
		if got := Sum([]byte(s)); got == base {
			t.Errorf("change not detected: %q", s)
		}
	}
}

// The documented blind spot: a "--" line inside a literal is skipped by Sum although it runs.
func TestSumSkipsDashLineInsideLiteral(t *testing.T) {
	a := Sum([]byte("select '\n-- one\n';\n"))
	b := Sum([]byte("select '\n-- two\n';\n"))
	if a != b {
		t.Fatal("Sum parses literals now: update the doc comment of Sum")
	}
}

func rawSum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestAcceptsLegacyOnlyForUnchangedSQL(t *testing.T) {
	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	m := all[4] // 0005: its header comment was edited by #264
	if m.Version != 5 {
		t.Fatalf("got %s", m.Name)
	}
	if !m.Accepts(m.Checksum) || !m.Accepts(LegacySum(5)) {
		t.Fatal("current or legacy sum refused for the unchanged file")
	}
	if m.Accepts("") || m.Accepts("other") || m.Accepts(LegacySum(6)) {
		t.Fatal("foreign sum accepted")
	}
	if got := m.Recorded(); got != LegacySum(5) {
		t.Fatalf("Recorded = %s, want the legacy sum", got)
	}

	// the same version with its SQL edited: the legacy row must no longer pass, nor be recorded
	edited := Migration{Version: 5, Name: m.Name, SQL: m.SQL + "DROP TABLE everything;\n"}
	edited.Checksum = Sum([]byte(edited.SQL))
	if edited.Accepts(LegacySum(5)) {
		t.Fatal("legacy row accepted for an edited SQL")
	}
	if err := verify([]Migration{edited}, map[int]string{5: LegacySum(5)}); !errors.Is(err, ErrChecksum) {
		t.Fatalf("verify: want ErrChecksum, got %v", err)
	}
	if edited.Recorded() != edited.Checksum {
		t.Fatal("an edited file records the legacy sum")
	}
	if err := verify(all, map[int]string{5: LegacySum(5)}); err != nil {
		t.Fatalf("legacy row of the unchanged file refused by verify: %v", err)
	}
}

func TestAfterManifestRecordsCurrentSum(t *testing.T) {
	m := Migration{Version: 50, Name: "0050_x.sql", SQL: "-- note\nselect 1;\n"}
	m.Checksum = Sum([]byte(m.SQL))
	if m.Recorded() != m.Checksum || m.Accepts(rawSum(m.SQL)) {
		t.Fatal("a migration after 0049 must use the new sum only")
	}
}

// The manifest is frozen at 0049 and describes the embedded files: each of them keeps its original SQL
// (the comment-free sum), and exactly the files #264 and #282 did not touch still have their whole-file sum.
func TestLegacyManifest(t *testing.T) {
	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 49 {
		t.Fatalf("legacy manifest has %d entries, want 49", len(legacy))
	}
	// edited by #264 (comments only; 11: tracker keys, review 1119) and by #282 (comments only: references to
	// the design documents)
	edited := map[int]bool{}
	for _, v := range []int{5, 6, 7, 8, 9, 10, 11, 13, 18, 19, 23, 24, 25, 26, 27, 30, 37, // #264
		1, 2, 3, 4, 14, 15, 16, 17, 20, 21, 22, 28, 29, 31, 32, 33, 34, 35, 36, 38, 39, 40, 41, 42, 43, 45, 46, 47,
		48, 49} { // #282
		edited[v] = true
	}
	for _, m := range all {
		l, ok := legacy[m.Version]
		if want := m.Version <= 49; ok != want {
			t.Errorf("%s: in manifest = %v, want %v", m.Name, ok, want)
			continue
		}
		if !ok {
			continue
		}
		if m.Checksum != l.stripped {
			t.Errorf("%s: SQL differs from the original (comment-free sum changed)", m.Name)
		}
		if unchanged := rawSum(m.SQL) == l.raw; unchanged == edited[m.Version] {
			t.Errorf("%s: whole-file sum matches = %v, edited by #264/#282 = %v", m.Name, unchanged, edited[m.Version])
		}
		if !m.Accepts(l.raw) || m.Recorded() != l.raw {
			t.Errorf("%s: legacy row not accepted or not recorded", m.Name)
		}
	}
}
