package query

import (
	"strings"
	"testing"
	"time"
)

// TestFitBudgetOrder: over budget, the newest statements outrank old facts — statements are cut down to
// MinRecentStatements first, then the oldest facts, then tasks, then the rest of the statements. No database.
func TestFitBudgetOrder(t *testing.T) {
	long := strings.Repeat("x", 400)
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mk := func(nStatements, nFacts, nTasks int) Bundle {
		b := Bundle{Tasks: []TaskRow{}, RecentStatements: []IndexRow{}, RecentContexts: []RecentContext{},
			Facts: []FactRow{}, Consumers: []ConsumerRow{}, Hidden: []Hidden{}}
		for i := 0; i < nStatements; i++ {
			b.RecentStatements = append(b.RecentStatements, IndexRow{Title: long, EffectiveAt: day.Add(time.Duration(-i) * time.Hour)})
		}
		for i := 0; i < nFacts; i++ {
			v := long
			// facts come by name/kind/key, not by age: the oldest one sits in the middle
			b.Facts = append(b.Facts, FactRow{Key: string(rune('a' + i)), Value: &v,
				Provenance: Provenance{EffectiveAt: day.AddDate(0, 0, -((i + 2) % nFacts))}})
		}
		for i := 0; i < nTasks; i++ {
			b.Tasks = append(b.Tasks, TaskRow{Num: i + 1, Title: long})
		}
		return b
	}

	// the case of the live check: 30 statements, 5 facts, a budget that holds about 15 rows
	b := mk(30, 5, 3)
	b.Budget = Budget{TokensLimit: 1700, Truncated: []string{}}
	fitBudget(&b)
	if len(b.RecentStatements) != MinRecentStatements {
		t.Fatalf("statements: %d, want the floor %d (facts go before them); truncated %v", len(b.RecentStatements), MinRecentStatements, b.Budget.Truncated)
	}
	if len(b.Facts) >= 5 {
		t.Fatalf("facts were not cut: %d; truncated %v", len(b.Facts), b.Budget.Truncated)
	}
	// fact i is (i+2)%5 days back: the oldest is i=2 (key c, 4 days back)
	for _, f := range b.Facts {
		if f.Key == "c" {
			t.Fatalf("the oldest fact (c, 4 days back) must go first: %+v", b.Facts)
		}
	}
	if b.Budget.TokensUsed > b.Budget.TokensLimit {
		t.Fatalf("over budget: %d > %d", b.Budget.TokensUsed, b.Budget.TokensLimit)
	}
	if len(b.Budget.Truncated) == 0 || !strings.HasPrefix(b.Budget.Truncated[0], "recent_statements: 30 → 5") {
		t.Fatalf("truncated report: %v", b.Budget.Truncated)
	}

	// a tiny budget: everything cuttable goes, statements reported once with the total cut
	tiny := mk(30, 5, 3)
	tiny.Budget = Budget{TokensLimit: 100, Truncated: []string{}}
	fitBudget(&tiny)
	if len(tiny.RecentStatements) != 0 || len(tiny.Facts) != 0 || len(tiny.Tasks) != 0 {
		t.Fatalf("tiny budget: %d statements, %d facts, %d tasks", len(tiny.RecentStatements), len(tiny.Facts), len(tiny.Tasks))
	}
	want := []string{"recent_statements: 30 → 0", "facts: 5 → 0", "tasks: 3 → 0"}
	if strings.Join(tiny.Budget.Truncated, "|") != strings.Join(want, "|") {
		t.Fatalf("tiny truncated: %v, want %v", tiny.Budget.Truncated, want)
	}

	// within budget: nothing is cut
	ok := mk(3, 2, 1)
	ok.Budget = Budget{TokensLimit: 32000, Truncated: []string{}}
	fitBudget(&ok)
	if len(ok.Budget.Truncated) != 0 || len(ok.RecentStatements) != 3 || len(ok.Facts) != 2 {
		t.Fatalf("within budget: %+v", ok.Budget)
	}
}

// TestFitRecall: over budget_tokens a step package loses recall: statements from the last selector back; the
// inputs and other selectors stay whole, a selector left empty says why. No database.
func TestFitRecall(t *testing.T) {
	long := strings.Repeat("x", 400)
	rows := func(n int) []PackStatement {
		out := make([]PackStatement, n)
		for i := range out {
			out[i] = PackStatement{Title: long}
		}
		return out
	}
	mk := func() StepPackOut {
		return StepPackOut{Params: map[string]any{}, Inputs: map[string]any{"collect": long}, Context: []PackItem{
			{Selector: "recall:first", Value: rows(5)},
			{Selector: "steps.collect.output", Value: long},
			{Selector: "recall:second", Value: rows(5)}}}
	}

	// room for about three statements: the second selector goes first, the first keeps its head
	p := mk()
	fitRecall(&p, 700)
	if estimateTokens(p) > 700 {
		t.Fatalf("over budget: %d", estimateTokens(p))
	}
	if p.Context[2].Value != nil || p.Context[2].Truncated != 5 || p.Context[2].Unresolved == "" {
		t.Fatalf("the last recall selector: %+v", p.Context[2])
	}
	first, _ := p.Context[0].Value.([]PackStatement)
	if len(first) == 0 || len(first)+p.Context[0].Truncated != 5 {
		t.Fatalf("the first recall selector: %d kept, %d truncated", len(first), p.Context[0].Truncated)
	}
	if p.Context[1].Value != long || p.Inputs["collect"] != long {
		t.Fatal("a step output was cut")
	}

	// within budget: nothing is cut
	ok := mk()
	fitRecall(&ok, 32000)
	for _, it := range ok.Context {
		if it.Truncated != 0 || it.Unresolved != "" {
			t.Fatalf("within budget: %+v", it)
		}
	}
}

// TestDueOf: overdue and coming tasks by their due day in Europe/Kyiv, nearest first; none — no block.
func TestDueOf(t *testing.T) {
	d := func(s string) *string { return &s }
	now := time.Date(2026, 11, 1, 23, 30, 0, 0, time.UTC) // 01:30 on 2 November in Kyiv
	tasks := []TaskRow{{Num: 1, Title: "paid late", Due: d("2026-10-30")}, {Num: 2, Title: "today", Due: d("2026-11-02")},
		{Num: 3, Title: "next week", Due: d("2026-11-09")}, {Num: 4, Title: "later", Due: d("2026-11-20")}, {Num: 5, Title: "no date"},
		{Num: 6, Title: "yesterday", Due: d("2026-11-01")}}
	got := dueOf(tasks, now)
	if got == nil || len(got.Overdue) != 2 || got.Overdue[0].Num != 1 || got.Overdue[0].Days != -3 || got.Overdue[1].Num != 6 {
		t.Fatalf("overdue: %+v", got)
	}
	if len(got.Soon) != 2 || got.Soon[0].Num != 2 || got.Soon[0].Days != 0 || got.Soon[1].Num != 3 {
		t.Fatalf("soon: %+v", got.Soon)
	}
	if dueOf([]TaskRow{{Num: 1, Due: d("2027-01-01")}}, now) != nil {
		t.Error("a far due date made a block")
	}
}
