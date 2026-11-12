// Package textnorm turns text into what search indexes and matches: the same function
// runs on a record before it is stored in the search projection and on a query, so both sides always agree.
// The original text is never changed: the journal keeps it, and a new Version rebuilds the projection.
//
//   - Unicode NFKC, lower case, one apostrophe (') and one hyphen (-)
//   - identifiers kept whole and canonical: references (R-498 -> r-498; #44, T-44 -> t-44),
//     hashes, versions, IP[:port], paths and file names, hex and E-codes, snake_case, kebab-case with a digit,
//     dotted names, CamelCase
//   - Cyrillic look-alikes folded to Latin inside tokens that mix both scripts (НР_TOOLS with Cyrillic Н, Р)
//   - stems of words: Cyrillic by the Ukrainian Snowball stemmer, Latin by the English one; the letter parts of an
//     identifier are words too, so "obsidian_sync_couchdb" is also found by "couchdb"
//
// Texts are Ukrainian and English only. THERE IS NO RUSSIAN SUPPORT (owner 2026-10-19).
package textnorm

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"litopys.smart-gw.co.ua/core/textnorm/stem"
)

// Version of the rules. Changing anything that changes Analyze output means a new Version and a rebuild of the
// search projection (it is part of the projector version).
const Version = "3" // 3: the legacy reference forms of a previous service are gone; 2: all-capitals words are words, not identifiers

// MaxToken: a word or identifier longer than this (in characters) is not searchable text — base64, a minified
// blob, a run of one letter — and is left out (PostgreSQL also refuses a lexeme over 2046 bytes).
const MaxToken = 128

// Doc is the analysed form of a text.
type Doc struct {
	Idents  []string // canonical identifiers, sorted, unique
	IdentTF []int    // how many times each of Idents occurs in the text (aligned with Idents)
	// IdentForms: each of Idents as first written in the text (aligned) — "R-364" for r-364. Not stored: a result
	// names the identifier as the query typed it, since a canonical form reads as a distortion.
	IdentForms []string
	Stems      []string // stems of words in text order (repeats kept: frequency is information)
	// WordStems: Stems without those of identifier tokens — what a query says in words, beside the identifiers it
	// names. Search v2 ranks the words channel by these, so an identifier is counted once, in its own tier (x8).
	WordStems []string
	// Words: the word behind each of WordStems, lower-cased as typed (aligned) — what a result says it matched or
	// missed, readable to a person and a model ("cimd", not a stem).
	Words []string
}

var apostrophes = strings.NewReplacer("’", "'", "ʼ", "'", "‘", "'", "`", "'", "´", "'")
var hyphens = strings.NewReplacer("‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-", "−", "-")

// Normalize: NFKC, apostrophes and hyphens unified. Case is kept: CamelCase is an identifier signal.
func Normalize(s string) string {
	return hyphens.Replace(apostrophes.Replace(norm.NFKC.String(s)))
}

// Analyze gives the identifiers and stems of a text.
func Analyze(s string) Doc {
	s = Normalize(s)
	idents := map[string]int{}
	forms := map[string]string{}
	seen := func(id, form string) {
		if idents[id]++; forms[id] == "" {
			forms[id] = form
		}
	}
	var stems, wordStems, wordForms []string
	addWords := func(t string, ident bool) {
		for _, w := range words(t) {
			if st := stemOf(w); st != "" {
				stems = append(stems, st)
				if !ident {
					wordStems, wordForms = append(wordStems, st), append(wordForms, w)
				}
			}
		}
	}
	// references first: they carry a sign ("#44") the token pass would lose
	s = refRe.ReplaceAllStringFunc(s, func(m string) string {
		if id := canonicalRef(m); id != "" {
			seen(id, strings.TrimSpace(m))
			return " "
		}
		return m
	})
	for _, t := range tokens(s) {
		if utf8.RuneCountInString(t) > MaxToken {
			continue // a blob: neither an identifier nor words
		}
		typed := t
		t = foldMixed(t)
		id := identifier(t)
		if id != "" {
			seen(id, strings.Trim(typed, ".:'"))
		}
		addWords(t, id != "")
	}
	out := Doc{Stems: stems, WordStems: wordStems, Words: wordForms}
	for id := range idents {
		out.Idents = append(out.Idents, id)
	}
	sort.Strings(out.Idents)
	for _, id := range out.Idents {
		out.IdentTF = append(out.IdentTF, idents[id])
		out.IdentForms = append(out.IdentForms, forms[id])
	}
	return out
}

// tokens splits on whitespace and on punctuation that never belongs to an identifier.
func tokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		if unicode.IsSpace(r) {
			return true
		}
		switch r {
		case ',', ';', '!', '?', '(', ')', '[', ']', '{', '}', '<', '>', '"', '«', '»', '„', '“', '”', '|', '=', '*', '`':
			return true
		}
		return false
	})
}

// words: the letter-and-digit runs of a token (an apostrophe between letters stays inside: п'ять), lower case.
func words(t string) []string {
	var out []string
	var b strings.Builder
	rs := []rune(t)
	flush := func() {
		if b.Len() > 0 {
			out = append(out, strings.ToLower(b.String()))
			b.Reset()
		}
	}
	for i, r := range rs {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '\'' && i > 0 && i < len(rs)-1 && unicode.IsLetter(rs[i-1]) && unicode.IsLetter(rs[i+1]):
			b.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// stemOf: Cyrillic words by the Ukrainian stemmer, Latin by the English one, numbers as they are; a word mixing
// scripts or shorter than 2 characters is not a word.
func stemOf(w string) string {
	if n := utf8.RuneCountInString(w); n < 2 || n > MaxToken || stopwords[w] {
		return ""
	}
	cyr, lat, digit := false, false, false
	for _, r := range w {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			cyr = true
		case unicode.Is(unicode.Latin, r):
			lat = true
		case unicode.IsDigit(r):
			digit = true
		}
	}
	switch {
	case cyr && !lat && !digit:
		return stem.Ukrainian(w)
	case lat && !cyr && !digit:
		return stem.English(w)
	case !cyr && !lat:
		return w // a number
	}
	return w // letters with digits (e17, p100): kept whole
}
