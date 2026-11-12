package textnorm

import (
	"slices"
	"strings"
	"testing"
)

func has(t *testing.T, what string, list []string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !slices.Contains(list, w) {
			t.Errorf("%s: %q missing in %q", what, w, list)
		}
	}
}

func hasNot(t *testing.T, what string, list []string, not ...string) {
	t.Helper()
	for _, w := range not {
		if slices.Contains(list, w) {
			t.Errorf("%s: %q must not be in %q", what, w, list)
		}
	}
}

// TestReferences: every written form of a record or task reference has one canonical identifier, and a number in
// prose ("44 події") is not a reference.
func TestReferences(t *testing.T) {
	for in, want := range map[string]string{
		"див. R-498": "r-498", "(r-234)": "r-234", "R-0498": "r-498",
		"закрито #44": "t-44", "T-44": "t-44", "todo:infra-repo#8": "t-8",
	} {
		has(t, in, Analyze(in).Idents, want)
	}
	// forms that are not R-N are no references (an importer rewrites its source's forms to R-N); the near misses
	// guard the bounds of the R-N rule
	for _, in := range []string{"rec 498", "rec:498", "rec-498", "litopys:rec:498", "r498", "xr-498", "R-498a"} {
		hasNot(t, in, Analyze(in).Idents, "r-498")
	}
	d := Analyze("44 події, 31 файл")
	hasNot(t, "prose numbers", d.Idents, "t-44", "t-31", "r-44")
}

func TestIdentifiers(t *testing.T) {
	d := Analyze(`Контур піднято до 3b80ed5; RouterOS 7.22.3 на 192.0.2.15:8200, код 0x80070002, E17 зелений.
		Файли /etc/semaphore/config.json і deploy-facade.ps1, таблиця search_tsv, апка lxc-102, хост dev.example.org,
		заголовок Mcp-Session-Id, HP_TOOLS, CouchDB, порт 8798.`)
	has(t, "idents", d.Idents, "3b80ed5", "7.22.3", "192.0.2.15:8200", "0x80070002", "e17", "/etc/semaphore/config.json",
		"deploy-facade.ps1", "search_tsv", "lxc-102", "dev.example.org", "mcp-session-id", "hp_tools", "couchdb", "8798")
	hasNot(t, "idents", d.Idents, "контур", "піднято", "файли")
}

// TestAbbreviations: an all-capitals word is a word, not an identifier (incident golden-1: ОСББ as an identifier
// ranked every record mentioning it above the words of the query); CamelCase stays an identifier.
func TestAbbreviations(t *testing.T) {
	d := Analyze("ОСББ ліфти CIMD HTTP")
	hasNot(t, "abbreviations", d.Idents, "осбб", "cimd", "http")
	has(t, "abbreviations", d.Stems, "осбб")
	has(t, "camel", Analyze("CouchDB BitLocker").Idents, "couchdb", "bitlocker")
}

// TestHomoglyphs: a code typed with Cyrillic look-alikes is the same identifier; a Ukrainian word is untouched.
func TestHomoglyphs(t *testing.T) {
	cyr := Analyze("НР_TOOLS") // Н and Р are Cyrillic
	has(t, "folded", cyr.Idents, "hp_tools")
	word := Analyze("Нарада")
	if len(word.Idents) != 0 || len(word.Stems) != 1 {
		t.Errorf("a Ukrainian word must stay a word: %+v", word)
	}
}

// TestStems: word forms meet on one stem in both languages; parts of an identifier are words too.
func TestStems(t *testing.T) {
	a, b := Analyze("міграції пулу"), Analyze("міграція пул")
	if !slices.Equal(a.Stems, b.Stems) {
		t.Errorf("uk forms: %q vs %q", a.Stems, b.Stems)
	}
	c, d := Analyze("failed upgrades"), Analyze("failing upgrade")
	if c.Stems[1] != d.Stems[1] {
		t.Errorf("en forms: %q vs %q", c.Stems, d.Stems)
	}
	e := Analyze("obsidian_sync_couchdb")
	has(t, "parts", e.Stems, "obsidian", "sync", "couchdb")
	f := Analyze("п'ять квартир")
	if len(f.Stems) != 2 {
		t.Errorf("an apostrophe inside a word keeps it one word: %q", f.Stems)
	}
}

