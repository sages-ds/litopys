package vocab

import (
	"errors"
	"slices"
	"testing"
)

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// TestCatalog: general is the families, every type has a family, the legacy map lands on live types of the same
// family the map promises.
func TestCatalog(t *testing.T) {
	g, ok := Get(General)
	if !ok {
		t.Fatal("no general vocabulary")
	}
	for _, f := range Families() {
		if g.Types[f].Family != f {
			t.Errorf("general.%s: family %q", f, g.Types[f].Family)
		}
	}
	if !g.Types["outcome"].Reserved {
		t.Error("outcome must be reserved")
	}
	for _, k := range Keys() {
		v, _ := Get(k)
		for tk, typ := range v.Types {
			if !IsFamily(typ.Family) || typ.Description == "" {
				t.Errorf("%s.%s: family %q, description %q", k, tk, typ.Family, typ.Description)
			}
		}
	}
	for old, want := range map[string]string{"discovery": "finding", "bugfix": "event", "feature": "event", "refactor": "event", "config": "event"} {
		if f, ok := FamilyOf(old); !ok || f != want {
			t.Errorf("legacy %s: family %q (%v), want %s", old, f, ok, want)
		}
	}
	for _, kept := range []string{"decision", "note", "question", "proposal", "document", "outcome"} {
		if f, ok := FamilyOf(kept); !ok || f != kept {
			t.Errorf("old type %s stays a family: %q %v", kept, f, ok)
		}
	}
	if to, _ := Legacy("bugfix"); to != "dev.bugfix" {
		t.Errorf("legacy bugfix -> %q", to)
	}
	if _, ok := FamilyOf("nonsense"); ok {
		t.Error("an unknown type has a family")
	}
}

// TestOldName: the facade 1.19 names — the legacy map backwards, else the short name.
func TestOldName(t *testing.T) {
	for stored, want := range map[string]string{"dev.bugfix": "bugfix", "finding": "discovery", "infra.config": "config",
		"decision": "decision", "bugfix": "bugfix", "dba.migration": "migration", "event": "event"} {
		if got := OldName(stored); got != want {
			t.Errorf("OldName(%s) = %s, want %s", stored, got, want)
		}
	}
	// an imported record keeps the old type it came with (0040): event of a project that is not technical is bugfix
	bugfix, empty := "bugfix", ""
	if got := OldNameOf("event", &bugfix); got != "bugfix" {
		t.Errorf("OldNameOf(event, bugfix) = %s", got)
	}
	if got := OldNameOf("dev.bugfix", nil); got != "bugfix" {
		t.Errorf("OldNameOf(dev.bugfix, nil) = %s", got)
	}
	if got := OldNameOf("finding", &empty); got != "discovery" {
		t.Errorf("OldNameOf(finding, \"\") = %s", got)
	}
	// OldName walks the legacy map backwards: it must stay one to one, or the facade's output is not deterministic
	seen := map[string]string{}
	for old, to := range legacy {
		if prev, ok := seen[to]; ok {
			t.Errorf("legacy map: %s and %s both map to %s", prev, old, to)
		}
		seen[to] = old
	}
	if got := OldNames("bugfix"); len(got) != 2 || got[1] != "dev.bugfix" {
		t.Errorf("OldNames(bugfix) = %v", got)
	}
	if got := OldNames("decision"); len(got) != 1 {
		t.Errorf("OldNames(decision) = %v", got)
	}
}

