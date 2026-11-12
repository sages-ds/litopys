package query

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/linktypes"
)

// TaskRow is read-models.yaml tasks.row — the todo v2 model.
type TaskRow struct {
	TaskID   uuid.UUID `json:"task_id"`
	Ref      string    `json:"ref"` // <project>/T-<num>
	Project  string    `json:"project"`
	Num      int       `json:"num"`
	Title    string    `json:"title"`
	Body     *string   `json:"body,omitempty"`
	Kind     string    `json:"kind"`
	Status   string    `json:"status"`
	Open     bool      `json:"open"`
	Priority *string   `json:"priority,omitempty"`
	Rank     *int      `json:"rank,omitempty"`
	Role     *string   `json:"role,omitempty"`
	Owner    *string   `json:"owner,omitempty"`
	// OwnerProject: the project that executes the task when it is not its own — a handoff (0019)
	OwnerProject *string `json:"owner_project,omitempty"`
	// Assignee: who does it — {"actor_id"} | {"role"} | {"agent"} (0019)
	Assignee json.RawMessage `json:"assignee,omitempty"`
	// WaitingOn / Blocking: open targets of the task's depends_on / handoff links, open tasks waiting on it
	WaitingOn int `json:"waiting_on"`
	Blocking  int `json:"blocking"`
	// WaitingOnRefs / BlockingRefs name those tasks: a count alone left the blocked side guessing who waits (roast r2, Р2-3)
	WaitingOnRefs []string   `json:"waiting_on_refs,omitempty"`
	BlockingRefs  []string   `json:"blocking_refs,omitempty"`
	Due           *string    `json:"due,omitempty"` // YYYY-MM-DD
	Parent        *int       `json:"parent,omitempty"`
	Acceptance    *string    `json:"acceptance,omitempty"`
	Severity      *string    `json:"severity,omitempty"`
	Resolution    *string    `json:"resolution,omitempty"`
	Labels        []string   `json:"labels,omitempty"`
	Refs          []string   `json:"refs,omitempty"`
	ClosedAt      *time.Time `json:"closed_at,omitempty"`
	Revision      int        `json:"revision"`
	HasDetails    bool       `json:"has_details"`
	CreatedAt     time.Time  `json:"created_at"`  // valid time of task.declared (the contract's created_at)
	DeclaredBy    uuid.UUID  `json:"declared_by"` // actor of task.declared (the contract's writer)
	Provenance    Provenance `json:"provenance"`
}

// satisfiable: the relations a task waits on until their target closes (link-types.yaml on_end_closed), as an
// SQL array literal — the names come from the generated table, not from input.
var satisfiable = "ARRAY['" + strings.Join(linktypes.Satisfiable(), "','") + "']::text[]"

// "що далі" order of todo v2: work in flight first, backlog last; closed after open.
const taskOrder = `array_position(ARRAY['in_progress','review','testing','analysis','open','blocked','waiting','backlog','done','dropped'], t.status),
	t.rank NULLS LAST, array_position(ARRAY['high','medium','low'], t.priority) NULLS LAST, t.num`

type TaskListIn struct {
	Project      string   `json:"project"`
	State        string   `json:"state,omitempty"` // open (default) | closed | all
	Status       []string `json:"status,omitempty"`
	Kind         []string `json:"kind,omitempty"`
	Role         string   `json:"role,omitempty"`
	Owner        string   `json:"owner,omitempty"`
	AssigneeRole string   `json:"assignee_role,omitempty"`
	Label        string   `json:"label,omitempty"`
	Priority     string   `json:"priority,omitempty"`
	Query        string   `json:"query,omitempty"`
	AnyWords     []string `json:"-"` // any of these in title or body (stems of a topic)
	Nums         []int    `json:"nums,omitempty"`
	IncludeBody  bool     `json:"include_body,omitempty"`
	Limit        int      `json:"limit,omitempty"`
}

type TaskListOut struct {
	Items  []TaskRow      `json:"items"`
	Total  int            `json:"total"`  // rows matching the filter (the page may be shorter)
	Counts map[string]int `json:"counts"` // by status, whole project (not only the page)
}

