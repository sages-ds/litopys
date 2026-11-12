// Package vocab is the built-in catalog of usage vocabularies: gen/vocabularies.json is generated from
// domain/vocabularies.yaml (domain/tools/gen.mjs, drift-checked in CI). A record type belongs to one family; the
// types of the general vocabulary are the families and are stored without a prefix ("decision"), a type of another
// vocabulary by its full name ("dba.migration"). A project works with a set of vocabularies — general always in it.
package vocab

import (
	_ "embed"
	"encoding/json"
	"slices"
	"sort"
	"strings"
)

//go:embed gen/vocabularies.json
var catalogJSON []byte

// General is the vocabulary every project has; its type keys are the families.
const General = "general"

// Title is the human name of a vocabulary or a type.
type Title struct {
	UK string `json:"uk"`
	EN string `json:"en,omitempty"`
}

// Type is one record type of a vocabulary.
type Type struct {
	Family        string   `json:"family"`
	Title         Title    `json:"title"`
	Description   string   `json:"description"` // for the model, English
	Template      []string `json:"template,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	DataClassHint string   `json:"data_class_hint,omitempty"`
	Reserved      bool     `json:"reserved,omitempty"` // written only by the server (outcome — session.close)
	Retired       bool     `json:"retired,omitempty"`
	ReplacedBy    string   `json:"replaced_by,omitempty"`
	// Fields: the structured fields a record of this type may carry — amount, currency, payee…
	Fields map[string]Field `json:"fields,omitempty"`
}

// Field is one structured field of a type: number, string, date (YYYY-MM-DD), currency (ISO 4217), email, phone.
type Field struct {
	Type     string `json:"type"`
	Title    string `json:"title,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// Vocabulary is one built-in vocabulary at its current version.
type Vocabulary struct {
	Version  int             `json:"version"`
	Title    Title           `json:"title"`
	Purpose  string          `json:"purpose"`
	Includes []string        `json:"includes,omitempty"` // full names of types of other vocabularies
	Types    map[string]Type `json:"types"`
}

var (
	families []string
	legacy   map[string]string
	catalog  map[string]Vocabulary
	keys     []string
)

func init() {
	var doc struct {
		Families     []string              `json:"families"`
		Legacy       map[string]string     `json:"legacy"`
		Vocabularies map[string]Vocabulary `json:"vocabularies"`
	}
	if err := json.Unmarshal(catalogJSON, &doc); err != nil {
		panic("vocab: gen/vocabularies.json: " + err.Error())
	}
	families, legacy, catalog = doc.Families, doc.Legacy, doc.Vocabularies
	for k := range catalog {
		keys = append(keys, k)
	}
	sort.Strings(keys)
}

// Families: the closed list of families of the core.
func Families() []string { return slices.Clone(families) }

// IsFamily reports whether f is a family.
func IsFamily(f string) bool { return slices.Contains(families, f) }

// Keys: the keys of the built-in vocabularies, sorted.
func Keys() []string { return slices.Clone(keys) }

// Get returns a built-in vocabulary.
func Get(key string) (Vocabulary, bool) { v, ok := catalog[key]; return v, ok }

// split: "dba.migration" -> (dba, migration); "decision" -> (general, decision); "general.decision" is general too.
func split(name string) (vocabulary, short string) {
	if v, t, ok := strings.Cut(name, "."); ok {
		return v, t
	}
	return General, name
}

// Canonical is the stored form of a type: no prefix for general.
func Canonical(vocabulary, short string) string {
	if vocabulary == General {
		return short
	}
	return vocabulary + "." + short
}

// Lookup finds a type of the catalog by its stored or full name.
func Lookup(name string) (Type, bool) {
	v, t := split(name)
	voc, ok := catalog[v]
	if !ok {
		return Type{}, false
	}
	typ, ok := voc.Types[t]
	return typ, ok
}

// Legacy maps a type of the old service that is no family (bugfix, discovery, …) to its type in the catalog;
// ok is false for anything else.
func Legacy(old string) (string, bool) { t, ok := legacy[old]; return t, ok }

// OldName is a stored type as the memory_* facade 1.19 shows it: a type of the legacy map under its old
// name (dev.bugfix → bugfix, finding → discovery), any other under its short name (dba.migration → migration); a
// short name two vocabularies share (incident) loses its vocabulary there. The legacy map is one to one (TestOldName).
func OldName(stored string) string {
	for old, to := range legacy {
		if to == stored {
			return old
		}
	}
	_, short := split(stored)
	return short
}

// OldNameOf is OldName of a record that may carry the old type it was imported with (0040): that type
// when present — bugfix of a project that is not technical stays bugfix for the facade, not event.
func OldNameOf(stored string, legacy *string) string {
	if legacy != nil && *legacy != "" {
		return *legacy
	}
	return OldName(stored)
}

// OldNames: the stored names an old type filter of the facade matches — the old name as stored before the facade
// mapped its types, and its type of the legacy map.
func OldNames(old string) []string {
	if to, ok := legacy[old]; ok {
		return []string{old, to}
	}
	return []string{old}
}

// FamilyOf is the family of a stored type: of the catalog type, or of the legacy map for events written before the
// family existed (the projector). ok is false for an unknown type.
func FamilyOf(stored string) (string, bool) {
	if t, ok := Lookup(stored); ok {
		return t.Family, true
	}
	if to, ok := legacy[stored]; ok {
		t, ok := Lookup(to)
		return t.Family, ok
	}
	return "", false
}

// TypesOf: every stored type name of a family — the catalog's (live and retired) and the old names of the legacy map
// that land in it — for reads by family on a column without one (a past version's type, as_of).
func TypesOf(family string) []string {
	var out []string
	for _, k := range keys {
		for t, typ := range catalog[k].Types {
			if typ.Family == family {
				out = append(out, Canonical(k, t))
			}
		}
	}
	for old := range legacy {
		if f, ok := FamilyOf(old); ok && f == family {
			out = append(out, old)
		}
	}
	sort.Strings(out)
	return out
}

// Entry is a type of a project's set as the model sees it.
type Entry struct {
	Type        string `json:"type"`  // the stored name
	Short       string `json:"short"` // the name without the vocabulary
	Family      string `json:"family"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// Set is the vocabularies a project works with; general is always in it, unknown keys are ignored.
type Set []string

// NewSet: general first, then the known keys in order, without repeats.
func NewSet(enabled ...string) Set {
	s := Set{General}
	for _, k := range enabled {
		if _, ok := catalog[k]; ok && !slices.Contains(s, k) {
			s = append(s, k)
		}
	}
	return s
}

// members: every live or retired type the set offers — its vocabularies' own types and what they include.
func (s Set) members() []string {
	var out []string
	add := func(name string) {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	for _, k := range s {
		voc, ok := catalog[k]
		if !ok {
			continue
		}
		own := make([]string, 0, len(voc.Types))
		for t := range voc.Types {
			own = append(own, t)
		}
		sort.Strings(own)
		for _, t := range own {
			add(Canonical(k, t))
		}
		for _, inc := range voc.Includes {
			add(inc)
		}
	}
	return out
}

// Entries: the live types of the set (no reserved, no retired), general first — the block of the start package and
// the "allowed" of an error.
func (s Set) Entries() []Entry {
	var out []Entry
	for _, name := range s.members() {
		t, _ := Lookup(name)
		if t.Reserved || t.Retired {
			continue
		}
		_, short := split(name)
		out = append(out, Entry{Type: name, Short: short, Family: t.Family, Title: t.Title.UK, Description: t.Description})
	}
	return out
}

// Block is the vocabulary block of the start package (mandatory): the
// vocabularies of the project's set and their live types, so the model writes with them instead of guessing.
type Block struct {
	Sets  []string `json:"sets"`
	Types []Entry  `json:"types"`
	Hint  string   `json:"hint"`
}

const blockHint = "Record types of this project: pass type as the short or full name. The general types are the " +
	"families. A type you need is missing: vocabulary_list shows the other vocabularies — ask the user, enabling one is " +
	"their decision (fact_record {kind: vocabulary, key, value: on})."

// BlockFull: up to this many types the block carries every description; above it only the general types keep theirs
// and the rest are names (vocabulary_read has the text) — the block is outside budget_tokens, so it bounds itself.
const BlockFull = 20

// Block: the set as the start package shows it.
func (s Set) Block() Block {
	b := Block{Sets: slices.Clone(s), Types: s.Entries(), Hint: blockHint}
	if len(b.Types) > BlockFull {
		for i := range b.Types {
			if b.Types[i].Type != b.Types[i].Short { // not a general type
				b.Types[i].Description = ""
			}
		}
		b.Hint += " Descriptions of the vocabulary types: vocabulary_read."
	}
	return b
}

// Summary is one built-in vocabulary in the catalog (vocabulary.list).
type Summary struct {
	Key      string `json:"key"`
	Version  int    `json:"version"`
	Title    Title  `json:"title"`
	Purpose  string `json:"purpose"`
	Types    int    `json:"types"`              // live own types
	Includes int    `json:"includes,omitempty"` // types taken from other vocabularies
	Enabled  *bool  `json:"enabled,omitempty"`  // in the project's set, when a project was given
}

// Catalog: the built-in vocabularies, general first; enabled is set against the given set (nil — no project).
func Catalog(set Set) []Summary {
	out := make([]Summary, 0, len(keys))
	add := func(k string) {
		v := catalog[k]
		n := 0
		for _, t := range v.Types {
			if !t.Reserved && !t.Retired {
				n++
			}
		}
		s := Summary{Key: k, Version: v.Version, Title: v.Title, Purpose: v.Purpose, Types: n, Includes: len(v.Includes)}
		if set != nil {
			on := slices.Contains(set, k)
			s.Enabled = &on
		}
		out = append(out, s)
	}
	add(General)
	for _, k := range keys {
		if k != General {
			add(k)
		}
	}
	return out
}

// TypeDoc is one type of a vocabulary in full (vocabulary.read).
type TypeDoc struct {
	Name string `json:"type"` // the stored name
	Type
}

// Doc is a vocabulary in full (vocabulary.read): its own types by stored name, sorted, and what it includes.
type Doc struct {
	Key      string    `json:"key"`
	Version  int       `json:"version"`
	Title    Title     `json:"title"`
	Purpose  string    `json:"purpose"`
	Types    []TypeDoc `json:"types"`
	Includes []string  `json:"includes,omitempty"`
}

// Read: a built-in vocabulary in full; ok is false for an unknown key.
func Read(key string) (Doc, bool) {
	v, ok := catalog[key]
	if !ok {
		return Doc{}, false
	}
	own := make([]string, 0, len(v.Types))
	for t := range v.Types {
		own = append(own, t)
	}
	sort.Strings(own)
	d := Doc{Key: key, Version: v.Version, Title: v.Title, Purpose: v.Purpose, Includes: slices.Clone(v.Includes)}
	for _, t := range own {
		d.Types = append(d.Types, TypeDoc{Name: Canonical(key, t), Type: v.Types[t]})
	}
	return d, true
}

// Error is a refused type; Code is the error code of the verb.
type Error struct {
	Code       string   // type_unknown | type_ambiguous | type_retired | type_reserved
	Name       string   // as given
	Candidates []string // type_ambiguous: the full names
	Enable     []string // type_unknown: vocabularies outside the set that have this type
	ReplacedBy string   // type_retired
}

func (e *Error) Error() string {
	switch e.Code {
	case "type_ambiguous":
		return "type " + e.Name + " is ambiguous in this project: " + strings.Join(e.Candidates, ", ")
	case "type_retired":
		msg := "type " + e.Name + " is retired"
		if e.ReplacedBy != "" {
			msg += "; use " + e.ReplacedBy
		}
		return msg
	case "type_reserved":
		return "type " + e.Name + " is written only by the server"
	}
	msg := "type " + e.Name + " is not in this project's vocabularies"
	if len(e.Enable) > 0 {
		msg += "; it is in " + strings.Join(e.Enable, ", ") + ", not enabled for the project"
	}
	return msg
}

// Resolved is a type the set accepts.
type Resolved struct {
	Type   string // the stored name
	Family string
	Legacy bool // given by an old name of the legacy map (warning type_legacy_alias)
	// Outside: the type of the legacy map when its vocabulary is not in the set, so the old name landed on the
	// family of that type instead; empty otherwise.
	Outside string
}

// Resolve checks a type given to a command against the set, in this order: a full
// name, a short name among the set's types, an old name of the legacy map — its type if the set offers it, else
// the type's family (bugfix in a project without dev is an event, not dev.bugfix).
func (s Set) Resolve(name string) (Resolved, error) {
	members := s.members()
	accept := func(stored string, viaLegacy bool) (Resolved, error) {
		t, _ := Lookup(stored)
		switch {
		case t.Reserved:
			return Resolved{}, &Error{Code: "type_reserved", Name: name}
		case t.Retired:
			return Resolved{}, &Error{Code: "type_retired", Name: name, ReplacedBy: t.ReplacedBy}
		}
		return Resolved{Type: stored, Family: t.Family, Legacy: viaLegacy}, nil
	}
	if strings.Contains(name, ".") {
		v, short := split(name)
		stored := Canonical(v, short)
		if _, ok := Lookup(stored); ok && slices.Contains(members, stored) {
			return accept(stored, false)
		}
		if _, ok := Lookup(stored); ok {
			return Resolved{}, &Error{Code: "type_unknown", Name: name, Enable: []string{v}}
		}
		return Resolved{}, &Error{Code: "type_unknown", Name: name}
	}
	var live, retired []string
	for _, m := range members {
		if _, short := split(m); short == name {
			if t, _ := Lookup(m); t.Retired {
				retired = append(retired, m)
			} else {
				live = append(live, m)
			}
		}
	}
	switch {
	case len(live) == 1:
		return accept(live[0], false)
	case len(live) > 1:
		return Resolved{}, &Error{Code: "type_ambiguous", Name: name, Candidates: live}
	case len(retired) > 0:
		return accept(retired[0], false)
	}
	if to, ok := legacy[name]; ok {
		if slices.Contains(members, to) {
			return accept(to, true)
		}
		t, ok := Lookup(to)
		if !ok {
			return Resolved{}, &Error{Code: "type_unknown", Name: name}
		}
		r, err := accept(t.Family, true)
		if err == nil {
			r.Outside = to
		}
		return r, err
	}
	var enable []string
	for _, k := range keys {
		if _, ok := catalog[k].Types[name]; ok && !slices.Contains(s, k) {
			enable = append(enable, k)
		}
	}
	return Resolved{}, &Error{Code: "type_unknown", Name: name, Enable: enable}
}