// TestResolve: the order — full name, short name in the set, legacy alias — and every refusal.
func TestResolve(t *testing.T) {
	cases := []struct {
		set        Set
		name       string
		want, code string
		legacy     bool
	}{
		{NewSet(), "decision", "decision", "", false},
		{NewSet(), "general.decision", "decision", "", false},
		{NewSet("dba"), "dba.migration", "dba.migration", "", false},
		{NewSet("dba"), "migration", "dba.migration", "", false},
		{NewSet(), "migration", "", "type_unknown", false},     // dba not enabled
		{NewSet(), "dba.migration", "", "type_unknown", false}, // the same, by full name
		{NewSet("dba", "infra"), "incident", "", "type_ambiguous", false},
		{NewSet("dba", "infra"), "infra.incident", "infra.incident", "", false},
		{NewSet("itops"), "change", "infra.change", "", false}, // through includes
		{NewSet("itops"), "infra.change", "infra.change", "", false},
		{NewSet("itops"), "infra.access", "", "type_unknown", false}, // infra is not in the set, only two of its types
		{NewSet(), "outcome", "", "type_reserved", false},
		{NewSet(), "bugfix", "event", "", true}, // old name, dev not enabled: its family
		{NewSet(), "config", "event", "", true},
		{NewSet("dba"), "feature", "event", "", true},
		{NewSet("dev"), "bugfix", "dev.bugfix", "", false},
		{NewSet("infra"), "config", "infra.config", "", false},
		{NewSet(), "discovery", "finding", "", true},
		{NewSet(), "nonsense", "", "type_unknown", false},
		{NewSet("no-such-vocabulary"), "decision", "decision", "", false},
	}
	for _, c := range cases {
		r, err := c.set.Resolve(c.name)
		if code(err) != c.code || r.Type != c.want || r.Legacy != c.legacy {
			t.Errorf("%v %q: %+v, %v; want %q code %q legacy %v", c.set, c.name, r, err, c.want, c.code, c.legacy)
		}
	}
	if r, _ := NewSet().Resolve("bugfix"); r.Outside != "dev.bugfix" || r.Family != "event" {
		t.Errorf("legacy alias outside the set names its type: %+v", r)
	}
	if r, _ := NewSet().Resolve("discovery"); r.Outside != "" {
		t.Errorf("legacy alias inside the set: %+v", r)
	}
	_, err := NewSet().Resolve("migration")
	var e *Error
	if !errors.As(err, &e) || !slices.Contains(e.Enable, "dba") {
		t.Errorf("type_unknown names the vocabularies to enable: %+v", e)
	}
	_, err = NewSet("dba", "infra").Resolve("incident")
	if !errors.As(err, &e) || !slices.Equal(e.Candidates, []string{"dba.incident", "infra.incident"}) {
		t.Errorf("type_ambiguous candidates: %+v", e)
	}
}

// TestCatalogAndRead: vocabulary.list puts general first and marks enabled only against a set; vocabulary.read gives
// stored names; the block of the start package carries the set and its entries.
func TestCatalogAndRead(t *testing.T) {
	all := Catalog(nil)
	if len(all) != len(Keys()) || all[0].Key != General || all[0].Enabled != nil {
		t.Errorf("catalog: %d entries, first %+v", len(all), all[0])
	}
	for _, s := range Catalog(NewSet("dba")) {
		if want := s.Key == General || s.Key == "dba"; *s.Enabled != want {
			t.Errorf("%s enabled %v", s.Key, *s.Enabled)
		}
	}
	d, ok := Read("dba")
	if !ok || d.Types[0].Name == "" || d.Types[0].Name[:4] != "dba." {
		t.Errorf("read dba: %+v", d)
	}
	if g, _ := Read(General); g.Types[0].Name != "decision" {
		t.Errorf("general types are stored without a prefix: %s", g.Types[0].Name)
	}
	if _, ok := Read("nonsense"); ok {
		t.Error("an unknown vocabulary was read")
	}
	b := NewSet("dba").Block()
	if !slices.Equal(b.Sets, []string{General, "dba"}) || len(b.Types) != len(NewSet("dba").Entries()) || b.Hint == "" ||
		b.Types[len(b.Types)-1].Description == "" {
		t.Errorf("block: %+v", b)
	}
	// above BlockFull types only the general ones keep their descriptions (the block is outside budget_tokens)
	big := NewSet(Keys()...).Block()
	if len(big.Types) <= BlockFull {
		t.Fatalf("every vocabulary gives only %d types", len(big.Types))
	}
	for _, e := range big.Types {
		if general := e.Type == e.Short; (e.Description != "") != general {
			t.Errorf("%s: description %q", e.Type, e.Description)
		}
	}
}

// TestTypesOf: a family's names cover the general type, vocabulary types and the old names that land in it.
func TestTypesOf(t *testing.T) {
	dec := TypesOf("decision")
	for _, want := range []string{"decision", "exec.board-decision"} {
		if !slices.Contains(dec, want) {
			t.Errorf("decision family lacks %s: %v", want, dec)
		}
	}
	if ev := TypesOf("event"); !slices.Contains(ev, "bugfix") || !slices.Contains(ev, "dev.bugfix") || slices.Contains(ev, "decision") {
		t.Errorf("event family: %v", ev)
	}
}

// TestEntries: the start package lists the live types of the set — general's families without outcome, then the
// vocabularies' own and included types.
func TestEntries(t *testing.T) {
	var got []string
	for _, e := range NewSet("itops").Entries() {
		got = append(got, e.Type)
	}
	want := []string{"decision", "document", "event", "finding", "note", "proposal", "question",
		"itops.problem", "itops.release", "infra.change", "infra.incident"}
	if !slices.Equal(got, want) {
		t.Errorf("entries: %v\nwant %v", got, want)
	}
}
