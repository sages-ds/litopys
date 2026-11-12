package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/textnorm"
	"litopys.smart-gw.co.ua/core/vocab"
)

// The *_resume family: one meaning everywhere — return what is needed to continue work and
// attach this client's session in the project, opening one when needed. Thin wrappers over the context
// bundle (query.ContextPack) and scoped selections (query/resume.go).

func init() {
	handle("project.resume", projectResume)
	handle("session.resume", sessionResume)
	handle("task.resume", taskResume)
	handle("topic.resume", topicResume)
}

// NextSteps is how many open tasks project_resume names as next_steps.
const NextSteps = 3

// OpenOutcomes is how many unfinished outcomes project_resume shows.
const OpenOutcomes = 5

// ProjectLinks is how many links to other projects project_resume shows (as the 1.19 start: 10).
const ProjectLinks = 10

func nextSteps(tasks []query.TaskRow) []string {
	out := []string{}
	for _, t := range tasks {
		if len(out) == NextSteps {
			break
		}
		s := fmt.Sprintf("#%d %s [%s", t.Num, t.Title, t.Status)
		if t.Priority != nil {
			s += ", " + *t.Priority
		}
		out = append(out, s+"]")
	}
	if len(out) == 0 {
		out = append(out, "no open tasks: ask the user what to work on, then record it with task_declare")
	}
	return out
}

func projectResume(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
	var in struct {
		Project      string          `json:"project,omitempty"`
		AutoCreate   bool            `json:"auto_create,omitempty"`
		BudgetTokens int             `json:"budget_tokens,omitempty"`
		Agent        string          `json:"agent,omitempty"`
		Client       json.RawMessage `json:"client,omitempty"`
		OpenSession  *bool           `json:"open_session,omitempty"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	if err := checkAgent(in.Agent); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Project) == "" {
		rows, err := a.Q.Catalog(ctx, c.query())
		if err != nil {
			return nil, err
		}
		return map[string]any{"catalog": nonNil(rows), "rules": rulesBlock(),
			"next_steps": []string{"pick a project with the user, then call project_resume with its name or title"}}, nil
	}
	slug, id, err := a.project(ctx, c, in.Project)
	created := false
	if apiCode(err) == "project_not_found" && in.AutoCreate {
		if slug, created, err = a.declareFromTitle(ctx, c, in.Project, nil); err == nil {
			_, id, err = a.project(ctx, c, slug)
		}
	}
	if err != nil {
		return nil, err
	}
	session, err := a.attachUnless(ctx, c, in.OpenSession, slug, id, in.Agent, in.Client)
	if err != nil {
		return nil, err
	}
	bundle, err := a.Q.ContextPack(ctx, c.query(), slug, in.BudgetTokens)
	if err != nil {
		return nil, err
	}
	// the record types of the project's set (mandatory)
	set, err := vocab.ProjectSet(ctx, a.DB, id)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"session": session, "bundle": bundle, "next_steps": nextSteps(bundle.Tasks), "vocabulary": set.Block(),
		"rules": rulesBlock()} // the minimum every client gets, plugin or not
	// what the last sessions left hanging: the latest outcome per subject that is not done
	open, err := a.Q.OpenOutcomes(ctx, c.query(), id, OpenOutcomes)
	if err != nil {
		return nil, err
	}
	if len(open) > 0 {
		out["open_outcomes"] = open
	}
	// deletion proposals waiting for the person: the agent asks for the GO, never gives it
	dl, err := a.Q.DeletionList(ctx, c.query(), query.DeletionListIn{Project: slug, State: "open", Limit: 10})
	if err != nil {
		return nil, err
	}
	if len(dl.Items) > 0 {
		out["pending_deletions"] = dl.Items
	}
	// retention labels waiting for the person's confirmation in the viewer: pass the link on
	if pc, err := a.pendingConfirmations(ctx, c, slug, id); err != nil {
		return nil, err
	} else if len(pc) > 0 {
		out["pending_confirmations"] = pc
	}
	// links to other projects: the "one owner + mirror" rule of cross-project work rests on them
	links, more, err := a.Q.ProjectLinks(ctx, c.query(), id, ProjectLinks)
	if err != nil {
		return nil, err
	}
	if len(links) > 0 {
		lb := map[string]any{"items": links}
		if more > 0 {
			lb["more"] = more
		}
		out["links"] = lb
	}
	if created {
		out["project_created"] = true
	}
	return out, nil
}

func sessionResume(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
	var in struct {
		SessionID    *uuid.UUID `json:"session_id,omitempty"`
		Project      string     `json:"project,omitempty"`
		BudgetTokens int        `json:"budget_tokens,omitempty"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	var s *openSession
	var err error
	switch {
	case in.SessionID != nil:
		if s, err = a.sessionByID(ctx, c, *in.SessionID); err == nil && s == nil {
			err = errorf("session_not_found", "session %s", *in.SessionID)
		}
	case in.Project != "":
		s, err = a.latestSession(ctx, c, in.Project)
	default:
		err = invalid("pass session_id or project")
	}
	if err != nil {
		return nil, err
	}
	var summary, current *string
	var next []string
	if err := a.DB.QueryRow(ctx, `SELECT summary, current, next_steps FROM projection.context WHERE context_id = $1`, s.id).Scan(&summary, &current, &next); err != nil {
		return nil, err
	}
	outcomes, err := a.Q.SessionOutcomes(ctx, c.query(), s.id)
	if err != nil {
		return nil, err
	}
	statements, err := a.Q.SessionStatements(ctx, c.query(), s.id, 30)
	if err != nil {
		return nil, err
	}
	tasks, err := a.Q.RunTasks(ctx, c.query(), s.run)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"statements": nonNil(statements), "tasks_touched": nonNil(tasks)}
	if summary != nil {
		out["summary"] = *summary
	}
	if current != nil {
		out["current"] = *current
	}
	if len(next) > 0 {
		out["next_steps"] = next
	}
	if len(outcomes) > 0 {
		out["outcomes"] = outcomes
	}
	this := SessionRef{SessionID: s.id, Project: s.projName, OpenedAt: &s.opened, ClosedAt: s.closed}
	if s.closed == nil { // an open session is continued, whoever of the person's clients opened it
		c.run = s.run
		this.Reused = true
		out["session"] = this
	} else { // a closed one is summarized; work goes on in this client's session
		next, err := a.attach(ctx, c, s.projName, s.project, "", nil)
		if err != nil {
			return nil, err
		}
		out["session"], out["resumed_from"] = next, this
	}
	if cat, err := a.Q.Catalog(ctx, c.query()); err == nil {
		for _, r := range cat {
			if r.Name == s.projName {
				out["project"] = r
			}
		}
	}
	return out, nil
}

