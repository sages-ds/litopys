package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"litopys.smart-gw.co.ua/core/policy"
)

// The generated surface and the Go dispatch must cover each other exactly.
func TestSurfaceMatchesHandlers(t *testing.T) {
	doc, err := Surface()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range doc.Tools {
		seen[tool.Verb] = true
		if _, ok := handlers[tool.Verb]; !ok {
			t.Errorf("%s (%s): no handler", tool.Name, tool.Verb)
		}
		if strings.Contains(string(tool.InputSchema), `"$ref"`) {
			t.Errorf("%s: input schema keeps a $ref", tool.Name)
		}
		var s map[string]any
		if err := json.Unmarshal(tool.InputSchema, &s); err != nil || s["type"] != "object" {
			t.Errorf("%s: input schema is not an object schema", tool.Name)
		}
		if len(tool.Description) < 40 || len(tool.Description) > 1024 {
			t.Errorf("%s: description of %d characters", tool.Name, len(tool.Description))
		}
		if hasCyrillic(tool.Description) || hasCyrillic(string(tool.InputSchema)) {
			t.Errorf("%s: model-facing text must be English", tool.Name)
		}
	}
	for verb := range handlers {
		if !seen[verb] {
			t.Errorf("handler %s: not in the surface", verb)
		}
	}
}

// A refused call is audited with its error code, also when it is refused before dispatch.
func TestAuditCarriesErrorCode(t *testing.T) {
	doc, _ := Surface()
	var tool Tool
	for _, x := range doc.Tools {
		if x.Name == "task_list" {
			tool = x
		}
	}
	var gotTool, gotCode string
	calls := 0
	a := &API{Audit: func(_ context.Context, name string, _ Caller, code string, _ time.Duration) {
		calls++
		gotTool, gotCode = name, code
	}}
	c := &Caller{Role: "owner", Grant: policy.Grant{Profile: policy.ProfileChat}}
	if _, e := a.call(context.Background(), tool, c, nil); e == nil || e.Code != "not_in_profile" {
		t.Fatalf("task_list in chat: %v", e)
	}
	if calls != 1 || gotTool != "task_list" || gotCode != "not_in_profile" {
		t.Errorf("audit: %d calls, tool %q, code %q", calls, gotTool, gotCode)
	}
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if r >= 0x0400 && r <= 0x04FF {
			return true
		}
	}
	return false
}

// Profiles: chat is the 16 listed tools (the guide included) and a subset of core; readonly sees
// no command except the resume family; admin verbs stay with the admin role.
func TestProfiles(t *testing.T) {
	doc, _ := Surface()
	want := map[string]bool{"project_resume": true, "session_resume": true, "task_resume": true, "topic_resume": true,
		"statement_assert": true, "statement_amend": true, "recall_search": true, "recall_read": true, "task_declare": true,
		"task_amend": true, "task_close": true, "session_checkpoint": true, "session_close": true, "project_list": true,
		"system_whoami": true, "system_guide": true}
	chat := 0
	for _, tool := range doc.Tools {
		if tool.Visible("owner", "chat") {
			chat++
			if !want[tool.Name] {
				t.Errorf("chat lists %s", tool.Name)
			}
			if !tool.Visible("owner", "core") {
				t.Errorf("%s: in chat but not in core", tool.Name)
			}
		}
		if tool.Visible("owner", "readonly") && tool.Kind == "command" && !strings.HasSuffix(tool.Name, "_resume") {
			t.Errorf("readonly lists the command %s", tool.Name)
		}
		if tool.Auth == "admin" && tool.Visible("owner", "full") {
			t.Errorf("owner sees the admin verb %s", tool.Name)
		}
	}
	if chat != len(want) {
		t.Errorf("chat lists %d tools, want %d", chat, len(want))
	}
}

// Onboarding: the core fits the first 512 characters, the whole text 2 KB.
func TestInstructions(t *testing.T) {
	if n := len(InstructionsCore); n > 512 {
		t.Errorf("core of instructions: %d characters, want ≤ 512", n)
	}
	if n := len(Instructions); n > 2048 {
		t.Errorf("instructions: %d characters, want ≤ 2048", n)
	}
	for _, must := range []string{"Litopys", "project_resume", "statement_assert", "session_close"} {
		if !strings.Contains(InstructionsCore, must) {
			t.Errorf("core of instructions does not name %s", must)
		}
	}
	// one source: instructions start with the core and carry every rule line and the guide
	if !strings.HasPrefix(Instructions, InstructionsCore) {
		t.Error("instructions do not start with their core")
	}
	for _, l := range append(Guide.Rules.Lines, Guide.Rules.Guide) {
		if !strings.Contains(Instructions, l) {
			t.Errorf("instructions lack the rule %q", l)
		}
	}
}

