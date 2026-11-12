package api

import (
	_ "embed"
	"encoding/json"
	"strings"
)

// The any-client guide (owner decision 2026-10-30): domain/guide → gen/guide.json (domain/tools/gen.mjs).
// One source for the instructions of initialize, the rules block of project_resume / session.open and the topics of
// system.guide, so no rule lives in two wordings.

//go:embed gen/guide.json
var guideJSON []byte

// GuideTopic is one playbook of system.guide.
type GuideTopic struct {
	Summary string   `json:"summary"`
	Text    string   `json:"text"`
	SeeAlso []string `json:"see_also"`
}

// GuideDoc is gen/guide.json.
type GuideDoc struct {
	Version      string `json:"version"`
	Core         string `json:"core"`
	Instructions string `json:"instructions"`
	Rules        struct {
		Lines []string `json:"lines"`
		Guide string   `json:"guide"`
	} `json:"rules"`
	Index  string                `json:"index"`
	Order  []string              `json:"order"` // topic names in the order of the index
	Topics map[string]GuideTopic `json:"topics"`
}

// Guide is the embedded guide; a broken file stops the process at start, as the surface would.
var Guide = func() GuideDoc {
	var g GuideDoc
	if err := json.Unmarshal(guideJSON, &g); err != nil || g.Core == "" || len(g.Topics) == 0 {
		panic("api: gen/guide.json is broken — run node domain/tools/gen.mjs")
	}
	return g
}()

// InstructionsCore is the part of the instructions that must survive any truncation (the first 512 characters).
var InstructionsCore = Guide.Core

// Instructions is the full text of initialize (≤ 2048 characters).
var Instructions = Guide.Instructions

// rulesBlock is the "rules" field of project_resume and session.open.
func rulesBlock() map[string]any {
	return map[string]any{"version": Guide.Version, "lines": Guide.Rules.Lines, "guide": Guide.Rules.Guide}
}

// systemGuide is system.guide: the index without a topic, a playbook with one; an unknown topic names the topics.
func systemGuide(topic string) (map[string]any, error) {
	if topic == "" {
		return map[string]any{"topic": "index", "version": Guide.Version, "text": Guide.Index, "see_also": Guide.Order}, nil
	}
	t, ok := Guide.Topics[topic]
	if !ok {
		return nil, invalid("topic: one of %s", strings.Join(Guide.Order, ", "))
	}
	return map[string]any{"topic": topic, "version": Guide.Version, "text": t.Text, "see_also": t.SeeAlso}, nil
}
