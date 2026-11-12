package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode"
)

// TestModelTextsInEnglish (owner decision 2026-11-03): every text a model reads from the generated surfaces is
// English. Allowed: the uk labels for people (title.uk) and the guide's example of a Ukrainian credential label.
func TestModelTextsInEnglish(t *testing.T) {
	allowed := func(path, s string) bool {
		return strings.HasSuffix(path, ".uk") || strings.Contains(s, `"password"/"`)
	}
	for _, file := range []string{"gen/surface.json", "gen/guide.json", "../vocab/gen/vocabularies.json",
		"../linktypes/gen/link_types.json"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		var walk func(path string, v any)
		walk = func(path string, v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, e := range x {
					walk(path+"."+k, e)
				}
			case []any:
				for _, e := range x {
					walk(path+"[]", e)
				}
			case string:
				for _, line := range strings.Split(x, "\n") {
					if strings.IndexFunc(line, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) >= 0 && !allowed(path, line) {
						t.Errorf("%s %s: not English: %.100s", file, path, line)
					}
				}
			}
		}
		walk("", doc)
	}
}
