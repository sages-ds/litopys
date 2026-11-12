// Package flow derives the state of a workflow execution: which steps are done, skipped, running,
// waiting for a person or ready to be taken. It is pure — the journal and the projections feed it — so commands
// (may this step start? is the run finished?) and reads (execution.read, step.open_items) see the same answer.
package flow

import (
	"encoding/json"
	"fmt"
	"time"

	"cel.dev/cel-go/cel"
)

// Step is what the state machine needs from a step definition (types.yaml StepDefinition).
type Step struct {
	Key              string   `json:"key"`
	Title            string   `json:"title"`
	Kind             string   `json:"kind"`
	ExecutorClass    *string  `json:"executor_class,omitempty"`
	Delegable        *bool    `json:"delegable,omitempty"`
	Verbs            []string `json:"verbs,omitempty"`
	WriteMode        *string  `json:"write_mode,omitempty"`
	MaxDataClass     *string  `json:"max_data_class,omitempty"`
	WhoMayRun        *string  `json:"who_may_run,omitempty"`
	RequiresApproval bool     `json:"requires_approval,omitempty"`
	Condition        *string  `json:"condition,omitempty"`
	Next             []string `json:"next,omitempty"`
	TimeoutS         *int     `json:"timeout_s,omitempty"`
	Retry            *struct {
		Max *int `json:"max,omitempty"`
	} `json:"retry,omitempty"`
}

// Definition is the part of a frozen version the state machine reads.
type Definition struct {
	Steps []Step `json:"steps"`
}

// ParseDefinition decodes a stored definition (already validated on declare / supersede).
func ParseDefinition(raw json.RawMessage) (Definition, error) {
	var d Definition
	err := json.Unmarshal(raw, &d)
	return d, err
}

// DefaultLease is the lease of an attempt when the step sets no timeout_s.
const DefaultLease = 15 * time.Minute

// Lease of a step attempt.
func (s Step) Lease() time.Duration {
	if s.TimeoutS != nil {
		return time.Duration(*s.TimeoutS) * time.Second
	}
	return DefaultLease
}

// DefaultAttempts: the attempts of a step without a retry rule. Unlimited was the old default — a model step that kept
// failing was retried every poll and burnt the executor's daily quota (runner-test X-3: 45 attempts, 2026-11-01).
const DefaultAttempts = 3

// MaxAttempts: retry.max + 1, or DefaultAttempts without a retry rule.
func (s Step) MaxAttempts() int {
	if s.Retry != nil && s.Retry.Max != nil {
		return *s.Retry.Max + 1
	}
	return DefaultAttempts
}

// Attempt is one attempt of a step as the projection holds it.
type Attempt struct {
	Key        string
	N          int
	State      string // working | completed | failed | canceled | rejected
	LeaseUntil time.Time
	Output     json.RawMessage
}

// Approval is a live (not revoked) GO of a person on a step.
type Approval struct {
	Key  string
	Used bool
}

// Step states of an execution: the A2A vocabulary plus two derived ones — pending (its
// predecessors are not done) and skipped (its condition is false).
const (
	Pending       = "pending"
	Ready         = "submitted"
	Working       = "working"
	InputRequired = "input_required"
	Completed     = "completed"
	Failed        = "failed"
	Skipped       = "skipped"
)

// StepState is the derived state of one step.
type StepState struct {
	Key      string `json:"key"`
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	// Expired: the latest attempt is working but its lease ran out — the step may be taken again.
	Expired bool `json:"lease_expired,omitempty"`
	// Waiting: why a not-ready step waits — "approval" (GO of a person) or "person" (a human step).
	Waiting string `json:"waiting,omitempty"`
}

// State is the derived state of an execution.
type State struct {
	Steps    []StepState `json:"steps"`
	Done     bool        `json:"done"` // every step completed or skipped
	Failed   bool        `json:"failed"`
	ReadyFor []string    `json:"-"` // keys that may be started now
}

var condEnv = func() *cel.Env {
	env, err := cel.NewEnv(
		cel.Variable("params", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("steps", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		panic("flow: CEL environment: " + err.Error())
	}
	return env
}()

// EvalCondition evaluates a step condition over the run parameters and the outputs of finished steps.
// A condition that refers to a step without output yet evaluates with an empty map there.
func EvalCondition(expr string, params map[string]any, steps map[string]any) (bool, error) {
	ast, iss := condEnv.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return false, iss.Err()
	}
	prg, err := condEnv.Program(ast)
	if err != nil {
		return false, err
	}
	out, _, err := prg.Eval(map[string]any{"params": params, "steps": steps})
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("condition is not bool: %v", out.Value())
	}
	return b, nil
}

