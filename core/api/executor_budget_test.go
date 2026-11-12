package api

import (
	"bytes"
	"encoding/json"
	"testing"

	"litopys.smart-gw.co.ua/core/policy"
)

// TestExecutorBudget: the definitions an executor is listed for a typical step — a write, a
// task amend and a search besides its own step tools — fit 3 000 tokens (≈ 9 000 characters, chars / 3 as in O6).
func TestExecutorBudget(t *testing.T) {
	doc, err := Surface()
	if err != nil {
		t.Fatal(err)
	}
	g := policy.Grant{Profile: "core", Derived: true, StepKey: "s", Verbs: []string{"statement.assert", "task.amend", "recall.search"}}
	size, n := 0, 0
	for _, tool := range doc.Tools {
		if !tool.Visible("owner", "core") || derivedAllows(tool, g, json.RawMessage(`{}`)) != nil {
			continue
		}
		desc, schema := tool.Description, tool.InputSchema
		if tool.Executor != nil {
			desc, schema = tool.Executor.Description, tool.Executor.InputSchema
		}
		var compact bytes.Buffer // as sent: the surface file is indented
		if err := json.Compact(&compact, schema); err != nil {
			t.Fatal(err)
		}
		size += len(tool.Name) + len(tool.Title) + len(desc) + compact.Len()
		n++
	}
	if n != len(stepOwn)+len(g.Verbs) {
		t.Errorf("listed %d tools, want %d", n, len(stepOwn)+len(g.Verbs))
	}
	if size > 9000 {
		t.Errorf("a typical step's definitions: %d characters (≈ %d tokens), budget 9 000 (3 000 tokens)", size, size/3)
	}
	t.Logf("typical step: %d tools, %d characters (≈ %d tokens)", n, size, size/3)
}
