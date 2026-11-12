package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const openSecret = "password=abc"

// argsFor builds arguments holding text at one path of the surface ("definition.steps[].purpose", "fields{}").
func argsFor(path string, text any) any {
	head, rest, nested := strings.Cut(path, ".")
	var v any = text
	if nested {
		v = argsFor(rest, text)
	}
	for {
		switch {
		case strings.HasSuffix(head, "[]"):
			head, v = strings.TrimSuffix(head, "[]"), []any{v}
			continue
		case strings.HasSuffix(head, "{}"):
			head, v = strings.TrimSuffix(head, "{}"), map[string]any{"free_key": v}
			continue
		}
		return map[string]any{head: v}
	}
}

// TestTextFieldsSurface walks the whole surface: every classified path of every command is masked and loses its private
// block. A new field gets a class or the generator refuses it, so this reaches the fields of future verbs too.
func TestTextFieldsSurface(t *testing.T) {
	doc, err := Surface()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, tool := range doc.Tools {
		if tool.Kind != "command" && len(tool.TextFields) > 0 {
			t.Errorf("%s: a query has text classes", tool.Name)
		}
		for path, class := range tool.TextFields {
			seen++
			if class != textLine && class != textFree && class != textJSON {
				t.Errorf("%s %s: class %q", tool.Name, path, class)
			}
			var text any = openSecret + " <private>drop</private>"
			if class == textJSON {
				text = map[string]any{"note": text, "n": json.Number("12345678901234567890")}
			}
			in, _ := json.Marshal(argsFor(path, text))
			got := string(cleanArgs(tool, in))
			if strings.Contains(got, "abc") || strings.Contains(got, "drop") {
				t.Errorf("%s %s (%s): %s", tool.Name, path, class, got)
			}
			if class == textJSON && !strings.Contains(got, "12345678901234567890") {
				t.Errorf("%s %s: a number changed: %s", tool.Name, path, got)
			}
		}
	}
	if seen < 50 {
		t.Errorf("only %d classified fields", seen)
	}
}

func TestTextFieldsClasses(t *testing.T) {
	doc, err := Surface()
	if err != nil {
		t.Fatal(err)
	}
	byVerb := map[string]Tool{}
	for _, tool := range doc.Tools {
		byVerb[tool.Verb] = tool
	}
	for _, c := range []struct{ verb, path, class string }{
		{"statement.assert", "title", textLine},
		{"statement.assert", "body", textFree},
		{"statement.assert", "fields{}", textFree},
		{"task.declare", "acceptance", textFree},
		{"session.close", "outcomes[].result", textLine},
		{"session.close", "next_steps[]", textLine},
		{"workflow.declare", "definition.steps[].title", textLine},
	} {
		if got := byVerb[c.verb].TextFields[c.path]; got != c.class {
			t.Errorf("%s %s: %q, want %q", c.verb, c.path, got, c.class)
		}
	}
	// what must stay exact: ids, uploaded bytes, workflow code and execution data (owner 2026-11-08), the topic key
	for _, c := range []struct{ verb, path string }{
		{"attachment.record", "content_base64"}, {"statement.assert", "external_ref"}, {"task.amend", "task_id"}, {"project.declare", "name"},
		{"workflow.declare", "definition.steps[].condition"}, {"workflow.declare", "definition.steps[].idempotency"},
		{"workflow.declare", "definition.steps[].next[]"}, {"workflow.declare", "definition.steps[].input"},
		{"workflow.declare", "definition.params"}, {"workflow.declare", "definition.triggers[].spec"},
		{"execution.start", "params"}, {"step.finish", "output"}, {"step.checkpoint", "output"}, {"topic.resume", "topic"},
	} {
		if got, ok := byVerb[c.verb].TextFields[c.path]; ok {
			t.Errorf("%s %s is classified %q", c.verb, c.path, got)
		}
	}
}

// TestWorkflowCodeUntouched: conditions and step data keep their exact text, placeholders and types.
func TestWorkflowCodeUntouched(t *testing.T) {
	doc, err := Surface()
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range doc.Tools {
		if tool.Verb != "workflow.declare" && tool.Verb != "step.finish" {
			continue
		}
		for _, in := range []string{
			`{"definition":{"steps":[{"condition":"steps.auth.output.token == params.token"}]}}`,
			`{"definition":{"steps":[{"condition":"has_password == true && params.secret=='x'"}]}}`,
			`{"definition":{"steps":[{"input":{"token":"${GITEA_TOKEN}","password":null,"n":5}}]}}`,
			`{"output":{"token":5,"mode":"token: rotate"}}`,
		} {
			if got := string(cleanArgs(tool, json.RawMessage(in))); got != in {
				t.Errorf("%s changed %s into %s", tool.Verb, in, got)
			}
		}
	}
}

func TestCleanArgsExamples(t *testing.T) {
	tool := Tool{TextFields: map[string]string{"title": textLine, "body": textFree, "fields{}": textFree, "items[].note": textFree, "output": textJSON}}
	for in, want := range map[string]string{
		`{"title":"  a \n b password=abc  "}`:                   `{"title":"a b password=***"}`,
		`{"body":"keep\n  code <private>x</private>"}`:          `{"body":"keep\n  code"}`,
		`{"fields":{"any":"token: abc","n":5}}`:                 `{"fields":{"any":"token: ***","n":5}}`,
		`{"items":[{"note":"pwd:1"}]}`:                          `{"items":[{"note":"pwd:***"}]}`,
		`{"output":{"token":"x","text":"secret=abc","n":1.50}}`: `{"output":{"n":1.50,"text":"secret=***","token":"***"}}`,
		`{"key":"password=abc"}`:                                `{"key":"password=abc"}`, // no class: untouched
		`{"title":`:                                             `{"title":`,              // does not parse: decode refuses it
	} {
		if got := string(cleanArgs(tool, json.RawMessage(in))); got != want {
			t.Errorf("cleanArgs(%s) = %s, want %s", in, got, want)
		}
	}
}

// TestDispatchCleansBeforeTheHandler: the pipeline is wired in front of every command, and a query is left alone.
func TestDispatchCleansBeforeTheHandler(t *testing.T) {
	doc, err := Surface()
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	capture := func(_ context.Context, _ *API, _ *Caller, args json.RawMessage) (any, error) {
		seen = string(args)
		return nil, nil
	}
	for _, name := range []string{"statement.assert", "recall.search"} {
		old := handlers[name]
		handlers[name] = capture
		defer func() { handlers[name] = old }()
	}
	a := &API{}
	c := &Caller{Role: "owner"}
	for _, tool := range doc.Tools {
		switch tool.Verb {
		case "statement.assert":
			if _, e := a.dispatch(context.Background(), tool, c, json.RawMessage(`{"body":"pwd:1"}`)); e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(seen, "pwd:***") {
				t.Errorf("a command reached its handler with %s", seen)
			}
		case "recall.search":
			if _, e := a.dispatch(context.Background(), tool, c, json.RawMessage(`{"query":"pwd:1"}`)); e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(seen, "pwd:1") {
				t.Errorf("a query reached its handler with %s", seen)
			}
		}
	}
}
