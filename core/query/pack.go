package query

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MaxRecentStatements mirrors START_CONTEXT_MAX_OBS of the current server (30).
var MaxRecentStatements = 30

type LastSummary struct {
	ContextID uuid.UUID       `json:"context_id"`
	ClosedAt  *time.Time      `json:"closed_at,omitempty"`
	Summary   *string         `json:"summary,omitempty"`
	Client    json.RawMessage `json:"client,omitempty"`
}

type RecentContext struct {
	ContextID  uuid.UUID       `json:"context_id"`
	ClosedAt   *time.Time      `json:"closed_at,omitempty"`
	Client     json.RawMessage `json:"client,omitempty"`
	HasContent bool            `json:"has_content"`
}

type Budget struct {
	TokensLimit int      `json:"tokens_limit"`
	TokensUsed  int      `json:"tokens_used"`
	Truncated   []string `json:"truncated"`
}

// Bundle is read-models.yaml context_bundle.shape.
type Bundle struct {
	Project          ProjectRow      `json:"project"`
	Current          *string         `json:"current,omitempty"`
	LastSummary      *LastSummary    `json:"last_summary,omitempty"`
	Tasks            []TaskRow       `json:"tasks"`
	Due              *Due            `json:"due,omitempty"` // overdue and coming open tasks; absent when none
	Decisions        []IndexRow      `json:"decisions"`
	RecentStatements []IndexRow      `json:"recent_statements"`
	RecentContexts   []RecentContext `json:"recent_contexts"`
	Facts            []FactRow       `json:"facts"`
	Layers           *Layers         `json:"layers,omitempty"` // the resolved stack; absent when empty
	Consumers        []ConsumerRow   `json:"consumers"`
	Budget           Budget          `json:"budget"`
	Hidden           []Hidden        `json:"hidden"`
}

// estimateTokens is deliberately rough and conservative: UTF-8 bytes / 4 over-counts Cyrillic
// (2 bytes per letter), so a bundle within budget by this measure is within budget in practice.
func estimateTokens(v any) int {
	b, _ := json.Marshal(v)
	return (len(b) + 3) / 4
}

