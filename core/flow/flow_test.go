package flow

import (
	"encoding/json"
	"testing"
	"time"
)

func def(t *testing.T, raw string) Definition {
	t.Helper()
	d, err := ParseDefinition(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func states(st State) map[string]string {
	out := map[string]string{}
	for _, s := range st.Steps {
		out[s.Key] = s.State
	}
	return out
}

func TestDerive(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	d := def(t, `{"steps": [
	  {"key": "collect", "kind": "model"},
	  {"key": "go", "kind": "human"},
	  {"key": "write", "kind": "model", "requires_approval": true},
	  {"key": "restore", "kind": "external", "condition": "params.migration == true"},
	  {"key": "done", "kind": "script", "retry": {"max": 1}}]}`)
	params := map[string]any{"migration": false}

	st, err := Derive(d, params, nil, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := states(st); got["collect"] != Ready || got["go"] != Pending || len(st.ReadyFor) != 1 {
		t.Fatalf("start: %v", got)
	}

	att := []Attempt{{Key: "collect", N: 1, State: "working", LeaseUntil: now.Add(time.Minute)}}
	st, _ = Derive(d, params, att, nil, now)
	if states(st)["collect"] != Working || len(st.ReadyFor) != 0 {
		t.Fatalf("working: %v", states(st))
	}
	st, _ = Derive(d, params, att, nil, now.Add(2*time.Minute))
	if s, _ := st.Of("collect"); s.State != Ready || !s.Expired {
		t.Fatalf("expired lease: %+v", s)
	}

	att = []Attempt{{Key: "collect", N: 1, State: "completed", Output: json.RawMessage(`{"n": 3}`)}}
	st, _ = Derive(d, params, att, nil, now)
	if s, _ := st.Of("go"); s.State != InputRequired || s.Waiting != "person" {
		t.Fatalf("human step: %+v", s)
	}
	// GO on the human step completes it; write then waits for its own GO
	st, _ = Derive(d, params, att, []Approval{{Key: "go"}}, now)
	if s, _ := st.Of("write"); s.State != InputRequired || s.Waiting != "approval" {
		t.Fatalf("approval wait: %+v", s)
	}
	st, _ = Derive(d, params, att, []Approval{{Key: "go"}, {Key: "write"}}, now)
	if states(st)["write"] != Ready {
		t.Fatalf("approved: %v", states(st))
	}

	// write done -> restore skipped by its condition -> done ready; one retry, then the run fails
	att = append(att, Attempt{Key: "write", N: 1, State: "completed"})
	appr := []Approval{{Key: "go"}, {Key: "write", Used: true}}
	st, _ = Derive(d, params, att, appr, now)
	if got := states(st); got["restore"] != Skipped || got["done"] != Ready {
		t.Fatalf("skip: %v", got)
	}
	att2 := append(att, Attempt{Key: "done", N: 1, State: "failed"})
	st, _ = Derive(d, params, att2, appr, now)
	if states(st)["done"] != Ready || st.Failed {
		t.Fatalf("retry left: %v", states(st))
	}
	att2 = append(att2, Attempt{Key: "done", N: 2, State: "failed"})
	st, _ = Derive(d, params, att2, appr, now)
	if states(st)["done"] != Failed || !st.Failed {
		t.Fatalf("retries used: %v", states(st))
	}
	att3 := append(att, Attempt{Key: "done", N: 1, State: "completed"})
	st, _ = Derive(d, params, att3, appr, now)
	if !st.Done {
		t.Fatalf("done: %v", states(st))
	}
	// the condition sees outputs of earlier steps
	st, _ = Derive(def(t, `{"steps": [{"key": "a", "kind": "script"}, {"key": "b", "kind": "script", "condition": "steps.a.output.n > 2"}]}`),
		nil, []Attempt{{Key: "a", N: 1, State: "completed", Output: json.RawMessage(`{"n": 3}`)}}, nil, now)
	if states(st)["b"] != Ready {
		t.Fatalf("condition on output: %v", states(st))
	}
}

func TestDeriveBranches(t *testing.T) {
	// a -> (b, c) in parallel -> d joins both
	d := def(t, `{"steps": [
	  {"key": "a", "kind": "script", "next": ["b", "c"]},
	  {"key": "b", "kind": "script", "next": ["d"]},
	  {"key": "c", "kind": "script", "next": ["d"]},
	  {"key": "d", "kind": "script"}]}`)
	now := time.Now()
	st, _ := Derive(d, nil, []Attempt{{Key: "a", N: 1, State: "completed"}}, nil, now)
	if len(st.ReadyFor) != 2 {
		t.Fatalf("parallel: %v", states(st))
	}
	st, _ = Derive(d, nil, []Attempt{{Key: "a", N: 1, State: "completed"}, {Key: "b", N: 1, State: "completed"}}, nil, now)
	if states(st)["d"] != Pending {
		t.Fatalf("join waits for both: %v", states(st))
	}
}
