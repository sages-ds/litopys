package query

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Reads behind the *_resume family of the native API. The bundle itself is ContextPack;
// these are the scoped selections around it.

// CatalogRow is read-models.yaml projects.catalog_row: the compact catalog of project.resume.
type CatalogRow struct {
	Name             string    `json:"name"`
	Title            string    `json:"title"`
	Status           string    `json:"status"`
	Summary          string    `json:"summary,omitempty"`
	SummaryTruncated bool      `json:"summary_truncated,omitempty"`
	Scope            string    `json:"scope,omitempty"` // 0041: what belongs to the project
	UpdatedAt        time.Time `json:"updated_at"`
	OpenTasks        int       `json:"open_tasks"`
}

// CatalogSummaryRunes is where the catalog cuts a summary.
const CatalogSummaryRunes = 160

// Catalog lists the principal's projects, most recently worked on first, with open task counts.
func (s *Service) Catalog(ctx context.Context, p Principal) ([]CatalogRow, error) {
	ctx, me, err := s.enter(ctx, p)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT pr.name, COALESCE(pr.title, pr.name), pr.status, COALESCE(pr.summary, ''), COALESCE(pr.scope, ''), pr.updated_at,
	       (SELECT count(*) FROM projection.task t WHERE t.project_id = pr.project_id AND t.open AND NOT t.hidden)
	  FROM projection.project pr WHERE pr.tenant_id = $1 AND pr.owner_id = $2
	 ORDER BY pr.updated_at DESC, pr.name`, p.TenantID, me)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CatalogRow, error) {
		var x CatalogRow
		if err := r.Scan(&x.Name, &x.Title, &x.Status, &x.Summary, &x.Scope, &x.UpdatedAt, &x.OpenTasks); err != nil {
			return x, err
		}
		x.Summary = strings.Join(strings.Fields(x.Summary), " ")
		if rs := []rune(x.Summary); len(rs) > CatalogSummaryRunes {
			x.Summary, x.SummaryTruncated = string(rs[:CatalogSummaryRunes])+"…", true
		}
		return x, nil
	})
}

// ProjectLink is one live link between two projects as project.resume shows it: To is set
// for an outgoing link (this project --type--> To), From for an incoming one.
type ProjectLink struct {
	Type     string `json:"type"`
	To       string `json:"to,omitempty"`
	From     string `json:"from,omitempty"`
	Archived bool   `json:"archived,omitempty"`
}

// ProjectLinkRelations are the relations reserved for project ends (migration 0012) that the start shows.
var ProjectLinkRelations = []string{"depends_on", "predecessor_of", "parallel_line_of"}

// ProjectLinks: live project links of a project in both directions, outgoing first, by relation and name;
// at most limit, the second value counts the rest.
func (s *Service) ProjectLinks(ctx context.Context, p Principal, projectID uuid.UUID, limit int) ([]ProjectLink, int, error) {
	ctx, _, err := s.enter(ctx, p)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.Query(ctx, `SELECT l.relation, l.from_id = $2, o.name, o.status = 'archived'
	   FROM projection.link l
	   JOIN projection.project o ON o.project_id = CASE WHEN l.from_id = $2 THEN l.to_id ELSE l.from_id END
	  WHERE l.tenant_id = $1 AND l.from_type = 'project' AND l.to_type = 'project' AND NOT l.retired
	    AND (l.from_id = $2 OR l.to_id = $2) AND l.relation = ANY($3)
	  ORDER BY l.from_id = $2 DESC, l.relation, o.name`, p.TenantID, projectID, ProjectLinkRelations)
	if err != nil {
		return nil, 0, err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProjectLink, error) {
		var x ProjectLink
		var outgoing bool
		var other string
		if err := r.Scan(&x.Type, &outgoing, &other, &x.Archived); err != nil {
			return x, err
		}
		if outgoing {
			x.To = other
		} else {
			x.From = other
		}
		return x, nil
	})
	if err != nil || len(all) <= limit {
		return all, 0, err
	}
	return all[:limit], len(all) - limit, nil
}

// UnconfirmedRetention is a live retention label of a project the person has not confirmed in its current version.
// Whether it needs a confirmation at all (purge, applies_to: record) is the verbs' rule: the
// caller filters with verbs.ParseRetention(...).HumanOnly().
type UnconfirmedRetention struct {
	Key           string    `json:"key"`
	Value         string    `json:"value"`
	RecordEventID uuid.UUID `json:"record_event_id"`
}

// UnconfirmedRetentions reads them, by key.
func (s *Service) UnconfirmedRetentions(ctx context.Context, p Principal, projectID uuid.UUID) ([]UnconfirmedRetention, error) {
	ctx, _, err := s.enter(ctx, p)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT key, COALESCE(value, ''), last_event_id FROM projection.fact
	         WHERE project_id = $1 AND kind = 'retention' AND NOT retired AND confirmed_at IS NULL ORDER BY key`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (UnconfirmedRetention, error) {
		var x UnconfirmedRetention
		return x, r.Scan(&x.Key, &x.Value, &x.RecordEventID)
	})
}

