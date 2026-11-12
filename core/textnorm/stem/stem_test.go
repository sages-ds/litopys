package stem

import (
	"bufio"
	"os"
	"path/filepath"
	"testing"
)

// TestReference: the Go stemmers give exactly the reference stems of snowball-data (voc.txt -> output.txt). The
// word lists are CC BY-SA and are not committed: STEM_DATA names a directory with <language>/voc.txt and
// output.txt (the stemmers workflow downloads them at a pinned commit). Without it the test is skipped.
func TestReference(t *testing.T) {
	dir := os.Getenv("STEM_DATA")
	if dir == "" {
		t.Skip("STEM_DATA is not set (the stemmers workflow sets it)")
	}
	for lang, fn := range map[string]func(string) string{"ukrainian": Ukrainian, "english": English} {
		voc := lines(t, filepath.Join(dir, lang, "voc.txt"))
		out := lines(t, filepath.Join(dir, lang, "output.txt"))
		if len(voc) != len(out) || len(voc) == 0 {
			t.Fatalf("%s: %d words, %d stems", lang, len(voc), len(out))
		}
		bad := 0
		for i, w := range voc {
			if got := fn(w); got != out[i] {
				if bad < 10 {
					t.Errorf("%s: %q -> %q, reference %q", lang, w, got, out[i])
				}
				bad++
			}
		}
		t.Logf("%s: %d words, %d differ from the reference", lang, len(voc), bad)
		if bad > 0 {
			t.Fail()
		}
	}
}

// TestSmoke: a few words without the reference data, so the package is exercised by every test run.
func TestSmoke(t *testing.T) {
	for _, c := range []struct {
		fn   func(string) string
		a, b string
	}{
		{Ukrainian, "міграції", "міграція"},
		{Ukrainian, "квартири", "квартира"},
		{English, "migrations", "migration"},
		{English, "running", "runs"},
	} {
		if x, y := c.fn(c.a), c.fn(c.b); x != y {
			t.Errorf("%q -> %q and %q -> %q: one stem expected", c.a, x, c.b, y)
		}
	}
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		out = append(out, s.Text())
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