// TaskList is task.list (todo_list of the facade). Retracted tasks never appear.
func (s *Service) TaskList(ctx context.Context, p Principal, in TaskListIn) (TaskListOut, error) {
	ctx, _, err := s.enter(ctx, p)
	if err != nil {
		return TaskListOut{}, err
	}
	out := TaskListOut{Items: []TaskRow{}, Counts: map[string]int{}}
	projectID, err := s.projectID(ctx, p, in.Project)
	if err != nil {
		return out, err
	}
	var a args
	where := []string{"t.project_id = " + a.add(projectID), "NOT t.hidden"}
	switch in.State {
	case "", "open":
		if len(in.Nums) == 0 { // explicit nums are returned whatever their state
			where = append(where, "t.open")
		}
	case "closed":
		where = append(where, "NOT t.open")
	case "all":
	default:
		return out, fail("invalid_input", "state: open | closed | all")
	}
	if len(in.Status) > 0 {
		where = append(where, "t.status = ANY("+a.add(in.Status)+"::text[])")
	}
	if len(in.Kind) > 0 {
		where = append(where, "t.kind = ANY("+a.add(in.Kind)+"::text[])")
	}
	if in.Role != "" {
		where = append(where, "t.role = "+a.add(in.Role))
	}
	if in.Owner != "" {
		where = append(where, "t.owner = "+a.add(in.Owner))
	}
	if in.AssigneeRole != "" {
		where = append(where, "t.assignee->>'role' = "+a.add(in.AssigneeRole))
	}
	if in.Label != "" {
		where = append(where, a.add(in.Label)+" = ANY(t.labels)")
	}
	if in.Priority != "" {
		where = append(where, "t.priority = "+a.add(in.Priority))
	}
	if in.Query != "" {
		q := a.add("%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(in.Query) + "%")
		where = append(where, "(t.title ILIKE "+q+" OR t.body ILIKE "+q+")")
	}
	if len(in.AnyWords) > 0 {
		where = append(where, anyLike(&a, []string{"t.title", "t.body"}, in.AnyWords))
	}
	if len(in.Nums) > 0 {
		where = append(where, "t.num = ANY("+a.add(in.Nums)+"::int[])")
	}
	body := "NULL::text"
	if in.IncludeBody || len(in.Nums) > 0 {
		body = "t.body"
	}
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM projection.task t WHERE `+strings.Join(where, " AND "), a...).Scan(&out.Total); err != nil {
		return out, err
	}
	limit := a.add(clamp(in.Limit, 100, 1, 500))
	if out.Items, err = s.taskRows(ctx, p, body, strings.Join(where, " AND "), limit, a); err != nil {
		return out, err
	}
	rows, err := s.DB.Query(ctx, `SELECT status, count(*) FROM projection.task WHERE project_id = $1 AND NOT hidden GROUP BY status`, projectID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return out, err
		}
		out.Counts[st] = n
	}
	return out, rows.Err()
}

func (s *Service) taskRows(ctx context.Context, p Principal, bodyCol, where, limit string, a args) ([]TaskRow, error) {
	rows, err := s.DB.Query(ctx, `SELECT t.task_id, pr.name, t.num, t.title, `+bodyCol+`, t.kind, t.status, t.open, t.priority, t.rank,
		       t.role, t.owner, to_char(t.due, 'YYYY-MM-DD'), t.parent, t.acceptance, t.severity, t.resolution, t.labels, t.refs, t.closed_at, op.name, t.assignee,
		       (SELECT coalesce(array_agg(xp.name || '/T-' || x.num ORDER BY xp.name, x.num), '{}') FROM projection.link l
		          JOIN projection.task x ON x.task_id = l.to_id JOIN projection.project xp ON xp.project_id = x.project_id
		         WHERE l.from_type = 'task' AND l.from_id = t.task_id AND l.to_type = 'task' AND NOT l.retired
		           AND l.relation = ANY(`+satisfiable+`) AND x.open), -- a gone target does not unblock
		       (SELECT coalesce(array_agg(xp.name || '/T-' || x.num ORDER BY xp.name, x.num), '{}') FROM projection.link l
		          JOIN projection.task x ON x.task_id = l.from_id JOIN projection.project xp ON xp.project_id = x.project_id
		         WHERE l.to_type = 'task' AND l.to_id = t.task_id AND l.from_type = 'task' AND NOT l.retired
		           AND l.relation = ANY(`+satisfiable+`) AND x.open AND NOT x.hidden),
		       t.revision, (t.body IS NOT NULL OR t.acceptance IS NOT NULL OR t.resolution IS NOT NULL), d.effective_at, d.actor_id, `+provCols("j")+`
		  FROM projection.task t JOIN projection.project pr ON pr.project_id = t.project_id
		  LEFT JOIN projection.project op ON op.project_id = t.owner_project`+
		provJoin("j", "t.tenant_id", "t.last_seq")+provJoin("d", "t.tenant_id", "t.declared_seq")+`
		 WHERE `+where+` ORDER BY `+taskOrder+` LIMIT `+limit, a...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TaskRow, error) {
		var x TaskRow
		err := r.Scan(append([]any{&x.TaskID, &x.Project, &x.Num, &x.Title, &x.Body, &x.Kind, &x.Status, &x.Open, &x.Priority, &x.Rank,
			&x.Role, &x.Owner, &x.Due, &x.Parent, &x.Acceptance, &x.Severity, &x.Resolution, &x.Labels, &x.Refs, &x.ClosedAt, &x.OwnerProject, &x.Assignee, &x.WaitingOnRefs, &x.BlockingRefs,
			&x.Revision, &x.HasDetails, &x.CreatedAt, &x.DeclaredBy}, x.Provenance.dest()...)...)
		x.Ref = x.Project + "/T-" + strconv.Itoa(x.Num)
		x.WaitingOn, x.Blocking = len(x.WaitingOnRefs), len(x.BlockingRefs)
		if x.WaitingOn == 0 {
			x.WaitingOnRefs = nil
		}
		if x.Blocking == 0 {
			x.BlockingRefs = nil
		}
		return x, err
	})
}

// tasks: open tasks of a project in "що далі" order (context bundle, work.open_items).
func (s *Service) tasks(ctx context.Context, p Principal, projectID uuid.UUID, openOnly bool) ([]TaskRow, error) {
	var a args
	where := "t.project_id = " + a.add(projectID) + " AND NOT t.hidden"
	if openOnly {
		where += " AND t.open"
	}
	return s.taskRows(ctx, p, "NULL::text", where, a.add(500), a)
}
