package privacy

import (
	"reflect"
	"strings"
	"testing"
)

const longJWT = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"

// Table A of the privacy specification, with the owner's П2 (б) applied to the one-line column. same: the input stays as it is.
const same = "\x00"

var tableA = []struct{ in, text, line string }{
	{"password=hunter2 next", "password=*** next", "password=*** next"},
	{`token: "abc123def"`, `token: "***"`, `token: "***"`},
	{"api_key='k-9'", "api_key='***'", "api_key='***'"},
	{"secret :  s3cr3t;", "secret :  ***;", "secret :  ***;"},
	{"Authorization=xyz, then", "Authorization=***, then", "Authorization=***, then"},
	{"pwd:1", "pwd:***", same}, // a number
	{"Пароль: qwerty, далі", "Пароль: ***, далі", "Пароль: ***, далі"},
	{"новий токен=abc ok", "новий токен=*** ok", "новий токен=*** ok"},
	{"passwordless login", same, same},
	{"паролька: x", same, same},
	{"Bearer token треба передати", same, same},
	{"Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123", "Authorization: Bearer ***", "Authorization: Bearer ***"},
	{"Authorization: Basic dXNlcjpwYXNzd29yZA==", "Authorization: Basic ***", "Authorization: Basic ***"},
	{"authorization: token abc", "authorization: token ***", same},
	{"Authorization: Bearer ,", same, same},
	{"secret=s1,secret2=x;token = Bearer   abcdefghijklmnopqrstu", "secret=***,secret2=x;token = Bearer   ***", "secret=***,secret2=x;token = Bearer   ***"},
	{"postgres://app_user:p4ss@127.0.0.1/db", "postgres://app_user:***@127.0.0.1/db", "postgres://app_user:***@127.0.0.1/db"},
	{"redis://:p4ss@host:6379", "redis://:***@host:6379", "redis://:***@host:6379"},
	{`"token": "abc123secret"`, `"token": "***"`, `"token": "***"`},
	{`{\"password\": \"hunter2\"}`, `{\"password\": \"***\"}`, `{\"password\": \"***\"}`},
	{`'api_key': 'zzz'`, `'api_key': '***'`, `'api_key': '***'`},
	{`token: "abc`, same, same},
	{`token=abc"`, `token=***"`, `token=***"`},
	{"token " + longJWT + " end", "token eyJ*** end", "token eyJ*** end"},
	{"token=${APP_TOKEN}, Bearer <token>, pass", same, same},
	{"TOKEN=$(cat /root/.api-token)", same, same},
	{"TOKEN=*** already", same, same},
	{"password: %DB_PASS%", same, same},
	{"token={{ vault_token }}", same, same},
	{`"tokens": "${X}"`, same, same},
	{"пароль: ${DB_PASS}", same, same},
	// the new cases of §4
	{"GITEA_TOKEN=abc1234", "GITEA_TOKEN=***", "GITEA_TOKEN=***"},
	{"password: hunter2", "password: ***", "password: ***"},
	{"postgres://app:${PGPASSWORD}@db", same, same},
	// connection passwords that only begin like a placeholder
	{"postgres://app:%21Sup3rS3cret@db/x", "postgres://app:***@db/x", "postgres://app:***@db/x"},
	{"redis://:%40abc123def@h:6379", "redis://:***@h:6379", "redis://:***@h:6379"},
	{"postgres://app:$3cr3t!Pass1@db/x", "postgres://app:***@db/x", "postgres://app:***@db/x"},
	{"postgres://app:*Sup3r5ecret@db/x", "postgres://app:***@db/x", "postgres://app:***@db/x"},
	{"postgres://app:{{ db_pass }}@db/x", same, same},
	{"postgres://app:%DB_PASS%@db/x", same, same},
	// a ticket key in a title is in capitals; anything else after a label is a password
	{"password: Admin-2024", "password: ***", "password: ***"},
	{"Пароль: Summer-2024", "Пароль: ***", "Пароль: ***"},
	// a scheme counts only after authorization
	{"password: token hunter2", "password: token ***", "password: token ***"},
	{"secret = digest abc123", "secret = digest ***", "secret = digest ***"},
	// the long value after a scheme, also quoted
	{"authorization: 'token abcdefghijklmnopqrstuv'", "authorization: 'token ***'", "authorization: 'token ***'"},
	// a pair right after another
	{"token='a'password=b", "token='***'password=***", "token='***'password=***"},
	{"password=a\"token=b", "password=***\"token=***", "password=***\"token=***"},
	// П2 (б): what stays in a title because it is plainly not a credential
	{"token: 2026-10-20", "token: ***", same},
	{"api_key: v2.3.10", "api_key: ***", same},
	{"токен: PROJ-1234", "токен: ***", same},
	{"secret: /run/secrets/db1", "secret: ***", same},
	{"password=hunter2 in title", "password=*** in title", "password=*** in title"},
	{"credential: abc", "credential: ***", "credential: ***"},
	{"private_key=abc", "private_key=***", "private_key=***"},
	// the loose form of the label, free text only
	{"пароль від Wi-Fi: Abc12345x", "пароль від Wi-Fi: ***", same},
}

func want(in, w string) string {
	if w == same {
		return in
	}
	return w
}

func TestMaskTableAFreeText(t *testing.T) {
	for _, c := range tableA {
		if got := MaskText(c.in); got != want(c.in, c.text) {
			t.Errorf("MaskText(%q) = %q, want %q", c.in, got, want(c.in, c.text))
		}
	}
}