// latestSession: the person's latest session in a project, this client's first.
func (a *API) latestSession(ctx context.Context, c *Caller, project string) (*openSession, error) {
	_, pid, err := a.project(ctx, c, project)
	if err != nil {
		return nil, err
	}
	var s openSession
	err = a.DB.QueryRow(ctx, `SELECT x.context_id, x.run_id, x.project_id, x.opened_at, x.closed_at, COALESCE(x.client->>'grant', ''), p.name
	   FROM projection.context x JOIN projection.project p ON p.project_id = x.project_id
	  WHERE x.project_id = $1 AND x.owner_id = $2
	  ORDER BY (x.client->>'grant' = $3) IS TRUE DESC, x.last_event_at DESC LIMIT 1`, pid, c.Owner, c.grant()).
		Scan(&s.id, &s.run, &s.project, &s.opened, &s.closed, &s.grant, &s.projName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &Error{Code: "session_not_found", Message: "no session in project " + project, Hint: "call project_resume to start one"}
	}
	return &s, err
}

func taskResume(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
	var in struct {
		Project      string `json:"project"`
		Num          int    `json:"num"`
		BudgetTokens int    `json:"budget_tokens,omitempty"`
		OpenSession  *bool  `json:"open_session,omitempty"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	if in.Num < 1 {
		return nil, invalid("num: a task number, 1 or more")
	}
	slug, id, err := a.project(ctx, c, in.Project)
	if err != nil {
		return nil, err
	}
	task, parent, children, err := a.Q.TaskFamily(ctx, c.query(), slug, in.Num)
	if err != nil {
		return nil, err
	}
	related, err := a.Q.TaskMentions(ctx, c.query(), slug, in.Num, 20)
	if err != nil {
		return nil, err
	}
	session, err := a.attachUnless(ctx, c, in.OpenSession, slug, id, "", nil)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"task": task, "children": nonNil(children), "related": nonNil(related), "session": session}
	if parent != nil {
		out["parent"] = parent
	}
	latest, err := a.Q.LatestOutcome(ctx, c.query(), task.TaskID)
	if err != nil {
		return nil, err
	}
	if latest != nil {
		out["latest_outcome"] = latest
	}
	return out, nil
}

// TopicGroupMax caps each group of topic_resume.
const TopicGroupMax = 12

func topicResume(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
	var in struct {
		Topic        string `json:"topic"`
		Project      string `json:"project,omitempty"`
		Mode         string `json:"mode,omitempty"`
		BudgetTokens int    `json:"budget_tokens,omitempty"`
		OpenSession  *bool  `json:"open_session,omitempty"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	if len([]rune(strings.TrimSpace(in.Topic))) < 2 {
		return nil, invalid("topic: at least 2 characters")
	}
	switch in.Mode {
	case "", "recall", "preflight":
	default:
		return nil, invalid(`mode: "recall" or "preflight"`)
	}
	var slug string
	var pid uuid.UUID
	if in.Project != "" {
		var err error
		if slug, pid, err = a.project(ctx, c, in.Project); err != nil {
			return nil, err
		}
	}
	res, err := a.Q.Search(ctx, c.query(), query.SearchIn{Query: in.Topic, Project: slug, Limit: 50})
	if err != nil {
		return nil, err
	}
	groups := map[string][]query.IndexRow{"decisions": {}, "proposals": {}, "findings": {}, "questions": {}, "replaced": {}, "outcomes": {}, "resolved": {}}
	// answered questions and removed risks go to "resolved", not to the open ones (roast r1)
	ids := make([]uuid.UUID, len(res.Items))
	for i, r := range res.Items {
		ids[i] = r.StatementID
	}
	resolved, err := a.Q.Resolved(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range res.Items {
		// by family, so dba.migration counts as a decision of its vocabulary; the rest are findings
		family, _ := vocab.FamilyOf(r.Type)
		g := "findings"
		switch {
		case r.Status == "superseded" || r.Status == "retracted":
			g = "replaced"
		case resolved[r.StatementID]:
			g = "resolved"
		case family == "decision":
			g = "decisions"
		case family == "proposal":
			g = "proposals"
		case family == "question":
			g = "questions"
		case family == "outcome":
			g = "outcomes"
		}
		if len(groups[g]) < TopicGroupMax {
			groups[g] = append(groups[g], r)
		}
	}
	// every grouped row carries its match: a partial one in "decisions" says what it misses
	out := map[string]any{"relaxed": res.Relaxed, "hidden": res.Hidden}
	if res.Coverage != "" {
		out["coverage"] = res.Coverage
	}
	if len(res.UnknownWords) > 0 {
		out["unknown_words"] = res.UnknownWords
	}
	for k, v := range groups {
		out[k] = v
	}
	// the topic as a label of outcomes (topic:<name>)
	latest, err := a.Q.TopicOutcome(ctx, c.query(), in.Topic)
	if err != nil {
		return nil, err
	}
	if latest != nil {
		out["latest_outcome"] = latest
	}
	if in.Mode == "preflight" && slug != "" {
		// by the stems of the topic's words, not the whole phrase: "плита газ індукція" finds the task "…газовику на
		// відключення плити" and the fact about the budget (roast r1)
		words := topicStems(in.Topic)
		tl, err := a.Q.TaskList(ctx, c.query(), query.TaskListIn{Project: slug, AnyWords: words, Limit: 20})
		if err != nil {
			return nil, err
		}
		facts, err := a.Q.FactsAbout(ctx, c.query(), slug, words)
		if err != nil {
			return nil, err
		}
		out["open_tasks"], out["facts"] = nonNil(tl.Items), nonNil(facts)
	}
	if slug != "" {
		session, err := a.attachUnless(ctx, c, in.OpenSession, slug, pid, "", nil)
		if err != nil {
			return nil, err
		}
		if session != nil {
			out["session"] = session
		}
	}
	return out, nil
}

// topicStems: the distinct word stems of a topic (at least 3 letters — shorter ones match everything).
func topicStems(topic string) []string {
	var out []string
	for _, st := range textnorm.Analyze(topic).WordStems {
		if len([]rune(st)) >= 3 && !slices.Contains(out, st) {
			out = append(out, st)
		}
	}
	return out
}

// attachUnless: the session of a *_resume, unless the caller only looks (open_session: false) — then nothing is opened
// and no session is left to close (roast r1).
func (a *API) attachUnless(ctx context.Context, c *Caller, open *bool, slug string, projectID uuid.UUID, agent string, client json.RawMessage) (*SessionRef, error) {
	if open != nil && !*open {
		return nil, nil
	}
	return a.attach(ctx, c, slug, projectID, agent, client)
}