// ContextPack is the start bundle without opening a context (get_latest_session_state of the facade;
// also the bundle context.open returns). Truncation order when over budget: recent statements down to
// MinRecentStatements → old facts → tasks → the rest of the statements → decisions down to MinDecisions →
// recent contexts → the rest of the decisions; current, last_summary and the layers are never cut.
func (s *Service) ContextPack(ctx context.Context, p Principal, project string, budgetTokens int) (Bundle, error) {
	b := Bundle{Tasks: []TaskRow{}, Decisions: []IndexRow{}, RecentStatements: []IndexRow{}, RecentContexts: []RecentContext{},
		Facts: []FactRow{}, Consumers: []ConsumerRow{}, Hidden: []Hidden{}}
	b.Budget = Budget{TokensLimit: clamp(budgetTokens, DefaultPackBudget, 500, 32000), Truncated: []string{}}
	ctx, root, err := s.enter(ctx, p)
	if err != nil {
		return b, err
	}
	b.Hidden = hiddenOf(ctx)
	err = s.DB.QueryRow(ctx, `SELECT `+projectCols+provCols("j")+`
		  FROM projection.project pr`+provJoin("j", "pr.tenant_id", "pr.last_seq")+`
		 WHERE pr.tenant_id = $1 AND pr.name = $2`, p.TenantID, project).Scan(b.Project.dest()...)
	if isNoRows(err) {
		return b, fail("project_not_found", "project %q", project)
	}
	if err != nil {
		return b, err
	}
	b.Current = b.Project.Current

	// last MEANINGFUL context (has a statement or a checkpoint); empty tails are skipped.
	var ls LastSummary
	err = s.DB.QueryRow(ctx, `SELECT c.context_id, c.closed_at, c.summary, c.client FROM projection.context c
		 WHERE c.project_id = $1 AND c.closed_at IS NOT NULL AND c.has_content AND `+mineSQL("c.actor_id", "$2")+`
		 ORDER BY c.closed_at DESC LIMIT 1`, b.Project.ProjectID, root).Scan(&ls.ContextID, &ls.ClosedAt, &ls.Summary, &ls.Client)
	if err == nil {
		b.LastSummary = &ls
	} else if !isNoRows(err) {
		return b, err
	}

	rows, err := s.DB.Query(ctx, `SELECT c.context_id, c.closed_at, c.client, c.has_content FROM projection.context c
		 WHERE c.project_id = $1 AND c.closed_at IS NOT NULL AND `+mineSQL("c.actor_id", "$2")+`
		 ORDER BY c.closed_at DESC LIMIT 3`, b.Project.ProjectID, root)
	if err != nil {
		return b, err
	}
	if b.RecentContexts, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (RecentContext, error) {
		var x RecentContext
		return x, r.Scan(&x.ContextID, &x.ClosedAt, &x.Client, &x.HasContent)
	}); err != nil {
		return b, err
	}
	if b.Tasks, err = s.tasks(ctx, p, b.Project.ProjectID, true); err != nil {
		return b, err
	}
	b.Due = dueOf(b.Tasks, time.Now())
	// the current decisions get their own block, newest by creation: dated payments and session outcomes pushed
	// every decision out of the recent statements (roast r1)
	dr, err := s.Search(ctx, p, SearchIn{Project: project, Family: "decision", ExcludeSuperseded: true, ByCreation: true, Limit: MaxDecisions})
	if err != nil {
		return b, err
	}
	b.Decisions = dr.Items
	inDecisions := make(map[uuid.UUID]bool, len(dr.Items))
	for _, d := range dr.Items {
		inDecisions[d.StatementID] = true
	}
	// by creation, not by the event date: a payment due next week is not the newest thing said
	sr, err := s.Search(ctx, p, SearchIn{Project: project, ExcludeSuperseded: true, ByCreation: true, Limit: 50 + len(dr.Items)})
	if err != nil {
		return b, err
	}
	for _, r := range sr.Items {
		// outcomes come as open_outcomes of project_resume; the decisions are above
		if r.Type == "outcome" || inDecisions[r.StatementID] {
			continue
		}
		if len(b.RecentStatements) == MaxRecentStatements {
			break
		}
		b.RecentStatements = append(b.RecentStatements, r)
	}
	facts, err := s.FactList(ctx, p, project, "", "")
	if err != nil {
		return b, err
	}
	b.Facts = facts[:0]
	for _, f := range facts {
		// a folder of some machine says nothing about the project to the model; layer rules go to the stack
		if f.Kind != "binding" && !LayerKind(f.Kind) {
			b.Facts = append(b.Facts, f)
		}
	}
	if l, err := s.layers(ctx, p, root, b.Project.ProjectID, project); err != nil {
		return b, err
	} else if len(l.Stack) > 0 || len(l.Spaces) > 0 || l.Global != "" {
		b.Layers = &l
	}
	c, err := s.ConsumerList(ctx, p, "")
	if err != nil {
		return b, err
	}
	for _, x := range c.Items {
		// a lagging consumer that still moves is a signal for the session; one silent for weeks is the operator's
		// (consumer_list, the status page) — every client read it as an alarm (roast r1)
		if x.Pending > 0 && time.Since(x.UpdatedAt) < StaleConsumer {
			b.Consumers = append(b.Consumers, x)
		}
	}

	fitBudget(&b)
	return b, nil
}