// Derive computes the state of every step at time now. Successors of a step are its next, or else the step after
// it; a step becomes due when all its predecessors are completed or skipped. A due step with a false condition is
// skipped (its successors go on); a due human step waits for a person; a due step with requires_approval waits for
// a live GO. A step whose latest attempt failed is failed while attempts remain unused; when they are used up the
// execution is failed.
func Derive(d Definition, params map[string]any, attempts []Attempt, approvals []Approval, now time.Time) (State, error) {
	n := len(d.Steps)
	index := make(map[string]int, n)
	for i, s := range d.Steps {
		index[s.Key] = i
	}
	preds := make([][]int, n)
	for i, s := range d.Steps {
		if len(s.Next) == 0 {
			if i+1 < n {
				preds[i+1] = append(preds[i+1], i)
			}
			continue
		}
		for _, k := range s.Next {
			if j, ok := index[k]; ok {
				preds[j] = append(preds[j], i)
			}
		}
	}
	latest := map[string]Attempt{}
	count := map[string]int{}
	for _, a := range attempts {
		count[a.Key]++
		if cur, ok := latest[a.Key]; !ok || a.N > cur.N {
			latest[a.Key] = a
		}
	}
	approved := map[string]bool{}
	for _, a := range approvals {
		if !a.Used {
			approved[a.Key] = true
		}
	}
	st := State{Steps: make([]StepState, n)}
	outputs := map[string]any{}
	for i, s := range d.Steps { // definitions are acyclic and steps come in an order where every predecessor that
		// is not an explicit back edge precedes; resolve in rounds until stable
		st.Steps[i] = StepState{Key: s.Key, State: Pending, Attempts: count[s.Key]}
	}
	resolved := make([]bool, n)
	for changed := true; changed; {
		changed = false
		for i, s := range d.Steps {
			if resolved[i] {
				continue
			}
			due := true
			for _, p := range preds[i] {
				if !resolved[p] || (st.Steps[p].State != Completed && st.Steps[p].State != Skipped) {
					due = false
					break
				}
			}
			ss := &st.Steps[i]
			if a, ok := latest[s.Key]; ok {
				switch a.State {
				case "completed":
					ss.State = Completed
					var out any
					if len(a.Output) > 0 {
						_ = json.Unmarshal(a.Output, &out)
					}
					outputs[s.Key] = map[string]any{"output": orEmpty(out), "state": Completed}
					resolved[i], changed = true, true
					continue
				case "working":
					if now.Before(a.LeaseUntil) {
						ss.State = Working
						resolved[i], changed = true, true
						continue
					}
					ss.Expired = true
				default: // failed, canceled, rejected: a new attempt may follow while any remain
					if m := s.MaxAttempts(); m > 0 && count[s.Key] >= m {
						ss.State = Failed
						st.Failed = true
						resolved[i], changed = true, true
						continue
					}
				}
			}
			if !due {
				continue
			}
			if s.Condition != nil {
				ok, err := EvalCondition(*s.Condition, params, outputs)
				if err != nil {
					return st, fmt.Errorf("step %s: condition: %w", s.Key, err)
				}
				if !ok {
					ss.State = Skipped
					outputs[s.Key] = map[string]any{"output": map[string]any{}, "state": Skipped}
					resolved[i], changed = true, true
					continue
				}
			}
			switch {
			case s.Kind == "human" && !approved[s.Key]:
				ss.State, ss.Waiting = InputRequired, "person"
			case s.Kind == "human": // a GO on a human step completes it
				ss.State = Completed
				outputs[s.Key] = map[string]any{"output": map[string]any{}, "state": Completed}
			case s.RequiresApproval && !approved[s.Key] && ss.Attempts == 0:
				ss.State, ss.Waiting = InputRequired, "approval"
			default:
				ss.State = Ready
				st.ReadyFor = append(st.ReadyFor, s.Key)
			}
			resolved[i], changed = true, true
		}
	}
	st.Done = true
	for _, ss := range st.Steps {
		if ss.State != Completed && ss.State != Skipped {
			st.Done = false
		}
	}
	return st, nil
}

func orEmpty(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

// Of returns the derived state of a step by key.
func (s State) Of(key string) (StepState, bool) {
	for _, ss := range s.Steps {
		if ss.Key == key {
			return ss, true
		}
	}
	return StepState{}, false
}
