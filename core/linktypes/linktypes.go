// Package linktypes is the one table of link relations: gen/link_types.json is generated from
// domain/link-types.yaml (domain/tools/gen.mjs, drift-checked in CI) with the ends and rules of every active
// relation. Commands (core/verbs) check links against it, reads (core/query) take inverse names
// and "satisfied" relations from it; no package lists relations by hand.
package linktypes

import (
	_ "embed"
	"encoding/json"
	"slices"
	"sort"
	"strings"
)

//go:embed gen/link_types.json
var tableJSON []byte

// Type is one active relation.
type Type struct {
	Layer       string `json:"layer"`
	Inverse     string `json:"inverse"`
	Ends        []End  `json:"ends"`
	Symmetric   bool   `json:"symmetric"`
	Transitive  bool   `json:"transitive"`
	Acyclic     bool   `json:"acyclic"`
	Scope       string `json:"scope"`
	Projects    string `json:"projects"` // any | same | different — for two task ends
	OnEndClosed string `json:"on_end_closed"`
}

type End struct {
	From string `json:"from"`
	To   string `json:"to"`
}

var (
	table     map[string]Type
	relations []string
)

func init() {
	var doc struct {
		LinkTypes map[string]Type `json:"link_types"`
	}
	if err := json.Unmarshal(tableJSON, &doc); err != nil {
		panic("linktypes: gen/link_types.json: " + err.Error())
	}
	table = doc.LinkTypes
	for name := range table {
		relations = append(relations, name)
	}
	sort.Strings(relations)
}

// Get returns a relation and whether it is active.
func Get(relation string) (Type, bool) {
	t, ok := table[relation]
	return t, ok
}

// Relations lists the active relations, sorted.
func Relations() []string { return slices.Clone(relations) }

// Satisfiable lists the relations read as satisfied once the target task is closed (depends_on, handoff):
// a task "waits on" the open targets of these and "blocks" their open sources.
func Satisfiable() []string {
	var out []string
	for _, r := range relations {
		if table[r].OnEndClosed == "satisfied" {
			out = append(out, r)
		}
	}
	return out
}

// Allows reports whether the pair of entity types is one of the declared ends ("*" matches any).
func (t Type) Allows(from, to string) bool {
	return slices.ContainsFunc(t.Ends, func(e End) bool {
		return (e.From == "*" || e.From == from) && (e.To == "*" || e.To == to)
	})
}

// EndsText renders the declared ends for an error message.
func (t Type) EndsText() string {
	parts := make([]string, len(t.Ends))
	for i, e := range t.Ends {
		parts[i] = e.From + " -> " + e.To
	}
	return strings.Join(parts, ", ")
}