// TestQueryAndRecordAgree: the same function on both sides — a query finds what the record was indexed as.
func TestQueryAndRecordAgree(t *testing.T) {
	rec := Analyze("Розбір R-498: чому BitLocker заважав оновленню Windows на WORKSTATION02")
	q := Analyze("bitlocker оновлення windows r-498")
	has(t, "record idents", rec.Idents, "r-498", "workstation02", "bitlocker")
	has(t, "query idents", q.Idents, "r-498")
	for _, s := range q.Stems {
		if !slices.Contains(rec.Stems, s) {
			t.Errorf("query stem %q not in the record's %q", s, rec.Stems)
		}
	}
}

// TestStopwords: function words are not stems, so a natural question does not require them in the record.
func TestStopwords(t *testing.T) {
	d := Analyze("why the windows upgrade failed")
	hasNot(t, "en", d.Stems, "whi", "the", "why")
	has(t, "en", d.Stems, "window", "upgrad", "fail")
	u := Analyze("чому зникла статусна лінія на ноутбуці")
	hasNot(t, "uk", u.Stems, "чому", "на")
	if len(u.Stems) != 4 {
		t.Errorf("uk stems: %q", u.Stems)
	}
}

// TestLongTokens: a blob is not searchable and must not break the index (lexeme limit of PostgreSQL).
func TestLongTokens(t *testing.T) {
	d := Analyze(strings.Repeat("x", 12000) + " квартира " + strings.Repeat("ab_", 100))
	if len(d.Idents) != 0 || len(d.Stems) != 1 {
		t.Errorf("long tokens must be dropped: %d idents, stems %q", len(d.Idents), d.Stems)
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize("п’ять — ﬁle"); got != "п'ять - file" {
		t.Errorf("Normalize: %q", got)
	}
}

// TestIdentTF: every identifier carries how often the text names it, in the order of Idents (x8).
func TestIdentTF(t *testing.T) {
	d := Analyze("#44 закрито; R-498 і ще раз #44, а t-44 — те саме посилання")
	got := map[string]int{}
	for i, id := range d.Idents {
		got[id] = d.IdentTF[i]
	}
	if len(d.IdentTF) != len(d.Idents) || got["t-44"] != 3 || got["r-498"] != 1 {
		t.Errorf("IdentTF %v for Idents %v, want t-44 = 3, r-498 = 1", d.IdentTF, d.Idents)
	}
}

// TestWordStems: the stems of identifier tokens stay in Stems but not in WordStems (x8).
func TestWordStems(t *testing.T) {
	d := Analyze("setuperr.log показав збій")
	if len(d.WordStems) == 0 || len(d.WordStems) >= len(d.Stems) {
		t.Fatalf("WordStems %v, Stems %v: want the words only", d.WordStems, d.Stems)
	}
	for _, w := range d.WordStems {
		if w == "setuperr" || w == "log" {
			t.Errorf("WordStems %v holds a stem of the identifier", d.WordStems)
		}
	}
}

// TestIdentForms: each identifier keeps the form it was first written in, aligned with Idents.
func TestIdentForms(t *testing.T) {
	d := Analyze("див. R-364, потім r-498 і #44; знову r-364. Коміт 3B80ED5:")
	got := map[string]string{}
	for i, id := range d.Idents {
		got[id] = d.IdentForms[i]
	}
	want := map[string]string{"r-364": "R-364", "r-498": "r-498", "t-44": "#44"}
	for id, form := range want {
		if got[id] != form {
			t.Errorf("%s: form %q, want %q (all: %v)", id, got[id], form, got)
		}
	}
	if len(d.IdentForms) != len(d.Idents) {
		t.Errorf("IdentForms %d not aligned with Idents %d", len(d.IdentForms), len(d.Idents))
	}
}