// SessionStatements: the current statements written in one session (context), oldest first.
func (s *Service) SessionStatements(ctx context.Context, p Principal, contextID uuid.UUID, limit int) ([]IndexRow, error) {
	ctx, root, err := s.enter(ctx, p)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT `+indexCols+provCols("j")+`
	  FROM projection.statement s JOIN projection.project p ON p.project_id = s.project_id`+provJoin("j", "s.tenant_id", "s.last_seq")+`
	 WHERE s.context_id = $1 AND NOT s.hidden AND `+mineSQL("s.actor_id", "$2")+`
	 ORDER BY s.asserted_seq LIMIT $3`, contextID, root, clamp(limit, 50, 1, 200))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (IndexRow, error) {
		var x IndexRow
		return x, r.Scan(x.dest()...)
	})
}

// RunTasks: tasks changed by events of one run (the run of a session), in "what next" order.
func (s *Service) RunTasks(ctx context.Context, p Principal, runID uuid.UUID) ([]TaskRow, error) {
	ctx, _, err := s.enter(ctx, p)
	if err != nil {
		return nil, err
	}
	var a args
	where := "NOT t.hidden AND t.task_id IN (SELECT DISTINCT rj.entity_id FROM journal rj WHERE rj.tenant_id = " +
		a.add(p.TenantID) + " AND rj.run_id = " + a.add(runID) + " AND rj.entity_type = 'task')"
	return s.taskRows(ctx, p, "NULL::text", where, a.add(100), a)
}

// TaskFamily: one task by number with its body, its parent and its children (any state).
func (s *Service) TaskFamily(ctx context.Context, p Principal, project string, num int) (task TaskRow, parent *TaskRow, children []TaskRow, err error) {
	ctx, _, err = s.enter(ctx, p)
	if err != nil {
		return
	}
	projectID, err := s.projectID(ctx, p, project)
	if err != nil {
		return
	}
	var a args
	got, err := s.taskRows(ctx, p, "t.body", "NOT t.hidden AND t.project_id = "+a.add(projectID)+" AND t.num = "+a.add(num), a.add(1), a)
	if err != nil {
		return
	}
	if len(got) == 0 {
		err = fail("task_not_found", "task #%d in %q", num, project)
		return
	}
	task = got[0]
	if task.Parent != nil {
		var b args
		ps, e := s.taskRows(ctx, p, "NULL::text", "NOT t.hidden AND t.project_id = "+b.add(projectID)+" AND t.num = "+b.add(*task.Parent), b.add(1), b)
		if e != nil {
			err = e
			return
		}
		if len(ps) == 1 {
			parent = &ps[0]
		}
	}
	var c args
	children, err = s.taskRows(ctx, p, "NULL::text", "NOT t.hidden AND t.project_id = "+c.add(projectID)+" AND t.parent = "+c.add(num), c.add(100), c)
	return
}

// TaskMentions: current statements that name task #num — first those with a mentions link to it (explicit refs, any
// project), then those of the project whose text names it — "#N", "todo:N", "todo #N", "task N", "T-N",
// "<project>/T-N" (title or body), the fallback channel for records written before refs or whose link is gone;
// newest first within each. "T-N" counts only standing alone: not "other/T-N", not "ST-N".
func (s *Service) TaskMentions(ctx context.Context, p Principal, project string, num int, limit int) ([]IndexRow, error) {
	ctx, root, err := s.enter(ctx, p)
	if err != nil {
		return nil, err
	}
	projectID, err := s.projectID(ctx, p, project)
	if err != nil {
		return nil, err
	}
	re := fmt.Sprintf(`(#|todo[:\s]*#?|task\s*#?|(^|[^[:alnum:]_/-])(%s/)?t-)%d([^0-9]|$)`, regexp.QuoteMeta(project), num)
	mentions := `EXISTS (SELECT 1 FROM projection.link l JOIN projection.task t ON t.task_id = l.to_id
	        WHERE l.from_type = 'statement' AND l.from_id = s.statement_id AND l.to_type = 'task' AND l.relation = 'mentions'
	          AND NOT l.retired AND t.project_id = $1 AND t.num = $5)`
	rows, err := s.DB.Query(ctx, `SELECT `+indexCols+provCols("j")+`
	  FROM projection.statement s JOIN projection.project p ON p.project_id = s.project_id`+provJoin("j", "s.tenant_id", "s.last_seq")+`
	 WHERE NOT s.hidden AND s.status IN ('current', 'amended') AND `+mineSQL("s.actor_id", "$2")+`
	   AND (`+mentions+` OR (s.project_id = $1 AND (s.title ~* $3 OR COALESCE(s.body, '') ~* $3)))
	 ORDER BY `+mentions+` DESC, s.effective_at DESC LIMIT $4`, projectID, root, re, clamp(limit, 20, 1, 50), num)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (IndexRow, error) {
		var x IndexRow
		return x, r.Scan(x.dest()...)
	})
}
