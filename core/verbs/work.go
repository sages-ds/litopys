package verbs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/journal"
	"litopys.smart-gw.co.ua/core/linktypes"
	"litopys.smart-gw.co.ua/core/projector"
)

// projectPurge projects the journal.purged event that journal_purge() appended at seq.
func (c *cmd) projectPurge(seq int64, a *Ack) error {
	events, err := journal.Read(c.ctx, c.tx, c.p.TenantID, seq-1, 1)
	if err != nil {
		return err
	}
	if len(events) != 1 || events[0].Type != "journal.purged" {
		return fmt.Errorf("purge event at seq %d not found", seq)
	}
	if err := projector.Apply(c.ctx, c.tx, &events[0]); err != nil {
		return err
	}
	*a = ack(&events[0])
	return nil
}

// ---------------------------------------------------------------------------
// projects
// ---------------------------------------------------------------------------

type ProjectDeclareIn struct {
	Name    string  `json:"name"`
	Title   *string `json:"title,omitempty"` // the human name; absent = the slug
	Summary *string `json:"summary,omitempty"`
	RepoURL *string `json:"repo_url,omitempty"`
	// DataClass of everything in the project (0023); absent = internal.
	DataClass *string `json:"data_class,omitempty"`
	Scope     *string `json:"scope,omitempty"` // what belongs to the project, one line ≤ 200 (0041)
}

