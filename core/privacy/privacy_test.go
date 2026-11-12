package privacy

import (
	"reflect"
	"testing"
)

func TestMaskText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"password=hunter2 next", "password=*** next"},
		// a typographic quote closes the value and stays; words between the label and a credential-like value
		{"«password=ab12cd» — далі", "«password=***» — далі"},
		{"пароль від Wi-Fi: Kitchen2026!", "пароль від Wi-Fi: ***"},
		{"password for the router — Kitchen2026!", "password for the router — ***"},
		{"пароль до кабінету — у сейфі на кухні", "пароль до кабінету — у сейфі на кухні"}, // prose stays
		{`token: "abc123def"`, `token: "***"`},
		{"api_key='k-9'", "api_key='***'"},
		{"secret :  s3cr3t;", "secret :  ***;"},
		{"Authorization=xyz, then", "Authorization=***, then"},
		{"passwordless login", "passwordless login"},                                          // no key/value
		{"Bearer token треба передати", "Bearer token треба передати"},                        // prose, not a token
		{"Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123", "Authorization: Bearer ***"}, // scheme kept, token masked (B1)
		{"env APP_STOP_GRACE_MIN=5", "env APP_STOP_GRACE_MIN=5"},                              // a name stays
		{"postgres://ai_memory:p4ss@127.0.0.1/db", "postgres://ai_memory:***@127.0.0.1/db"},
		{"no secrets here", "no secrets here"},
		// Ukrainian labels are masked too; the boundary character before the label stays
		{"Пароль: qwerty, далі", "Пароль: ***, далі"},
		{"новий токен=abc ok", "новий токен=*** ok"},
		{"пароль: ${DB_PASS}", "пароль: ${DB_PASS}"},
		{"паролька: x", "паролька: x"}, // not a label
	}
	for _, c := range cases {
		if got := MaskText(c.in); got != c.want {
			t.Errorf("MaskText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMaskTitle(t *testing.T) {
	if got := MaskTitle("dsn postgres://u:p@h/db"); got != "dsn postgres://u:***@h/db" {
		t.Errorf("title keeps connection password: %q", got)
	}
}

func TestStripPrivate(t *testing.T) {
	in := "keep\n  this <private>drop\nme</PRIVATE> and\tthis  "
	if got := StripPrivate(in); got != "keep\n  this  and\tthis" {
		t.Errorf("StripPrivate = %q", got)
	}
}

func TestMaskValue(t *testing.T) {
	in := map[string]any{"password": "x", "Tokens": "y", "note": "token=abc", "list": []any{"pwd:1"}, "n": 3.0}
	want := map[string]any{"password": "***", "Tokens": "***", "note": "token=***", "list": []any{"pwd:***"}, "n": 3.0}
	if got := MaskValue(in); !reflect.DeepEqual(got, want) {
		t.Errorf("MaskValue = %v", got)
	}
}

func TestMaskJWT(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	if got := MaskText("token " + jwt + " end"); got != "token eyJ*** end" {
		t.Errorf("MaskText jwt: %q", got)
	}
	if got := MaskTitle("jwt " + jwt); got != "jwt eyJ***" {
		t.Errorf("MaskTitle jwt: %q", got)
	}
}

func TestDetectSecret(t *testing.T) {
	refused := map[string]string{
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk=":       "private key",
		"key: -----BEGIN RSA PRIVATE KEY-----":                        "private key",
		"postgres://app:Sup3rS3cret@db:5432/x":                        "connection string with a password",
		"mongodb+srv://u:hunter22@cluster0.example.net/db":            "connection string with a password",
		"LTP_" + "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG":        "Litopys token",
		"ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789":               "GitHub token",
		"github_pat_" + "11ABCDEFG0123456789_abcdefghijklmnop":        "GitHub token",
		"glpat-" + "abcdefghij0123456789":                             "GitLab token",
		"xoxb-" + "1234567890-abcdefghij":                             "Slack token",
		"sk-ant-" + "api03-abcdefghijklmnopqrstuvwxyz":                "Anthropic key",
		"sk-proj-" + "abcdefghijklmnopqrstuvwxyz":                     "OpenAI key",
		"AKIA" + "ABCDEFGHIJKLMNOP":                                   "AWS access key",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4": "JWT",
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123":        "bearer token",
		// a label with a credential-like value, English and Ukrainian, any write verb
		"Пароль: sk-test-0000-FAKE":                 "credential value after a label",
		"пароль від Wi-Fi: Kitchen2026!":            "credential value after a label",
		"пароль до кабінету — Kitchen2026!":         "credential value after a label",
		"пароль=hunter22":                           "credential value after a label",
		"API-ключ: k3y-Abc-12345":                   "credential value after a label",
		"ключ доступу = A1b2C3d4e5":                 "credential value after a label",
		"секрет: 'S3cr3tValue'":                     "credential value after a label",
		"password: hunter22":                        "credential value after a label",
		`{"token": "abc123def456"}`:                 "credential value after a label",
		"export GITEA_TOKEN=a1b2c3d4e5f6":           "credential value after a label",
		"api_key = k9-long-value-1":                 "credential value after a label",
		"Authorization: token 0123456789abcdef0123": "credential value after a label",
	}
	for in, want := range refused {
		if got := DetectSecret(in); got != want {
			t.Errorf("DetectSecret(%q) = %q, want %q", in, got, want)
		}
	}
	allowed := []string{
		"postgres://app:${PGPASSWORD}@db/x", "postgres://app:$PGPASSWORD@db/x", "mysql://root:<пароль>@host/db",
		"postgres://u:***@db/x", "env APP_STOP_GRACE_MIN=5", "токен у змінній $GITEA_TOKEN",
		"ключ AKIA… лежить в OpenBao", "sk-short", "JWT починається з eyJ", "звичайний текст про ключі й паролі",
		// labels whose value is not a credential (the check must not refuse prose, references and placeholders)
		"пароль: ${DB_PASS}", "token=${APP_TOKEN}", "токен: оновлено вчора", "password: <пароль>", "секрет: ***",
		"token: 2026-10-20", "api_key: v2.3.10", "токен: PROJ-1234", "pwd: C:\\Users\\user1", "secret: /run/secrets/db1",
		"токен: https://id.example.org/realm1", "pwd:1", "TOKEN=$(cat /root/.semaphore-api-token)", "Bearer token треба передати",
		"токени: 3 шт.", "password: hunter", "Логін і пароль: `scripts/stand-rebuild.ps1 -Mode backup`",
	}
	for _, in := range allowed {
		if got := DetectSecret(in); got != "" {
			t.Errorf("DetectSecret(%q) = %q, want none", in, got)
		}
	}
}