func TestMaskTableAOneLine(t *testing.T) {
	for _, c := range tableA {
		if got := MaskTitle(c.in); got != want(c.in, c.line) {
			t.Errorf("MaskTitle(%q) = %q, want %q", c.in, got, want(c.in, c.line))
		}
	}
}

func TestMaskingIsIdempotent(t *testing.T) {
	for _, c := range tableA {
		if once := MaskText(c.in); MaskText(once) != once {
			t.Errorf("MaskText twice on %q: %q then %q", c.in, once, MaskText(once))
		}
		if once := MaskTitle(c.in); MaskTitle(once) != once {
			t.Errorf("MaskTitle twice on %q: %q then %q", c.in, once, MaskTitle(once))
		}
	}
}

func TestMaskPrivateBlocks(t *testing.T) {
	for in, w := range map[string]string{
		"keep <private>drop\nme</PRIVATE> and":        "keep  and",
		"a<private>1</private>b<private>2</private>c": "abc",
		"  edges  ":              "edges",
		"open <private>stays":    "open <private>stays",
		"table\n  a  b\n\tc":     "table\n  a  b\n\tc",
		"<private>all</private>": "",
	} {
		if got := StripPrivate(in); got != w {
			t.Errorf("StripPrivate(%q) = %q, want %q", in, got, w)
		}
	}
	if got := NormalizeOneLine("  a \n\t b  "); got != "a b" {
		t.Errorf("NormalizeOneLine: %q", got)
	}
}

func TestMaskStructuredValues(t *testing.T) {
	in := map[string]any{
		"token": "x", "Passwords": []any{"a"}, "private_key": 5.0, "credential": nil, "пароль": "x",
		"note": "password=abc", "n": 1.0, "ok": true, "none": nil,
		"list": []any{"token: abc", map[string]any{"secret": map[string]any{"a": 1.0}}},
	}
	w := map[string]any{
		"token": "***", "Passwords": "***", "private_key": "***", "credential": "***", "пароль": "***",
		"note": "password=***", "n": 1.0, "ok": true, "none": nil,
		"list": []any{"token: ***", map[string]any{"secret": "***"}},
	}
	if got := MaskValue(in); !reflect.DeepEqual(got, w) {
		t.Errorf("MaskValue: %v", got)
	}
}

// Table B of the privacy specification.
func TestDetectRefusal(t *testing.T) {
	long := strings.Repeat("a", 36)
	for in, kind := range map[string]string{
		"-----BEGIN OPENSSH PRIVATE KEY-----":         "private key",
		"postgres://app:%21Sup3rS3cret@db/x":          "connection string with a password",
		"postgres://app:$3cr3t!Pass1@db/x":            "connection string with a password",
		"postgres://app:Sup3rS3cret@db:5432/x":        "connection string with a password",
		"LTP_" + strings.Repeat("A", 43):              "Litopys token",
		"ghp_" + long:                                 "GitHub token",
		"github_pat_" + long:                          "GitHub token",
		"glpat-" + long:                               "GitLab token",
		"xoxb-" + long:                                "Slack token",
		"sk-ant-" + long:                              "Anthropic key",
		"sk-proj-" + long:                             "OpenAI key",
		"AKIAABCDEFGHIJKLMNOP":                        "AWS access key",
		longJWT:                                       "JWT",
		"Authorization: Bearer abcdefghijklmnopqrstu": "bearer token",
		"Пароль: sk-test-0000-FAKE":                   "credential value after a label",
		"пароль=hunter22":                             "credential value after a label",
		"API-ключ: k3y-Abc-12345":                     "credential value after a label",
		"ключ доступу = A1b2C3d4e5":                   "credential value after a label",
		"секрет: 'S3cr3tValue'":                       "credential value after a label",
		"password: hunter22":                          "credential value after a label",
		`{"token": "abc123def456"}`:                   "credential value after a label",
		"export GITEA_TOKEN=a1b2c3d4e5f6":             "credential value after a label",
		"api_key = k9-long-value-1":                   "credential value after a label",
		"Authorization: token 0123456789abcdef0123":   "credential value after a label",
		"credential: abc123def":                       "credential value after a label",
		"пароль від роутера: Abc12345x":               "credential value after a label",
	} {
		if got := DetectSecret(in); got != kind {
			t.Errorf("DetectSecret(%q) = %q, want %q", in, got, kind)
		}
	}
	for _, in := range []string{
		"postgres://app:${PGPASSWORD}@db/x", "mysql://root:<пароль>@host/db", "postgres://u:***@db/x",
		"postgres://u:password@db/x", "postgres://u:xxx@db/x",
		"токен у змінній $GITEA_TOKEN", "ключ AKIA… лежить в OpenBao", "sk-short", "JWT починається з eyJ",
		"звичайний текст про ключі й паролі", "пароль: ${DB_PASS}", "password: <пароль>", "секрет: ***", "token=${APP_TOKEN}",
		"Bearer token треба передати", "токен: оновлено вчора", "токени: 3 шт.", "password: hunter", "token: 2026-10-20",
		"api_key: v2.3.10", "токен: PROJ-1234", "pwd:1", `pwd: C:\Users\user1`, "secret: /run/secrets/db1",
		"токен: https://id.example.org/realm1", "Логін і пароль: `scripts/stand-rebuild.ps1 -Mode backup`",
	} {
		if got := DetectSecret(in); got != "" {
			t.Errorf("DetectSecret(%q) = %q, want none", in, got)
		}
	}
}
