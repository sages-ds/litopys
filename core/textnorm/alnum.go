package textnorm

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Alnum is one letters-and-number name of a query in its other spellings: «win11», «win 11» and «win-11» are
// the same name, but the index keeps «win11» as one identifier and «win 11» as a word and a number.
type Alnum struct {
	Joined []string // the name as one identifier, both spellings (win11, win-11), lower case
	Parts  []string // its letters and its number as the index stems them (win, 11)
	Runs   []string // its letters and its number as typed, lower case (win, 11): what the text of a record shows
}

// AlnumVariants finds the letters-and-number names of a query: a token of exactly one run of letters and one of
// digits, joined or by a hyphen («win11», «rtx-4060»), and two neighbouring tokens that are such runs
// («Win 11»). Each run must be a searchable stem on its own (two characters at least): «x570» and «qwen3» are not
// split, since neither «r» nor «3» is in the index. The analysed form of the query is not changed: these are variants
// the search may try beside it, so the index and textnorm.Version stay as they are.
func AlnumVariants(q string) []Alnum {
	var out []Alnum
	seen := map[string]bool{}
	add := func(a, b string) {
		pa, pb := stemOf(a), stemOf(b)
		if pa == "" || pb == "" || seen[a+b] {
			return
		}
		seen[a+b] = true
		out = append(out, Alnum{Joined: []string{a + b, a + "-" + b}, Parts: []string{pa, pb}, Runs: []string{a, b}})
	}
	// a reference («R-498», «#44») is not a name: Analyze has taken it whole
	ts := tokens(refRe.ReplaceAllString(Normalize(q), " "))
	for i := range ts {
		ts[i] = strings.ToLower(foldMixed(strings.Trim(ts[i], ".:;'")))
	}
	for i, t := range ts {
		if a, b, ok := alnumRuns(t); ok {
			add(a, b)
		}
		if i+1 < len(ts) && runOf(t) != 0 && runOf(ts[i+1]) != 0 && runOf(t) != runOf(ts[i+1]) {
			add(t, ts[i+1])
		}
	}
	return out
}

// alnumRuns splits a token of one run of letters and one of digits, in either order and joined or by one hyphen.
func alnumRuns(t string) (string, string, bool) {
	k := strings.IndexFunc(t, func(r rune) bool { return r == '-' })
	if k < 0 {
		for i, r := range t {
			if i > 0 {
				prev, _ := utf8.DecodeLastRuneInString(t[:i])
				if unicode.IsDigit(prev) != unicode.IsDigit(r) {
					k = i
					break
				}
			}
		}
		if k < 0 {
			return "", "", false
		}
		a, b := t[:k], t[k:]
		return a, b, runOf(a) != 0 && runOf(b) != 0 && runOf(a) != runOf(b)
	}
	a, b := t[:k], t[k+1:]
	return a, b, runOf(a) != 0 && runOf(b) != 0 && runOf(a) != runOf(b)
}

// runOf: 'l' for a token all of letters of one script, 'd' for one all of digits, 0 otherwise.
func runOf(t string) rune {
	if t == "" {
		return 0
	}
	letters, digits := true, true
	for _, r := range t {
		letters = letters && unicode.IsLetter(r)
		digits = digits && unicode.IsDigit(r)
	}
	switch {
	case digits:
		return 'd'
	case letters && foldMixed(t) == t:
		return 'l'
	}
	return 0
}
