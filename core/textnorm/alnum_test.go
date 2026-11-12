package textnorm

import (
	"reflect"
	"testing"
)

// TestAlnumVariants: a name of letters and a number in its three spellings, and what is not one.
func TestAlnumVariants(t *testing.T) {
	win := []Alnum{{Joined: []string{"win11", "win-11"}, Parts: []string{"win", "11"}, Runs: []string{"win", "11"}}}
	for in, want := range map[string][]Alnum{
		"win11":                    win,
		"Win 11":                   win,
		"WIN-11":                   win,
		"оновлення Win11 і Win 11": win, // one name, once
		"RTX4060 ноутбук":          {{Joined: []string{"rtx4060", "rtx-4060"}, Parts: []string{"rtx", "4060"}, Runs: []string{"rtx", "4060"}}},
		"SQL Server 2019":          {{Joined: []string{"server2019", "server-2019"}, Parts: []string{"server", "2019"}, Runs: []string{"server", "2019"}}},
		// one-character runs are not in the index; a third run, a version, a reference, a stopword: no name
		"2гому":         nil,
		"x570":          nil,
		"qwen3":         nil,
		"25H2":          nil,
		"Keycloak 26.8": nil,
		"R-498":         nil,
		"#44 і 45":      nil,
		"в 2026":        nil,
		"hp_tools":      nil,
	} {
		if got := AlnumVariants(in); !reflect.DeepEqual(got, want) {
			t.Errorf("AlnumVariants(%q) = %v, want %v", in, got, want)
		}
	}
}
