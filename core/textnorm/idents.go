package textnorm

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// References to records and tasks: the canonical forms are r-<n> and t-<n>.
//
//	records: R-498; r498 is NOT one (too loose). The previous service's forms are rewritten to R-N by its import,
//	         so the core knows none of them
//	tasks:   #44, T-44, todo:<project>#44 (the project part is a link concern, here the number only)
var refRe = regexp.MustCompile(`(?i)(?:\bR-\d+\b|(?:\btodo:[\p{L}\p{N}_\-]+)?#\d+\b|\bT-\d+\b)`)

var digitsRe = regexp.MustCompile(`\d+`)

func canonicalRef(m string) string {
	n := digitsRe.FindString(m)
	if n == "" {
		return ""
	}
	n = strings.TrimLeft(n, "0")
	if n == "" {
		n = "0"
	}
	l := strings.ToLower(m)
	if strings.Contains(l, "#") || strings.HasPrefix(l, "t-") {
		return "t-" + n
	}
	return "r-" + n
}

var (
	hashRe    = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	versionRe = regexp.MustCompile(`^v?\d+(\.\d+)+[a-z0-9.\-]*$`)
	ipRe      = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}(:\d{1,5})?(/\d{1,2})?$`)
	hexCodeRe = regexp.MustCompile(`^0x[0-9a-f]+$`)
	eCodeRe   = regexp.MustCompile(`^e\d+$`)
	pathRe    = regexp.MustCompile(`^(~?/|[a-z]:\\|\.{1,2}/|\\\\)\S+|^[\p{L}\p{N}_\-.]+\.[a-z0-9]{1,6}$`)
	numberRe  = regexp.MustCompile(`^\d{2,}$`)
)

// identifier returns the canonical form of a token that is an identifier, or "".
func identifier(t string) string {
	t = strings.Trim(t, ".:;'-")
	if t == "" || utf8.RuneCountInString(t) > MaxToken {
		return ""
	}
	camel := innerUpper(t)
	l := strings.ToLower(t)
	hasDigit := strings.IndexFunc(l, unicode.IsDigit) >= 0
	hasLetter := strings.IndexFunc(l, unicode.IsLetter) >= 0
	switch {
	case ipRe.MatchString(l), hexCodeRe.MatchString(l), eCodeRe.MatchString(l), versionRe.MatchString(l):
		return l
	case hashRe.MatchString(l) && hasDigit && strings.IndexFunc(l, func(r rune) bool { return r >= 'a' && r <= 'f' }) >= 0:
		return l
	case pathRe.MatchString(l):
		return l
	case strings.Contains(l, "_") && hasLetter:
		return l
	case strings.Contains(l, "-") && hasDigit && hasLetter:
		return l
	case strings.Count(l, ".") >= 1 && hasLetter && !strings.HasSuffix(l, "."):
		return l // dotted names: dev.example.org, litopys.smart-gw.co.ua
	case camel:
		return l
	case numberRe.MatchString(l):
		return l // ports, years, build numbers
	case hasDigit && hasLetter:
		return l // p100, x570, e2e
	}
	return ""
}

// innerUpper: mixed case with a capital after the first letter (CamelCase: CouchDB, BitLocker) — read before
// lower-casing. An all-capitals word (ОСББ, CIMD, HTTP) is an abbreviation, a word: as an identifier it put every
// record that merely mentions it above the words of the query (x4, incident golden-1, 2026-10-19).
func innerUpper(t string) bool {
	first, inner, lower := true, false, false
	for _, r := range t {
		if !unicode.IsLetter(r) {
			continue
		}
		if !first && unicode.IsUpper(r) {
			inner = true
		}
		if unicode.IsLower(r) {
			lower = true
		}
		first = false
	}
	return inner && lower
}
