package api

import (
	"context"
	"strconv"
	"strings"

	"litopys.smart-gw.co.ua/core/verbs"
)

// Refs of session_close outcomes: explicit, never parsed out of prose — an internal ref (refs.go: a
// record R-N, a task, workflow or execution <project>/T-, W-, X-N, or an id) or a topic as topic:<name>, a label
// without an entity.

func invalidRef(ref, why string) error {
	return &Error{Code: "invalid_ref", Message: "ref " + strconv.Quote(ref) + ": " + why,
		Hint: "R-498 (a record), T-12 or <project>/T-12 (a task; W- a workflow, X- an execution), topic:<name>, or the id of an entity"}
}

// resolveRef gives the subject of a ref (nil for a topic) or the topic label.
func (a *API) resolveRef(ctx context.Context, c *Caller, ref, sessionProject string) (*verbs.EntityRef, string, error) {
	ref = strings.TrimSpace(ref)
	if len(ref) >= 6 && strings.EqualFold(ref[:6], "topic:") {
		topic := strings.TrimSpace(ref[6:])
		if n := len([]rune(topic)); n < 2 || n > 100 {
			return nil, "", invalidRef(ref, "a topic of 2 to 100 characters")
		}
		return nil, topic, nil
	}
	subject, ok, err := a.lookupRef(ctx, c, ref, sessionProject, false)
	if !ok {
		return nil, "", invalidRef(ref, "not a known form")
	}
	return subject, "", err
}

// outcomeArg is one item of session_close outcomes as the client sends it.
type outcomeArg struct {
	Ref      string   `json:"ref"`
	Result   string   `json:"result"`
	State    string   `json:"state"`
	Next     *string  `json:"next,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
}

// resolveOutcomes gives the outcomes to write and warnings. A subject that exists but is no longer live (quarantined,
// retracted, purged) does not fail the whole close (roast r1): its outcome is kept under the ref as a topic
// and a warning says so; a ref to nothing at all is still refused — it is a typo, not the past.
func (a *API) resolveOutcomes(ctx context.Context, c *Caller, args []outcomeArg, sessionProject string) ([]verbs.OutcomeIn, []string, error) {
	if len(args) > verbs.MaxOutcomes {
		return nil, nil, invalid("outcomes: at most %d, one per subject", verbs.MaxOutcomes)
	}
	out := make([]verbs.OutcomeIn, 0, len(args))
	var warnings []string
	for _, o := range args {
		if strings.TrimSpace(o.Ref) == "" {
			return nil, nil, invalidRef(o.Ref, "empty")
		}
		subject, topic, err := a.resolveRef(ctx, c, o.Ref, sessionProject)
		if code := apiCode(err); code == "statement_not_found" || code == "task_not_found" || code == "entity_not_found" {
			if gone, _, gerr := a.lookupRef(ctx, c, strings.TrimSpace(o.Ref), sessionProject, true); gerr == nil && gone != nil {
				subject, topic, err = nil, outcomeTopic(o.Ref), nil
				warnings = append(warnings, "outcome_subject_gone: "+strings.TrimSpace(o.Ref)+
					" is no longer live (quarantined, retracted or purged) — its outcome is kept as topic:"+topic)
			}
		}
		if err != nil {
			return nil, nil, err
		}
		out = append(out, verbs.OutcomeIn{Ref: strings.TrimSpace(o.Ref), Subject: subject, Topic: topic, Result: o.Result,
			State: o.State, Next: o.Next, Evidence: o.Evidence})
	}
	return out, warnings, nil
}

// outcomeTopic is the topic an outcome of a gone subject keeps: its ref, cut to the topic limit.
func outcomeTopic(ref string) string {
	t := []rune(strings.TrimSpace(ref))
	if len(t) > 100 {
		t = t[:100]
	}
	return string(t)
}
