package query

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Search by number: a query that is wholly a number of an entity reads that entity first, then the
// records that mention it, then the usual list without repeats. A record: R-498 (#498 stays with the identifier
// tier: a task or a record). A task, workflow or execution: [<project>/]T-, W-, X-<num>, the short form only within
// the project filter of the search.

var (
	numRecordRe  = regexp.MustCompile(`^(?i:r-)(\d{1,12})$`)
	numProjectRe = regexp.MustCompile(`^(?:([A-Za-z0-9][A-Za-z0-9._-]*)/)?([TtWwXx])-(\d{1,9})$`)
)

// EntityHit is the entity a number query names when it is not a live record (live records come as the first item):
// a task, workflow or execution, or a purged record — then without a title and with its tombstone.
type EntityHit struct {
	Ref       string     `json:"ref"`
	Type      string     `json:"entity_type"`
	ID        uuid.UUID  `json:"entity_id"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	Tombstone *Tombstone `json:"tombstone,omitempty"`
}

// A retracted, quarantined or purged task still answers its number, with its state.
var numberKinds = map[byte]struct{ entity, sql string }{
	'T': {"task", `SELECT t.task_id, t.title, CASE WHEN t.purged THEN 'purged' WHEN t.quarantined THEN 'quarantined' WHEN t.retracted THEN 'retracted' ELSE t.status END
	        FROM projection.task t WHERE t.project_id = $1 AND t.num = $2`},
	'W': {"workflow", `SELECT w.workflow_id, w.title, CASE WHEN w.retired THEN 'retired' ELSE 'active' END
	        FROM projection.workflow w WHERE w.project_id = $1 AND w.num = $2`},
	'X': {"execution", `SELECT x.execution_id, w.key, x.state FROM projection.execution x
	        JOIN projection.workflow w ON w.workflow_id = x.workflow_id WHERE x.project_id = $1 AND x.num = $2`},
}

// numberHead resolves a number query: the record itself (head) or the entity hit, and the records mentioning it,
// newest first; by — the tasks, workflows and executions that mention it, which are not records and so come apart
// from the items. ok = false: the query is not a number, or names nothing the person has — the usual search
// answers.
func (s *Service) numberHead(ctx context.Context, p Principal, root uuid.UUID, in SearchIn) (head []IndexRow, hit *EntityHit, by []EntityHit, ok bool, err error) {
	head, hit, ok, err = s.numberTarget(ctx, p, root, in)
	if !ok || err != nil {
		return nil, nil, nil, ok, err
	}
	var target uuid.UUID
	if len(head) > 0 {
		target = head[0].StatementID
	} else {
		target = hit.ID
	}
	rows, err := s.DB.Query(ctx, `SELECT l.from_id FROM projection.link l
	        WHERE l.to_id = $1 AND l.relation = 'mentions' AND l.from_type = 'statement' AND NOT l.retired
	        ORDER BY l.asserted_seq DESC LIMIT 20`, target)
	if err != nil {
		return nil, nil, nil, false, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, nil, nil, false, err
	}
	mentions, err := s.rowsByID(ctx, p, root, ids)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if by, err = s.mentionedByEntities(ctx, target); err != nil {
		return nil, nil, nil, false, err
	}
	full := &Match{Coverage: "full", Matched: []string{strings.TrimSpace(in.Query)}}
	for i := range head {
		head[i].Match = full
	}
	for i := range mentions {
		mentions[i].Match = full
	}
	return append(head, mentions...), hit, by, true, nil
}

// mentionedByEntities: the live tasks, workflows and executions with a mentions link to target, newest first (≤ 20);
// rows the person cannot see are left out by row security.
func (s *Service) mentionedByEntities(ctx context.Context, target uuid.UUID) ([]EntityHit, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT e.ref, l.from_type, l.from_id, e.title, e.state FROM projection.link l
		  JOIN LATERAL (
		    SELECT p.name || '/T-' || t.num, t.title, t.status FROM projection.task t JOIN projection.project p ON p.project_id = t.project_id
		     WHERE l.from_type = 'task' AND t.task_id = l.from_id AND NOT t.hidden
		    UNION ALL
		    SELECT p.name || '/W-' || w.num, w.title, CASE WHEN w.retired THEN 'retired' ELSE 'active' END
		      FROM projection.workflow w JOIN projection.project p ON p.project_id = w.project_id
		     WHERE l.from_type = 'workflow' AND w.workflow_id = l.from_id
		    UNION ALL
		    SELECT p.name || '/X-' || x.num, w.key, x.state FROM projection.execution x
		      JOIN projection.workflow w ON w.workflow_id = x.workflow_id JOIN projection.project p ON p.project_id = x.project_id
		     WHERE l.from_type = 'execution' AND x.execution_id = l.from_id) e(ref, title, state) ON true
		 WHERE l.to_id = $1 AND l.relation = 'mentions' AND l.from_type <> 'statement' AND NOT l.retired
		 ORDER BY l.asserted_seq DESC LIMIT 20`, target)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EntityHit, error) {
		var h EntityHit
		return h, r.Scan(&h.Ref, &h.Type, &h.ID, &h.Title, &h.State)
	})
}

