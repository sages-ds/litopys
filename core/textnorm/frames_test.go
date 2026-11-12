package textnorm

import (
	"reflect"
	"testing"
)

// TestStripFrame: metamorphic — a query with a frame analyses as the same query without it (K1); words of the topic
// stay, even when they are frame words inside the query; a query that is only a frame stays.
func TestStripFrame(t *testing.T) {
	for framed, bare := range map[string]string{
		"Нагадай який порт відповідає за підключення віфі":   "який порт відповідає за підключення віфі",
		"Що вирішили щодо використання тріграм у літопис?":   "використання тріграм у літопис?",
		"нагадай мені що ми вирішили про корпоративну пошту": "корпоративну пошту",
		"Що вийшло в підсумку по налаштуваннях роутера":      "налаштуваннях роутера",
		"що не вийшло з Codex і CIMD":                        "codex і cimd",
		"what did we decide about the gate":                  "the gate",
		"remind me of the X570 benchmark":                    "the x570 benchmark",
		"Нагадай FastPool пул":                               "FastPool пул", // the case of an identifier survives
	} {
		rest, frame := StripFrame(framed)
		if frame == "" {
			t.Errorf("%q: no frame found", framed)
		}
		if a, b := Analyze(rest), Analyze(bare); !reflect.DeepEqual(a.WordStems, b.WordStems) || !reflect.DeepEqual(a.Idents, b.Idents) {
			t.Errorf("%q → %q: analyses as %v, want %v (of %q)", framed, rest, a.WordStems, b.WordStems, bare)
		}
	}
	for _, q := range []string{"рішення щодо трігерів", "коли вирішили переїжджати", "find_user function", "вирішили"} {
		if rest, frame := StripFrame(q); frame != "" && q != "find_user function" {
			t.Errorf("%q: frame %q taken, rest %q — a topic word inside or the whole query", q, frame, rest)
		}
	}
}