// The guide: an index naming every topic, a playbook per topic within the size limit, see_also only
// to topics, an unknown topic refused with the list.
func TestGuide(t *testing.T) {
	idx, err := systemGuide("")
	if err != nil || idx["topic"] != "index" || idx["version"] != Guide.Version {
		t.Fatalf("index: %v %v", idx, err)
	}
	if len(Guide.Order) != 7 {
		t.Errorf("topics: %v", Guide.Order)
	}
	for _, name := range Guide.Order {
		if !strings.Contains(Guide.Index, "- "+name+" — ") {
			t.Errorf("index does not list %s", name)
		}
		g, err := systemGuide(name)
		if err != nil || len(g["text"].(string)) == 0 || len(g["text"].(string)) > 2000 {
			t.Errorf("topic %s: %v", name, err)
		}
		for _, s := range Guide.Topics[name].SeeAlso {
			if _, ok := Guide.Topics[s]; !ok {
				t.Errorf("topic %s: see_also %s is no topic", name, s)
			}
		}
	}
	if _, err := systemGuide("nonsense"); err == nil || !strings.Contains(err.Error(), "cross-project") {
		t.Errorf("unknown topic: %v", err)
	}
	if n := len(strings.Join(Guide.Rules.Lines, "")); n > 1000 {
		t.Errorf("rules lines: %d characters, want ≤ 1000", n)
	}
}

// The native API speaks as Litopys: no text a model or a person gets from it names a forbidden term (the previous
// service, the corporate line) — tool titles and descriptions, parameter texts, instructions, error hints.
// The list is private, as for scripts/check-terms.sh: the CI secret FORBIDDEN_TERMS_B64 (base64, one Perl regular
// expression per line, # for comments). A pattern RE2 cannot compile is left to check-terms.sh; no secret — skipped.
func TestBranding(t *testing.T) {
	env := os.Getenv("FORBIDDEN_TERMS_B64")
	if env == "" {
		t.Skip("FORBIDDEN_TERMS_B64 is not set")
	}
	raw, err := base64.StdEncoding.DecodeString(env)
	if err != nil {
		t.Fatalf("FORBIDDEN_TERMS_B64 is no base64: %v", err) // a broken secret must not pass as a skipped check
	}
	// numbered like check-terms.sh: every line that is neither empty nor a comment, as it stands (CR dropped)
	type term struct {
		n  int
		re *regexp.Regexp
	}
	var terms []term
	n := 0
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r", ""), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
		re, err := regexp.Compile("(?i)" + line)
		if err != nil {
			t.Logf("pattern #%d: not RE2, left to check-terms.sh", n)
			continue
		}
		terms = append(terms, term{n, re})
	}
	if len(terms) == 0 {
		t.Fatalf("FORBIDDEN_TERMS_B64: none of %d patterns compiles as RE2 — the test would check nothing", n)
	}
	doc, _ := Surface()
	texts := map[string]string{"instructions": Instructions}
	for _, tool := range doc.Tools {
		texts[tool.Name+" title"] = tool.Title
		texts[tool.Name+" description"] = tool.Description
		texts[tool.Name+" schema"] = string(tool.InputSchema)
	}
	for code, h := range hints {
		texts["hint "+code] = h
	}
	for where, s := range texts {
		for _, x := range terms {
			if x.re.MatchString(s) {
				t.Errorf("%s matches forbidden pattern #%d", where, x.n) // the pattern itself is private
			}
		}
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Квартира Сад":          "kvartyra-sad",
		"Згорани":               "zghorany",
		"Їжак Юрій":             "yizhak-yurii",
		"Яблуко й груша":        "yabluko-y-hrusha",
		"Café Noël":             "cafe-noel",
		"chatgpt mobile":        "chatgpt-mobile",
		"  --Litopys 2.0 !! ":   "litopys-2-0",
		"Щука":                  "shchuka",
		"!!!":                   "",
		strings.Repeat("a", 70): strings.Repeat("a", 60),
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckAgent(t *testing.T) {
	for _, ok := range []string{"", "reviewer", "planner-2", "db-migrator", "gopher"} {
		if err := checkAgent(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"ChatGPT", "chatgpt", "gpt-4o", "claude", "claude-sonnet-5", "openai", "gemini-2.5", "codex",
		"o3", "my-gpt-helper", "llama3", "Reviewer", "a b"} {
		if err := checkAgent(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