// numberTarget: the record a number query names (as head) or the task, workflow or execution (as hit).
func (s *Service) numberTarget(ctx context.Context, p Principal, root uuid.UUID, in SearchIn) (head []IndexRow, hit *EntityHit, ok bool, err error) {
	q := strings.TrimSpace(in.Query)
	var target uuid.UUID
	if m := numRecordRe.FindStringSubmatch(q); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		var state *string
		err = s.DB.QueryRow(ctx, `SELECT statement_id, CASE WHEN purged THEN 'purged' WHEN quarantined THEN 'quarantined' END
		          FROM projection.statement WHERE owner_id = $1 AND num = $2`, root, n).Scan(&target, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, false, nil
		}
		if err != nil {
			return nil, nil, false, err
		}
		if state != nil {
			stones, err := s.tombstones(ctx, p.TenantID, "statement", []uuid.UUID{target})
			if err != nil {
				return nil, nil, false, err
			}
			return nil, &EntityHit{Ref: "R-" + strconv.FormatInt(n, 10), Type: "statement", ID: target, State: *state, Tombstone: stones[target]}, true, nil
		}
		if head, err = s.rowsByID(ctx, p, root, []uuid.UUID{target}); err != nil || len(head) == 0 {
			return nil, nil, false, err
		}
	} else if m := numProjectRe.FindStringSubmatch(q); m != nil {
		project := m[1]
		if project == "" {
			project = in.Project
		}
		if project == "" {
			return nil, nil, false, nil
		}
		projectID, perr := s.projectID(ctx, p, project)
		if perr != nil {
			return nil, nil, false, nil // an unknown project: not a number of this person
		}
		letter := strings.ToUpper(m[2])[0]
		k := numberKinds[letter]
		n, _ := strconv.Atoi(m[3])
		h := EntityHit{Ref: project + "/" + string(letter) + "-" + m[3], Type: k.entity}
		err = s.DB.QueryRow(ctx, k.sql, projectID, n).Scan(&h.ID, &h.Title, &h.State)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, false, nil
		}
		if err != nil {
			return nil, nil, false, err
		}
		if h.State == "purged" || h.State == "quarantined" { // its tombstone, never the title
			stones, err := s.tombstones(ctx, p.TenantID, k.entity, []uuid.UUID{h.ID})
			if err != nil {
				return nil, nil, false, err
			}
			h.Title, h.Tombstone = "", stones[h.ID]
		}
		hit = &h
	} else {
		return nil, nil, false, nil
	}
	return head, hit, true, nil
}

// rowsByID: index rows of the person's visible, live statements in the given order.
func (s *Service) rowsByID(ctx context.Context, p Principal, root uuid.UUID, ids []uuid.UUID) ([]IndexRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT `+indexCols+provCols("j")+`
	  FROM projection.statement s JOIN projection.project p ON p.project_id = s.project_id`+provJoin("j", "s.tenant_id", "s.last_seq")+`
	 WHERE s.tenant_id = $1 AND s.statement_id = ANY($2) AND NOT s.hidden AND `+mineSQL("s.actor_id", "$3")+`
	 ORDER BY array_position($2::uuid[], s.statement_id)`, p.TenantID, ids, root)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (IndexRow, error) {
		var x IndexRow
		return x, r.Scan(x.dest()...)
	})
}
