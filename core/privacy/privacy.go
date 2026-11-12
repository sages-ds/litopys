// Package privacy keeps credentials out of what is written: private blocks are cut, high-precision secrets refuse the
// whole command, the rest is masked. Section numbers below refer to the privacy specification. This is best effort,
// not a guarantee (§3.8 lists what is deliberately not caught).
package privacy

import (
	"regexp"
	"strings"
)

// §3.1: the one dictionary of labels, for free text, one-line fields, JSON keys and refusal.
const (
	labelsEN = `password|passwd|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|authorization|credential`
	labelsUK = `пароль|паролі|пароля|токен|токени|токена|секрет|секрети|(?:api|апі)[-\s]?ключ|ключ\s+(?:api|апі|доступу)`
	labels   = labelsEN + `|` + labelsUK
	// A label stands alone or as the tail of a name (GITEA_TOKEN): anything but a letter or a digit before it.
	labelStart = `(^|[^\p{L}\p{N}])`
	authScheme = `(?:bearer|basic|token|digest)`
	// A value ends at whitespace, a quote, a comma or a semicolon.
	valueChars = `[^\s"',;«»“”„]+`
	tokenChars = `[a-z0-9._~+/=-]`
)

var (
	privateBlock = regexp.MustCompile(`(?is)<private>.*?</private>`)
	whitespace   = regexp.MustCompile(`\s+`)

	jwt = regexp.MustCompile(`\beyJ[\w-]{10,}\.[\w-]{10,}\.[\w-]{10,}`)
	// label, separator, optional opening quote, optional scheme, value, optional closing quote
	labelled = regexp.MustCompile(`(?i)` + labelStart + `(` + labels + `)(\s*[:=]\s*)(["']?)(` + authScheme + `\s+)?(` + valueChars + `)(["']?)`)
	// a label, a few words, then the separator: "the password for the router - X". Only a credential-like value counts.
	labelledLoosely = regexp.MustCompile(`(?i)` + labelStart + `((?:` + labels + `)(?:\s+[^\s:=—–]{1,20}){1,4}\s*[:=—–]\s*["'«“]?)(` + valueChars + `)`)
	// quoted keys of JSON and Python dictionaries, also escaped, with a possible plural
	quotedKey = regexp.MustCompile(`(?i)(\\?["'])(` + labels + `)(s?)(\\?["'])(\s*:\s*)(\\?["'])(` + authScheme + `\s+)?([^"'\\\n]+)(\\?["'])`)
	// the form refusal reads: a label possibly plural or quoted, then the value
	labelledForRefusal = regexp.MustCompile(`(?i)` + labelStart + `(?:` + labels + `)s?\\?["']?\s*[:=]\s*\\?["']?(?:` + authScheme + `\s+)?(` + valueChars + `)`)
	schemeWord         = regexp.MustCompile(`(?i)^` + authScheme + `$`)

	bearer     = regexp.MustCompile(`(?i)\b(bearer\s+)(` + tokenChars + `{20,})`)
	authHeader = regexp.MustCompile(`(?i)\b(authorization\s*[:=]\s*["']?(?:basic|token|digest)\s+)(` + tokenChars + `{16,})`)
	connection = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s:/@]*):([^\s@]+)@`)

	credentialKey = regexp.MustCompile(`(?i)^(?:` + labels + `)s?$`)

	// §3.4: a stand-in password of a connection string
	examplePassword = regexp.MustCompile(`^(?:x+|\.\.\.|password|pass|pwd)$`)
	// a whole placeholder as a connection password: here a leading % or $ is often a real one (%21Sup3r is URL-encoded)
	connectionPlaceholder = regexp.MustCompile(`^(?:\$\{[A-Za-z_]\w*\}|\$[A-Za-z_]\w*|\$\(.*\)|<[^>]*>|\*+|%[A-Za-z_]\w*%|%[a-z]|\{\{.*\}\})$`)
	// §3.4: numbers and times, dates, versions, ticket keys, paths and URLs
	notCredential = regexp.MustCompile(`^(?:[0-9.:_+-]+|v?\d+(?:\.\d+)+\S*|[A-Za-z][A-Za-z0-9]*-\d+|[~.].*|.*[/\\].*)$`)
	// the same in a title, where a ticket key is in capitals only: Admin-2024 after a label is a password (П2)
	notCredentialInTitle = regexp.MustCompile(`^(?:[0-9.:_+-]+|v?\d+(?:\.\d+)+\S*|[A-Z][A-Z0-9]+-\d+|[~.].*|.*[/\\].*)$`)
	hasLetter            = regexp.MustCompile(`\pL`)
	hasDigit             = regexp.MustCompile(`\d`)
)

type format struct {
	kind string
	re   *regexp.Regexp
}

// refusedFormats is the head of the table in §3.4, in order: the first match names the kind.
var refusedFormats = []format{
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
	{"Litopys token", regexp.MustCompile(`\bLTP_[A-Za-z0-9_-]{43}\b`)},
	{"GitHub token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})\b`)},
	{"GitLab token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[bpas]-[A-Za-z0-9-]{10,}\b`)},
	{"Anthropic key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`)},
	{"OpenAI key", regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}\b`)},
	{"AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"JWT", jwt},
	{"bearer token", bearer},
}

// StripPrivate cuts private blocks (§3.3) and trims the edges; nothing else changes, so code and tables survive.
func StripPrivate(s string) string { return strings.TrimSpace(privateBlock.ReplaceAllString(s, "")) }

// NormalizeOneLine collapses whitespace in a one-line field (§3.2 step 2).
func NormalizeOneLine(s string) string { return strings.TrimSpace(whitespace.ReplaceAllString(s, " ")) }

// isPlaceholder: ${VAR}, $VAR, $(cmd), <name>, %VAR%, ***, {{ var }} are examples, never secrets (§3.1).
func isPlaceholder(v string) bool { return v != "" && strings.ContainsRune("$<%*{", rune(v[0])) }

// trimmedValue drops what wraps a value in prose: closing punctuation and markdown marks.
func trimmedValue(v string) string { return strings.Trim(strings.TrimRight(v, ".)]}"), "`*") }

// credentialLike is the test of refusal (§3.4): 8 symbols or more, a letter and a digit, not an obvious non-secret.
func credentialLike(v string) bool {
	v = trimmedValue(v)
	if len([]rune(v)) < 8 || isPlaceholder(v) || notCredential.MatchString(v) {
		return false
	}
	return hasLetter.MatchString(v) && hasDigit.MatchString(v)
}

// schemeWithoutSecret: "Authorization: Bearer ," names the scheme, the word itself is not the value (§3.5 step 2).
func schemeWithoutSecret(scheme, value, after string) bool {
	return scheme == "" && schemeWord.MatchString(value) && after != "" && strings.ContainsAny(after[:1], " \t\n\r")
}

// replaceWith is ReplaceAllStringFunc with groups and the text after the match; f returns false to keep the match.
// Each search starts where the last match ended, so ^ of labelStart holds there: a match ends at a delimiter or takes a
// closing quote, and the pair right after it (token='a'password=b) still has its boundary.
func replaceWith(re *regexp.Regexp, s string, f func(g []string, after string) (string, bool)) string {
	var b strings.Builder
	for len(s) > 0 {
		idx := re.FindStringSubmatchIndex(s)
		if idx == nil || idx[1] == 0 {
			break
		}
		g := make([]string, len(idx)/2)
		for i := range g {
			if idx[2*i] >= 0 {
				g[i] = s[idx[2*i]:idx[2*i+1]]
			}
		}
		b.WriteString(s[:idx[0]])
		if out, ok := f(g, s[idx[1]:]); ok {
			b.WriteString(out)
		} else {
			b.WriteString(g[0])
		}
		s = s[idx[1]:]
	}
	b.WriteString(s)
	return b.String()
}

// maskLabelled replaces the value of label=value. Quotes count only as a pair. keep decides what stays by the value.
func maskLabelled(s string, keep func(label, scheme, value string) bool) string {
	return replaceWith(labelled, s, func(g []string, after string) (string, bool) {
		start, label, sep, open, scheme, value, closing := g[1], g[2], g[3], g[4], g[5], g[6], g[7]
		if open != "" && closing != open {
			return "", false
		}
		if isPlaceholder(value) || schemeWithoutSecret(scheme, value, closing+after) || keep(label, scheme, value) {
			return "", false
		}
		return start + label + sep + open + scheme + "***" + closing, true
	})
}

// maskQuotedKeys replaces the value of "key": "value"; both pairs of quotes must match.
func maskQuotedKeys(s string, keep func(label, scheme, value string) bool) string {
	return replaceWith(quotedKey, s, func(g []string, after string) (string, bool) {
		q1, key, plural, q2, sep, q3, scheme, value, q4 := g[1], g[2], g[3], g[4], g[5], g[6], g[7], g[8], g[9]
		if q1 != q2 || q3 != q4 || isPlaceholder(value) || schemeWithoutSecret(scheme, value, after) || keep(key, scheme, value) {
			return "", false
		}
		return q1 + key + plural + q1 + sep + q3 + scheme + "***" + q3, true
	})
}

func maskLoosely(s string) string {
	return replaceWith(labelledLoosely, s, func(g []string, _ string) (string, bool) {
		if !credentialLike(g[3]) {
			return "", false
		}
		return g[1] + g[2] + "***", true
	})
}

// maskConnections hides the password of scheme://user:password@, whole placeholders stay.
func maskConnections(s string) string {
	return replaceWith(connection, s, func(g []string, _ string) (string, bool) {
		if connectionPlaceholder.MatchString(g[2]) {
			return "", false
		}
		return g[1] + ":***@", true
	})
}

func keepNothing(string, string, string) bool { return false }

// MaskText masks free text (§3.5): length and form of a labelled value do not matter.
func MaskText(s string) string {
	s = jwt.ReplaceAllString(s, "eyJ***")
	s = maskLabelled(s, keepNothing)
	s = maskLoosely(s)
	s = maskQuotedKeys(s, keepNothing)
	s = bearer.ReplaceAllString(s, "${1}***")
	return maskConnections(s)
}

// MaskTitle masks a one-line field (§3.6, owner's П2 б). A title is the main search key, so a labelled value stays when
// it is plainly not a credential; "Authorization: <scheme> <value>" counts only when long enough for the rules below.
func MaskTitle(s string) string {
	keep := func(label, scheme, value string) bool {
		if scheme != "" && strings.EqualFold(label, "authorization") {
			return true
		}
		return notCredentialInTitle.MatchString(trimmedValue(value))
	}
	s = jwt.ReplaceAllString(s, "eyJ***")
	s = maskConnections(s)
	s = bearer.ReplaceAllString(s, "${1}***")
	s = authHeader.ReplaceAllString(s, "${1}***")
	s = maskLabelled(s, keep)
	return maskQuotedKeys(s, keep)
}

// MaskValue masks decoded JSON (§3.7): a value under a credential key becomes "***" whatever its type.
func MaskValue(v any) any {
	switch x := v.(type) {
	case string:
		return MaskText(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = MaskValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if credentialKey.MatchString(k) {
				out[k] = "***"
			} else {
				out[k] = MaskValue(e)
			}
		}
		return out
	default:
		return v
	}
}

// DetectSecret names the kind of the first secret that refuses the command (§3.4), "" when none. The secret itself is
// never returned: callers report the kind only.
func DetectSecret(s string) string {
	for _, f := range refusedFormats {
		if f.re.MatchString(s) {
			return f.kind
		}
	}
	for _, m := range connection.FindAllStringSubmatch(s, -1) {
		if !connectionPlaceholder.MatchString(m[2]) && !examplePassword.MatchString(strings.ToLower(m[2])) {
			return "connection string with a password"
		}
	}
	for _, m := range labelledForRefusal.FindAllStringSubmatch(s, -1) {
		if credentialLike(m[2]) {
			return "credential value after a label"
		}
	}
	for _, m := range labelledLoosely.FindAllStringSubmatch(s, -1) {
		if credentialLike(m[3]) {
			return "credential value after a label"
		}
	}
	return ""
}