// ProjectDeclare creates a project; idempotent by name within the tenant (already + existing id).
func (s *Service) ProjectDeclare(ctx context.Context, p Principal, in ProjectDeclareIn) (uuid.UUID, Ack, error) {
	if err := checkProjectName("name", in.Name); err != nil {
		return uuid.Nil, Ack{}, err
	}
	if err := checkTitle(in.Title); err != nil {
		return uuid.Nil, Ack{}, err
	}
	cleanOpt(in.Summary)
	if err := optLen("summary", in.Summary, 4000); err != nil {
		return uuid.Nil, Ack{}, err
	}
	if err := optLen("repo_url", in.RepoURL, 512); err != nil {
		return uuid.Nil, Ack{}, err
	}
	if err := checkDataClass("data_class", in.DataClass); err != nil {
		return uuid.Nil, Ack{}, err
	}
	if err := checkScope(in.Scope); err != nil {
		return uuid.Nil, Ack{}, err
	}
	var id uuid.UUID
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		existing, err := c.projectID(in.Name)
		if err == nil {
			id, a.Already = existing, true
			return nil
		}
		if Code(err) != "project_not_found" {
			return err
		}
		if err := c.allowNewProject(in.Name); err != nil {
			return err
		}
		id = uuid.New()
		payload := map[string]any{"name": in.Name}
		if in.Title != nil && *in.Title != in.Name {
			payload["title"] = *in.Title
		}
		if in.Summary != nil {
			payload["summary"] = *in.Summary
		}
		if in.RepoURL != nil {
			payload["repo_url"] = *in.RepoURL
		}
		if in.DataClass != nil {
			payload["data_class"] = *in.DataClass
		}
		if in.Scope != nil && *in.Scope != "" {
			payload["scope"] = *in.Scope
		}
		e, err := c.emit("project.declared", "project", id, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return id, a, err
}

type ProjectAmendIn struct {
	Project string    `json:"project"`
	Name    *string   `json:"name,omitempty"` // rename (the slug)
	Title   *string   `json:"title,omitempty"`
	Summary *string   `json:"summary,omitempty"`
	RepoURL *string   `json:"repo_url,omitempty"`
	Labels  *[]string `json:"labels,omitempty"` // the whole new set (policy)
	// DataClass: the owner may raise or lower the project's class (records keep their own raises).
	DataClass *string `json:"data_class,omitempty"`
	Scope     *string `json:"scope,omitempty"` // 0041; an empty scope never overwrites, as the summary
}

// checkScope: one line of up to 200 characters, cleaned like a title (0041).
func checkScope(s *string) error {
	if s == nil {
		return nil
	}
	*s = cleanLine(*s)
	return optLen("scope", s, 200)
}

// ProjectAmend renames / re-describes a project. An empty summary never overwrites.
func (s *Service) ProjectAmend(ctx context.Context, p Principal, in ProjectAmendIn) (Ack, error) {
	payload := map[string]any{}
	if in.Name != nil {
		if err := checkProjectName("name", *in.Name); err != nil {
			return Ack{}, err
		}
		payload["name"] = *in.Name
	}
	if in.Title != nil {
		if err := checkTitle(in.Title); err != nil {
			return Ack{}, err
		}
		payload["title"] = *in.Title
	}
	cleanOpt(in.Summary)
	if in.Summary != nil && *in.Summary != "" {
		if err := optLen("summary", in.Summary, 4000); err != nil {
			return Ack{}, err
		}
		payload["summary"] = *in.Summary
	}
	if in.RepoURL != nil {
		if err := optLen("repo_url", in.RepoURL, 512); err != nil {
			return Ack{}, err
		}
		payload["repo_url"] = *in.RepoURL
	}
	if in.Labels != nil {
		if len(*in.Labels) > 20 {
			return Ack{}, invalid("labels", "%d items, max 20", len(*in.Labels))
		}
		for i, l := range *in.Labels {
			if !labelRe.MatchString(l) {
				return Ack{}, invalid(fmt.Sprintf("labels[%d]", i), "%q does not match %s", l, labelRe)
			}
		}
		payload["labels"] = nonNilStrings(*in.Labels)
	}
	if in.DataClass != nil {
		if err := checkDataClass("data_class", in.DataClass); err != nil {
			return Ack{}, err
		}
		payload["data_class"] = *in.DataClass
	}
	if err := checkScope(in.Scope); err != nil {
		return Ack{}, err
	}
	if in.Scope != nil && *in.Scope != "" {
		payload["scope"] = *in.Scope
	}
	if len(payload) == 0 {
		return Ack{Unchanged: true}, nil
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		id, err := c.projectID(in.Project)
		if err != nil {
			return err
		}
		if in.Name != nil && *in.Name != in.Project {
			if _, err := c.projectID(*in.Name); err == nil {
				return fail("project_exists", "project %q", *in.Name)
			}
		}
		e, err := c.emit("project.amended", "project", id, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

// ProjectTransition moves active ↔ paused ↔ archived; the same status gives already.
func (s *Service) ProjectTransition(ctx context.Context, p Principal, project, status string, reason *string) (Ack, error) {
	if err := checkEnum("status", status, projectStatus...); err != nil {
		return Ack{}, err
	}
	cleanOpt(reason)
	if err := optLen("reason", reason, 500); err != nil {
		return Ack{}, err
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		id, err := c.projectID(project)
		if err != nil {
			return err
		}
		var cur string
		if err := c.tx.QueryRow(ctx, `SELECT status FROM projection.project WHERE project_id = $1`, id).Scan(&cur); err != nil {
			return err
		}
		if cur == status {
			a.Already = true
			return nil
		}
		payload := map[string]any{"status": status}
		if reason != nil {
			payload["reason"] = *reason
		}
		e, err := c.emit("project.transitioned", "project", id, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

// ---------------------------------------------------------------------------
// tasks — the todo v2 model
// ---------------------------------------------------------------------------

var (
	taskKinds        = []string{"task", "bug", "requirement", "question", "risk", "test"}
	taskOpenStatuses = []string{"backlog", "open", "analysis", "in_progress", "review", "testing", "waiting", "blocked"}
	taskRoles        = []string{"dev", "dba", "ops", "pm", "ba", "qa"}
	severities       = []string{"critical", "major", "minor", "trivial"}
	dateRe           = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// TaskFields lists the settable fields (task.declare / task.amend).
var TaskFields = []string{"title", "body", "kind", "status", "priority", "rank", "role", "owner", "due", "parent",
	"acceptance", "severity", "resolution", "labels", "refs"}

func asInt(name string, v any, min int) (int, error) {
	var n int
	switch x := v.(type) {
	case int:
		n = x
	case int64:
		n = int(x)
	case float64:
		n = int(x)
		if float64(n) != x {
			return 0, invalid(name, "must be an integer")
		}
	default:
		return 0, invalid(name, "must be an integer")
	}
	if n < min {
		return 0, invalid(name, "must be >= %d", min)
	}
	return n, nil
}

func asStrings(name string, v any, maxItems, maxLen int) ([]string, error) {
	var out []string
	switch x := v.(type) {
	case []string:
		out = x
	case []any:
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, invalid(name, "items must be strings")
			}
			out = append(out, s)
		}
	default:
		return nil, invalid(name, "must be an array of strings")
	}
	return out, checkList(name, out, maxItems, maxLen)
}

// checkTaskField validates one task field value (nil = clear) and returns the value to store.
func checkTaskField(name string, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	s, isStr := v.(string)
	needStr := func() error {
		if !isStr {
			return invalid(name, "must be a string")
		}
		return nil
	}
	switch name {
	case "title":
		if err := needStr(); err != nil {
			return nil, err
		}
		s = cleanLine(s)
		return s, checkLen(name, s, 1, 500)
	case "body":
		if err := needStr(); err != nil {
			return nil, err
		}
		s = cleanText(s)
		return s, checkLen(name, s, 0, 8192)
	case "acceptance", "resolution":
		if err := needStr(); err != nil {
			return nil, err
		}
		s = cleanText(s)
		return s, checkLen(name, s, 0, 4000)
	case "owner":
		if err := needStr(); err != nil {
			return nil, err
		}
		return s, checkLen(name, s, 1, 100)
	case "owner_project": // a project name here; ownerProject resolves it to the id the event carries
		if err := needStr(); err != nil {
			return nil, err
		}
		return s, checkLen(name, s, 1, 100)
	case "assignee": // exactly one of actor_id, role, agent; the actor is checked by c.assignee
		m, ok := v.(map[string]any)
		if !ok || len(m) != 1 {
			return nil, invalid(name, "must be an object with exactly one of actor_id, role, agent")
		}
		for k, x := range m {
			str, ok := x.(string)
			if !ok {
				return nil, invalid(name+"."+k, "must be a string")
			}
			switch k {
			case "actor_id":
				if _, err := uuid.Parse(str); err != nil {
					return nil, invalid(name+".actor_id", "not a uuid")
				}
			case "role":
				if err := checkEnum(name+".role", str, taskRoles...); err != nil {
					return nil, err
				}
			case "agent":
				if !agentRoleRe.MatchString(str) {
					return nil, invalid(name+".agent", "lower-case letters, digits and '-', at most 63")
				}
			default:
				return nil, invalid(name, "unknown key %q: one of actor_id, role, agent", k)
			}
		}
		return m, nil
	case "data_class": // raises the project's class for this task (0023)
		if err := needStr(); err != nil {
			return nil, err
		}
		return s, checkEnum(name, s, dataClasses...)
	case "kind", "status", "priority", "role", "severity":
		if err := needStr(); err != nil {
			return nil, err
		}
		allowed := map[string][]string{"kind": taskKinds, "status": taskOpenStatuses, "priority": taskPriorities,
			"role": taskRoles, "severity": severities}[name]
		return s, checkEnum(name, s, allowed...)
	case "due":
		if !isStr || !dateRe.MatchString(s) {
			return nil, invalid(name, "must be YYYY-MM-DD")
		}
		if _, err := time.Parse("2006-01-02", s); err != nil {
			return nil, invalid(name, "not a date")
		}
		return s, nil
	case "rank":
		return asInt(name, v, -1_000_000)
	case "parent":
		return asInt(name, v, 1)
	case "labels":
		return asStrings(name, v, 20, 60)
	case "refs":
		return asStrings(name, v, 20, 300)
	}
	return nil, invalid(name, "unknown task field")
}

type TaskDeclareIn struct {
	Project  string         `json:"project"`
	Fields   map[string]any `json:"fields"` // title required; status (open ones) defaults to open
	Mentions []EntityRef    `json:"-"`      // the internal refs of Fields["refs"], resolved by the edge
}

type TaskDeclareOut struct {
	TaskID  uuid.UUID `json:"task_id"`
	Num     int       `json:"num"`
	Ref     string    `json:"ref"` // <project>/T-<num>
	Existed bool      `json:"existed,omitempty"`
	// Ignored: fields of an existed task's call that were not applied — task_amend changes them
	Ignored []string `json:"ignored_fields,omitempty"`
	Ack
}

// ownerProject replaces the owner_project name of a payload with the project id: the executing project
// of a handoff — another visible project of the owner, never the task's own.
func (c *cmd) ownerProject(payload map[string]any, own uuid.UUID) error {
	name, ok := payload["owner_project"].(string)
	if !ok {
		return nil // absent, or nil = clear
	}
	id, err := c.projectID(name)
	if err != nil {
		return err
	}
	if id == own {
		return invalid("owner_project", "the task's own project; owner_project names another project that executes the task")
	}
	payload["owner_project"] = id
	return nil
}

var agentRoleRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// assignee checks that an assignee actor exists in the tenant and is not revoked.
func (c *cmd) assignee(payload map[string]any) error {
	m, ok := payload["assignee"].(map[string]any)
	if !ok {
		return nil
	}
	actor, ok := m["actor_id"].(string)
	if !ok {
		return nil
	}
	var live bool
	err := c.tx.QueryRow(c.ctx, `SELECT revoked_at IS NULL FROM projection.actor WHERE actor_id = $1 AND tenant_id = $2`, actor, c.p.TenantID).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !live) {
		return invalid("assignee.actor_id", "no live actor %s", actor)
	}
	return err
}

func (c *cmd) parentExists(projectID uuid.UUID, num int) error {
	var ok bool
	if err := c.tx.QueryRow(c.ctx, `SELECT EXISTS (SELECT 1 FROM projection.task WHERE project_id = $1 AND num = $2 AND NOT hidden)`,
		projectID, num).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return fail("parent_not_found", "task #%d", num)
	}
	return nil
}

// refsNotLinks warns when a task's refs name other tasks it has no depends_on / handoff link to: refs only
// mention, waiting_on and blocking count links — models took refs for a dependency (roast r2, Р2-10).
func (c *cmd) refsNotLinks(taskID uuid.UUID, mentions []EntityRef) ([]string, error) {
	var targets []uuid.UUID
	for _, m := range mentions {
		if m.EntityType == "task" && m.EntityID != taskID {
			targets = append(targets, m.EntityID)
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}
	var unlinked int
	if err := c.tx.QueryRow(c.ctx, `SELECT count(*) FROM unnest($2::uuid[]) AS t(id)
	        WHERE NOT EXISTS (SELECT 1 FROM projection.link l WHERE l.from_type = 'task' AND l.from_id = $1
	                            AND l.to_type = 'task' AND l.to_id = t.id AND NOT l.retired
	                            AND l.relation IN ('depends_on', 'handoff'))`, taskID, targets).Scan(&unlinked); err != nil {
		return nil, err
	}
	if unlinked == 0 {
		return nil, nil
	}
	return []string{fmt.Sprintf("refs_not_dependency: refs name %d task(s) this task has no link to — refs only mention, "+
		"waiting_on and blocking count links; to wait on a task call link_assert {type: depends_on (handoff when another "+
		"project does the work), from: this task, to: that task}", unlinked)}, nil
}

// TaskDeclare adds a task; num = max+1 in the project under the project row lock. An open task with
// the same title is returned instead (existed).
func (s *Service) TaskDeclare(ctx context.Context, p Principal, in TaskDeclareIn) (TaskDeclareOut, error) {
	var out TaskDeclareOut
	payload := map[string]any{}
	for k, v := range in.Fields {
		if k == "resolution" {
			return out, invalid(k, "set on task.close")
		}
		val, err := checkTaskField(k, v)
		if err != nil {
			return out, err
		}
		if val != nil {
			payload[k] = val
		}
	}
	title, _ := payload["title"].(string)
	if title == "" {
		return out, invalid("title", "required")
	}
	if _, ok := payload["status"]; !ok {
		payload["status"] = "open"
	}
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		projectID, err := c.projectID(in.Project)
		if err != nil {
			return err
		}
		if _, err := c.tx.Exec(ctx, `SELECT 1 FROM projection.project WHERE project_id = $1 FOR UPDATE`, projectID); err != nil {
			return err
		}
		slug, err := c.slug(projectID)
		if err != nil {
			return err
		}
		err = c.tx.QueryRow(ctx, `SELECT task_id, num FROM projection.task
		        WHERE project_id = $1 AND title = $2 AND open AND NOT hidden ORDER BY num LIMIT 1`, projectID, title).Scan(&out.TaskID, &out.Num)
		if err == nil {
			out.Existed, out.Already, out.Ref = true, true, ProjectRef(slug, "T", out.Num)
			// the other fields of this call change nothing: say so instead of a silent "already" (roast r1)
			for k := range in.Fields {
				if k != "title" {
					out.Ignored = append(out.Ignored, k)
				}
			}
			slices.Sort(out.Ignored)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if parent, ok := payload["parent"].(int); ok {
			if err := c.parentExists(projectID, parent); err != nil {
				return err
			}
		}
		if err := c.ownerProject(payload, projectID); err != nil {
			return err
		}
		if err := c.assignee(payload); err != nil {
			return err
		}
		if out.Num, err = c.nextTaskNum(projectID); err != nil {
			return err
		}
		payload["project_id"], payload["num"] = projectID, out.Num
		out.Ref = ProjectRef(slug, "T", out.Num)
		out.TaskID = uuid.New()
		e, err := c.emit("task.declared", "task", out.TaskID, nil, nil, payload)
		if err != nil {
			return err
		}
		out.Ack = ack(e)
		if err := c.setMentions(EntityRef{EntityType: "task", EntityID: out.TaskID}, in.Mentions); err != nil {
			return err
		}
		out.Warnings, err = c.refsNotLinks(out.TaskID, in.Mentions)
		return err
	})
	return out, err
}

// task locks a live (not retracted) task of the tenant.
func (c *cmd) task(id uuid.UUID) (projectID uuid.UUID, open bool, revision int, err error) {
	err = c.tx.QueryRow(c.ctx, `SELECT project_id, open, revision FROM projection.task
	        WHERE task_id = $1 AND tenant_id = $2 AND NOT hidden FOR UPDATE`, id, c.p.TenantID).Scan(&projectID, &open, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, 0, fail("task_not_found", "task %s", id)
	}
	return
}

// TaskByNum resolves a task id from (project, num) — the facade addresses tasks by num.
func (s *Service) TaskByNum(ctx context.Context, p Principal, project string, num int) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.DB.QueryRow(ctx, `SELECT t.task_id FROM projection.task t JOIN projection.project pr ON pr.project_id = t.project_id
	        WHERE pr.tenant_id = $1 AND pr.name = $2 AND t.num = $3 AND NOT t.hidden`, p.TenantID, project, num).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fail("task_not_found", "%s #%d", project, num)
	}
	return id, err
}

type TaskAmendIn struct {
	TaskID     uuid.UUID      `json:"task_id"`
	IfRevision *int           `json:"if_revision,omitempty"`
	Fields     map[string]any `json:"fields"` // only these change; a nil value clears the field
	Mentions   []EntityRef    `json:"-"`      // the internal refs of Fields["refs"], resolved by the edge
}

// TaskAmend changes the given fields (status: open ones only — closing is task.close).
func (s *Service) TaskAmend(ctx context.Context, p Principal, in TaskAmendIn) (Ack, error) {
	payload := map[string]any{}
	for k, v := range in.Fields {
		if (k == "title" || k == "status" || k == "data_class") && v == nil {
			return Ack{}, invalid(k, "cannot be cleared")
		}
		val, err := checkTaskField(k, v)
		if err != nil {
			return Ack{}, err
		}
		payload[k] = val
	}
	if len(payload) == 0 {
		return Ack{}, invalid("task.amend", "nothing to change")
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		projectID, _, revision, err := c.task(in.TaskID)
		if err != nil {
			return err
		}
		if in.IfRevision != nil && *in.IfRevision != revision {
			return fail("revision_conflict", "revision is %d, not %d", revision, *in.IfRevision)
		}
		if parent, ok := payload["parent"].(int); ok {
			if err := c.parentExists(projectID, parent); err != nil {
				return err
			}
		}
		if err := c.ownerProject(payload, projectID); err != nil {
			return err
		}
		if err := c.assignee(payload); err != nil {
			return err
		}
		if dc, ok := payload["data_class"].(string); ok {
			if err := c.raiseOnly("task", "task_id", in.TaskID, dc); err != nil {
				return err
			}
		}
		e, err := c.emit("task.amended", "task", in.TaskID, nil, nil, payload)
		if err != nil {
			return err
		}
		a = ack(e)
		if _, ok := in.Fields["refs"]; ok { // a new list (or null) replaces the mentions too
			if err := c.setMentions(EntityRef{EntityType: "task", EntityID: in.TaskID}, in.Mentions); err != nil {
				return err
			}
			a.Warnings, err = c.refsNotLinks(in.TaskID, in.Mentions)
			return err
		}
		return nil
	})
	return a, err
}

// TaskClose closes with done | dropped (+ resolution); a closed task gives already.
func (s *Service) TaskClose(ctx context.Context, p Principal, id uuid.UUID, outcome string, resolution *string) (Ack, error) {
	if err := checkEnum("outcome", outcome, taskOutcomes...); err != nil {
		return Ack{}, err
	}
	cleanOpt(resolution)
	if err := optLen("resolution", resolution, 4000); err != nil {
		return Ack{}, err
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		_, open, _, err := c.task(id)
		if err != nil {
			return err
		}
		if !open {
			a.Already = true
			return nil
		}
		payload := map[string]any{"outcome": outcome}
		if resolution != nil {
			payload["resolution"] = *resolution
		}
		e, err := c.emit("task.closed", "task", id, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

// TaskReopen reopens a closed task into an open status (default open); an open one gives already.
func (s *Service) TaskReopen(ctx context.Context, p Principal, id uuid.UUID, status *string, reason *string) (Ack, error) {
	if status != nil {
		if err := checkEnum("status", *status, taskOpenStatuses...); err != nil {
			return Ack{}, err
		}
	}
	cleanOpt(reason)
	if err := optLen("reason", reason, 500); err != nil {
		return Ack{}, err
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		_, open, _, err := c.task(id)
		if err != nil {
			return err
		}
		if open {
			a.Already = true
			return nil
		}
		payload := map[string]any{}
		if status != nil {
			payload["status"] = *status
		}
		if reason != nil {
			payload["reason"] = *reason
		}
		e, err := c.emit("task.reopened", "task", id, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

// TaskRetract removes a mistaken task from the lists (history stays; num is never reused).
func (s *Service) TaskRetract(ctx context.Context, p Principal, id uuid.UUID, reason string) (Ack, error) {
	reason = cleanText(reason)
	if err := checkLen("reason", reason, 1, 500); err != nil {
		return Ack{}, err
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if _, _, _, err := c.task(id); err != nil {
			return err
		}
		e, err := c.emit("task.retracted", "task", id, nil, nil, map[string]any{"reason": reason})
		if err != nil {
			return err
		}
		a = ack(e)
		return c.askDependents(id, "retracted") // a live task (c.task) goes: its dependents are asked
	})
	return a, err
}

// ---------------------------------------------------------------------------
// facts
// ---------------------------------------------------------------------------

type FactRecordIn struct {
	Project   string  `json:"project"`
	Kind      string  `json:"kind"`
	Key       string  `json:"key"`
	Value     *string `json:"value,omitempty"`
	SecretRef *string `json:"secret_ref,omitempty"`
	// Source: the record the fact was taken from, R-N or an id; the API resolves it into SourceID
	Source   *string    `json:"source,omitempty"`
	SourceID *uuid.UUID `json:"-"`
}

// FactRecord upserts (project, kind, key). credential takes only secret_ref; the same value gives unchanged.
func (s *Service) FactRecord(ctx context.Context, p Principal, in FactRecordIn) (Ack, error) {
	if err := checkEnum("kind", in.Kind, factKinds...); err != nil {
		return Ack{}, err
	}
	if err := checkLen("key", in.Key, 1, 200); err != nil {
		return Ack{}, err
	}
	if err := optLen("value", in.Value, 8192); err != nil {
		return Ack{}, err
	}
	if err := optLen("secret_ref", in.SecretRef, 512); err != nil {
		return Ack{}, err
	}
	if in.Kind == "credential" && in.Value != nil {
		return Ack{}, fail("credential_value_forbidden", "credential %q: store secret_ref, never the value", in.Key)
	}
	if in.Kind == "credential" && in.SecretRef == nil {
		return Ack{}, invalid("secret_ref", "required for kind=credential")
	}
	if in.Kind == "retention" {
		if _, err := ParseRetention(in.Value); err != nil {
			return Ack{}, err
		}
	}
	if in.Kind == "vocabulary" {
		if err := checkVocabularyFact(in.Key, in.Value); err != nil {
			return Ack{}, err
		}
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		projectID, err := c.projectID(in.Project)
		if err != nil {
			return err
		}
		if in.Kind == "retention" {
			if err := c.checkRetention(in.Value); err != nil {
				return err
			}
		}
		var same, confirmed bool
		err = c.tx.QueryRow(ctx, `SELECT NOT retired AND value IS NOT DISTINCT FROM $4 AND secret_ref IS NOT DISTINCT FROM $5
		        AND source_id IS NOT DISTINCT FROM $6, confirmed_at IS NOT NULL
		        FROM projection.fact WHERE project_id = $1 AND kind = $2 AND key = $3`,
			projectID, in.Kind, in.Key, in.Value, in.SecretRef, in.SourceID).Scan(&same, &confirmed)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// a human-only retention label written again through the API needs the person's confirmation again, even
		// with the same value: only an unconfirmed one may stay as it is
		humanOnly := false
		if in.Kind == "retention" {
			r, _ := ParseRetention(in.Value)
			humanOnly = r.humanOnly()
		}
		if same && !(humanOnly && confirmed) {
			a.Unchanged = true
			if humanOnly {
				a.Warnings = append(a.Warnings, needsConfirmation(in.Project, in.Key))
			}
			return nil
		}
		payload := map[string]any{"project_id": projectID, "kind": in.Kind, "key": in.Key}
		if in.Value != nil {
			payload["value"] = *in.Value
		}
		if in.SecretRef != nil {
			payload["secret_ref"] = *in.SecretRef
		}
		if in.SourceID != nil {
			payload["source_id"] = *in.SourceID
		}
		e, err := c.emit("fact.recorded", "fact", FactID(projectID, in.Kind, in.Key), nil, nil, payload)
		if err == nil {
			a = ack(e)
			if humanOnly {
				a.Warnings = append(a.Warnings, needsConfirmation(in.Project, in.Key))
			}
		}
		return err
	})
	return a, err
}

// FactRetire retires (project, kind, key); an already retired fact gives already.
func (s *Service) FactRetire(ctx context.Context, p Principal, project, kind, key string, reason *string) (Ack, error) {
	if err := checkEnum("kind", kind, factKinds...); err != nil {
		return Ack{}, err
	}
	cleanOpt(reason)
	if err := optLen("reason", reason, 500); err != nil {
		return Ack{}, err
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		projectID, err := c.projectID(project)
		if err != nil {
			return err
		}
		var retired bool
		err = c.tx.QueryRow(ctx, `SELECT retired FROM projection.fact WHERE project_id = $1 AND kind = $2 AND key = $3`,
			projectID, kind, key).Scan(&retired)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail("fact_not_found", "%s/%s", kind, key)
		}
		if err != nil {
			return err
		}
		if retired {
			a.Already = true
			return nil
		}
		payload := map[string]any{}
		if reason != nil {
			payload["reason"] = *reason
		}
		e, err := c.emit("fact.retired", "fact", FactID(projectID, kind, key), nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

// ---------------------------------------------------------------------------
// links · consumers
// ---------------------------------------------------------------------------

type EntityRef struct {
	EntityType string    `json:"entity_type"`
	EntityID   uuid.UUID `json:"entity_id"`
}

// linkRules applies the declared rules of a relation (link-types.yaml) that need the database: the project rule of
// two task ends, the reverse edge of a symmetric relation (returned — re-asserting it is a no-op) and cycles of an
// acyclic one, along the whole chain of that relation.
func (c *cmd) linkRules(relation string, lt linktypes.Type, from, to EntityRef, bothTasks bool) (uuid.UUID, error) {
	if bothTasks && lt.Projects != "any" {
		var same bool
		if err := c.tx.QueryRow(c.ctx, `SELECT a.project_id = b.project_id FROM projection.task a, projection.task b
		        WHERE a.task_id = $1 AND b.task_id = $2`, from.EntityID, to.EntityID).Scan(&same); err != nil {
			return uuid.Nil, err
		}
		if lt.Projects == "different" && same {
			return uuid.Nil, invalid("to", "%s links tasks of different projects; within one project use depends_on or parent", relation)
		}
		if lt.Projects == "same" && !same {
			return uuid.Nil, invalid("to", "%s links tasks of one project", relation)
		}
	}
	if lt.Symmetric {
		var rev uuid.UUID
		err := c.tx.QueryRow(c.ctx, `SELECT link_id FROM projection.link WHERE from_type = $1 AND from_id = $2 AND to_type = $3
		        AND to_id = $4 AND relation = $5 AND NOT retired`, to.EntityType, to.EntityID, from.EntityType, from.EntityID, relation).Scan(&rev)
		if err == nil {
			return rev, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, err
		}
	}
	if lt.Acyclic { // is "from" already reachable from "to"? Then from -> to closes a cycle.
		var cycle bool
		if err := c.tx.QueryRow(c.ctx, `WITH RECURSIVE reach(id, depth) AS (
		        SELECT $1::uuid, 0
		        UNION
		        SELECT l.to_id, r.depth + 1 FROM projection.link l JOIN reach r ON l.from_id = r.id
		         WHERE l.relation = $2 AND NOT l.retired AND r.depth < 64)
		      SELECT EXISTS (SELECT 1 FROM reach WHERE id = $3)`, to.EntityID, relation, from.EntityID).Scan(&cycle); err != nil {
			return uuid.Nil, err
		}
		if cycle {
			return uuid.Nil, invalid("to", "%s %s -> %s would close a cycle: the target already leads back to the source; keep one direction", relation, from.EntityType, to.EntityType)
		}
	}
	return uuid.Nil, nil
}

// LinkAssert asserts a typed edge; idempotent by (from, to, relation) while the edge is live.
func (s *Service) LinkAssert(ctx context.Context, p Principal, from, to EntityRef, relation string, note *string) (uuid.UUID, Ack, error) {
	for _, r := range []EntityRef{from, to} {
		if err := checkEnum("entity_type", r.EntityType, entityTypes...); err != nil {
			return uuid.Nil, Ack{}, err
		}
	}
	if err := checkEnum("relation", relation, linktypes.Relations()...); err != nil {
		return uuid.Nil, Ack{}, err
	}
	if err := optLen("note", note, 500); err != nil {
		return uuid.Nil, Ack{}, err
	}
	var id uuid.UUID
	var a Ack
	lt, _ := linktypes.Get(relation)
	if !lt.Allows(from.EntityType, to.EntityType) {
		return uuid.Nil, Ack{}, invalid("relation", "%s links %s; not %s -> %s", relation, lt.EndsText(), from.EntityType, to.EntityType)
	}
	if from == to {
		return uuid.Nil, Ack{}, invalid("to", "a %s cannot be linked to itself", from.EntityType)
	}
	bothTasks := from.EntityType == "task" && to.EntityType == "task"
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		for _, r := range []EntityRef{from, to} {
			switch r.EntityType {
			case "statement":
				if _, _, err := c.statement(r.EntityID); err != nil {
					return err
				}
			case "project": // own and visible (RLS of 0008/0010)
				var one int
				if err := c.tx.QueryRow(ctx, `SELECT 1 FROM projection.project WHERE project_id = $1 AND owner_id = $2`, r.EntityID, c.me).Scan(&one); err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return fail("project_not_found", "project %s", r.EntityID)
					}
					return err
				}
			case "workflow": // a workflow of an own, visible project (0020)
				var one int
				if err := c.tx.QueryRow(ctx, `SELECT 1 FROM projection.workflow w JOIN projection.project pr ON pr.project_id = w.project_id
				        WHERE w.workflow_id = $1 AND pr.owner_id = $2`, r.EntityID, c.me).Scan(&one); err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return fail("workflow_not_found", "workflow %s", r.EntityID)
					}
					return err
				}
			case "task": // a live task of an own, visible project — tasks of different projects may be linked
				var one int
				if err := c.tx.QueryRow(ctx, `SELECT 1 FROM projection.task t JOIN projection.project pr ON pr.project_id = t.project_id
				        WHERE t.task_id = $1 AND pr.owner_id = $2 AND NOT t.hidden`, r.EntityID, c.me).Scan(&one); err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return fail("task_not_found", "task %s", r.EntityID)
					}
					return err
				}
			}
		}
		reverse, err := c.linkRules(relation, lt, from, to, bothTasks)
		if err != nil {
			return err
		}
		if reverse != uuid.Nil { // symmetric: B -> A is already A -> B
			id, a.Already = reverse, true
			return nil
		}
		var oldNote *string
		err = c.tx.QueryRow(ctx, `SELECT link_id, note FROM projection.link
		        WHERE from_type = $1 AND from_id = $2 AND to_type = $3 AND to_id = $4 AND relation = $5 AND NOT retired`,
			from.EntityType, from.EntityID, to.EntityType, to.EntityID, relation).Scan(&id, &oldNote)
		if err == nil && (note == nil || (oldNote != nil && *oldNote == *note)) {
			a.Already = true
			return nil
		}
		if err == nil { // the same live edge with a new note: re-asserting replaces the note
			a.Already = true
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		id = uuid.New()
		payload := map[string]any{"from": from, "to": to, "relation": relation}
		if note != nil {
			payload["note"] = *note
		}
		e, err := c.emit("link.asserted", "link", id, nil, nil, payload)
		if err == nil {
			already := a.Already
			a = ack(e)
			a.Already = already
		}
		return err
	})
	return id, a, err
}