// fitBudget cuts the bundle to its budget: statements (oldest first) down to MinRecentStatements → facts
// (oldest first) → tasks (lowest priority first) → the rest of the statements → decisions down to
// MinDecisions → contexts → the rest of the decisions. The newest
// statements outrank old facts: a project with many facts must not start with no recent statements.
func fitBudget(b *Bundle) {
	before := map[string]int{}
	var order []string
	cut := func(name string, n func() int, drop func(), floor int) {
		if _, seen := before[name]; !seen {
			before[name] = n()
			order = append(order, name)
		}
		for n() > floor && estimateTokens(b) > b.Budget.TokensLimit {
			drop()
		}
	}
	dropStatement := func() { b.RecentStatements = b.RecentStatements[:len(b.RecentStatements)-1] }
	nStatements := func() int { return len(b.RecentStatements) }
	cut("recent_statements", nStatements, dropStatement, MinRecentStatements)
	cut("facts", func() int { return len(b.Facts) }, func() { b.Facts = dropOldestFact(b.Facts) }, 0)
	cut("tasks", func() int { return len(b.Tasks) }, func() { b.Tasks = b.Tasks[:len(b.Tasks)-1] }, 0)
	cut("recent_statements", nStatements, dropStatement, 0)
	nDecisions := func() int { return len(b.Decisions) }
	dropDecision := func() { b.Decisions = b.Decisions[:len(b.Decisions)-1] }
	cut("decisions", nDecisions, dropDecision, MinDecisions)
	cut("recent_contexts", func() int { return len(b.RecentContexts) },
		func() { b.RecentContexts = b.RecentContexts[:len(b.RecentContexts)-1] }, 0)
	cut("decisions", nDecisions, dropDecision, 0)
	after := map[string]int{"recent_statements": len(b.RecentStatements), "facts": len(b.Facts),
		"tasks": len(b.Tasks), "decisions": len(b.Decisions), "recent_contexts": len(b.RecentContexts)}
	for _, name := range order {
		if after[name] < before[name] {
			b.Budget.Truncated = append(b.Budget.Truncated, fmt.Sprintf("%s: %d → %d", name, before[name], after[name]))
		}
	}
	b.Budget.TokensUsed = estimateTokens(b)
}

// DefaultPackBudget: the start pack's token budget when the caller names none. 5000, not 3000: provenance is about
// half of every row, and a small project's pack cut its recent statements at 3000 (roast r2, Р2-7). Interim — the
// final call comes with fields.
var DefaultPackBudget = 5000

// StaleConsumer: a consumer whose cursor has not moved this long stays out of the start bundle. A day, not a
// week: an imported cursor (moved once by the migration) still read as an alarm two days later (roast r2, Р2-5);
// consumer_list and the status page show every consumer.
var StaleConsumer = 24 * time.Hour

// Due: open tasks whose due date has passed or comes within DueSoonDays — what the next conversation must not miss
// (roast r1: due dates were only an order). Dates are days in Europe/Kyiv, as the task's due.
type Due struct {
	Overdue []DueTask `json:"overdue,omitempty"`
	Soon    []DueTask `json:"soon,omitempty"`
}

type DueTask struct {
	Num   int    `json:"num"`
	Title string `json:"title"`
	Due   string `json:"due"`
	Days  int    `json:"days"` // to the due date: negative — overdue by that many days
}

// DueSoonDays and DueMax: the window of "soon" and how many tasks each list keeps.
var DueSoonDays, DueMax = 7, 10

var kyiv = func() *time.Location {
	if l, err := time.LoadLocation("Europe/Kyiv"); err == nil {
		return l
	}
	return time.UTC
}()

func dueOf(tasks []TaskRow, now time.Time) *Due {
	y, m, d := now.In(kyiv).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	var out Due
	for _, t := range tasks {
		if t.Due == nil {
			continue
		}
		day, err := time.Parse("2006-01-02", *t.Due)
		if err != nil {
			continue
		}
		days := int(day.Sub(today).Hours() / 24)
		x := DueTask{Num: t.Num, Title: t.Title, Due: *t.Due, Days: days}
		switch {
		case days < 0 && len(out.Overdue) < DueMax:
			out.Overdue = append(out.Overdue, x)
		case days >= 0 && days <= DueSoonDays && len(out.Soon) < DueMax:
			out.Soon = append(out.Soon, x)
		}
	}
	if len(out.Overdue)+len(out.Soon) == 0 {
		return nil
	}
	slices.SortFunc(out.Overdue, func(a, b DueTask) int { return a.Days - b.Days })
	slices.SortFunc(out.Soon, func(a, b DueTask) int { return a.Days - b.Days })
	return &out
}

// MinRecentStatements: the newest statements kept before any fact is cut.
var MinRecentStatements = 5

// MaxDecisions: the current decisions of the start bundle; MinDecisions of them outlive everything but
// current, last_summary and the layers.
var MaxDecisions, MinDecisions = 15, 3

// dropOldestFact removes the fact with the oldest effective time, keeping the order of the rest.
func dropOldestFact(facts []FactRow) []FactRow {
	if len(facts) == 0 {
		return facts
	}
	oldest := 0
	for i, f := range facts {
		if f.Provenance.EffectiveAt.Before(facts[oldest].Provenance.EffectiveAt) {
			oldest = i
		}
	}
	return append(facts[:oldest], facts[oldest+1:]...)
}
