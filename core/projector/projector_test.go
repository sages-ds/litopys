package projector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	"litopys.smart-gw.co.ua/core/journal"
)

// The projector must handle exactly the slice-1 event types of the domain spec — no more, no less.
func TestHandlersMatchDomainSliceOne(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "domain", "events.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Events map[string]struct {
			Slice int `yaml:"slice"`
		} `yaml:"events"`
	}
	if err := yaml.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Events) == 0 {
		t.Fatal("no events parsed from domain/events.yaml")
	}
	var want, got []string
	for name, ev := range spec.Events {
		if ev.Slice == 1 {
			want = append(want, name)
		}
	}
	for name := range handlers {
		got = append(got, name)
	}
	sort.Strings(want)
	sort.Strings(got)
	missing, extra := diff(want, got), diff(got, want)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("projector vs domain slice 1:\n  missing handlers: %v\n  handlers not in slice 1: %v", missing, extra)
	}
}

func TestApplyUnknownType(t *testing.T) {
	err := Apply(context.Background(), nil, &journal.Event{Type: "nope.never"})
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("want ErrUnknownType, got %v", err)
	}
}

func diff(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}
