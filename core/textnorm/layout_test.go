package textnorm

import "testing"

// TestSwapLayout: both directions of a layout slip, and what is not one.
func TestSwapLayout(t *testing.T) {
	for in, want := range map[string]string{
		// Latin keys meant Ukrainian
		"ghjtrn":   "проект",
		"vsuhfwsz": "міграція",
		"lfirf":    "дашка",
		// Cyrillic keys meant English
		"щзутифщ":  "openbao",
		"екгутфі":  "truenas",
		"лунсдщфл": "keycloak",
		"проект":   "ghjtrn",
		// digits, signs, mixed scripts, a letter of no Ukrainian key: as they are
		"e17":      "e17",
		"hp_tools": "hp_tools",
		"abcї":     "abcї",
		"ыё":       "ыё",
	} {
		if got := SwapLayout(in); got != want {
			t.Errorf("SwapLayout(%q) = %q, want %q", in, got, want)
		}
	}
}