// LinkRetire retires a live edge; a retired one gives already.
func (s *Service) LinkRetire(ctx context.Context, p Principal, id uuid.UUID, reason *string) (Ack, error) {
	cleanOpt(reason)
	if err := optLen("reason", reason, 500); err != nil {
		return Ack{}, err
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		var author uuid.UUID
		var retired bool
		err := c.tx.QueryRow(ctx, `SELECT actor_id, retired FROM projection.link WHERE link_id = $1`, id).Scan(&author, &retired)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail("link_not_found", "link %s", id)
		}
		if err != nil {
			return err
		}
		if err := c.mine(author); err != nil {
			return err
		}
		if retired {
			a.Already = true
			return nil
		}
		payload := map[string]any{}
		if reason != nil {
			payload["reason"] = *reason
		}
		e, err := c.emit("link.retired", "link", id, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

// ConsumerCheckpoint moves a named consumer's cursor forward; seq <= current gives unchanged.
func (s *Service) ConsumerCheckpoint(ctx context.Context, p Principal, consumer string, seq int64) (Ack, error) {
	if err := checkLen("consumer", consumer, 1, 100); err != nil {
		return Ack{}, err
	}
	if !consumerNameRe.MatchString(consumer) {
		return Ack{}, invalid("consumer", "%q does not match %s", consumer, consumerNameRe)
	}
	if seq < 1 {
		return Ack{}, invalid("seq", "must be >= 1")
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		var head int64
		if err := c.tx.QueryRow(ctx, `SELECT last_seq FROM journal_head WHERE tenant_id = $1`, p.TenantID).Scan(&head); err != nil {
			return err
		}
		if seq > head {
			return fail("seq_out_of_range", "seq %d is beyond the journal head %d", seq, head)
		}
		var cur int64
		err := c.tx.QueryRow(ctx, `SELECT seq FROM projection.consumer WHERE tenant_id = $1 AND owner_id = $2 AND consumer = $3`,
			p.TenantID, c.me, consumer).Scan(&cur)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if seq <= cur {
			a.Unchanged = true
			return nil
		}
		e, err := c.emit("consumer.cursor_set", "consumer", ConsumerID(p.TenantID, c.me, consumer), nil, nil,
			map[string]any{"consumer": consumer, "seq": seq})
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

// checkTitle: a human project title — any characters, spaces collapsed, 1..200.
func checkTitle(t *string) error {
	if t == nil {
		return nil
	}
	*t = cleanLine(*t)
	return checkLen("title", *t, 1, 200)
}
