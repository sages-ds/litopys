package query

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The cross-project check of work.open_items (owner decision 2026-10-30): the portfolio rule
// "one owner, a mirror at the consumer" (a handoff link from the consumer's mirror to the owner's task) checked
// from one project. Links read from both ends, so a missing back-reference is not a finding here.

// CrossFinding is one thing to fix: Task is this project's task, Other the task of another project it concerns.
type CrossFinding struct {
	Kind  string `json:"kind"` // handoff_closed | handoff_gone | mirror_unlinked | duplicate_key
	Task  string `json:"task"` // <project>/T-<num>
	Title string `json:"title"`
	Other string `json:"other,omitempty"`
	Key   string `json:"key,omitempty"` // the shared external key of duplicate_key
	Hint  string `json:"hint"`
}

// CrossWaiting is a task of this project that a mirror of another project waits on.
type CrossWaiting struct {
	Task  string `json:"task"`
	Title string `json:"title"`
	From  string `json:"from"` // the mirror
}

// CrossProject is the block; empty slices are omitted by the caller.
type CrossProject struct {
	Findings    []CrossFinding `json:"findings,omitempty"`
	WaitingOnUs []CrossWaiting `json:"waiting_on_us,omitempty"`
}

var crossHints = map[string]string{
	"handoff_closed":  "the owner closed its task: check the result against the mirror's acceptance, then close the mirror",
	"handoff_gone":    "the owner's task is gone (retracted, quarantined or erased): link the mirror to another task or drop it",
	"mirror_unlinked": "a mirror without a handoff link: link it to the owner's task (link_assert handoff) or drop the label",
	"duplicate_key":   "the same external key is open in two projects: keep it open at the owner, make the other a mirror",
}

// crossProject runs inside an entered context (the owner's scope: RLS keeps every end with the same person).
func (s *Service) crossProject(ctx context.Context, projectID uuid.UUID) (CrossProject, error) {
	var out CrossProject
	rows, err := s.DB.Query(ctx, `
	  WITH mine AS (
	    SELECT t.task_id, t.title, p.name || '/T-' || t.num AS ref, t.labels, t.refs, t.owner_project
	      FROM projection.task t JOIN projection.project p ON p.project_id = t.project_id
	     WHERE t.project_id = $1 AND t.open AND NOT t.hidden),
	  handoffs AS (
	    SELECT m.task_id, m.title, m.ref, o.open AS other_open, o.hidden AS other_hidden, op.name || '/T-' || o.num AS other
	      FROM mine m JOIN projection.link l ON l.from_type = 'task' AND l.from_id = m.task_id AND l.relation = 'handoff' AND NOT l.retired
	      JOIN projection.task o ON o.task_id = l.to_id JOIN projection.project op ON op.project_id = o.project_id)
	  SELECT CASE WHEN other_hidden THEN 'handoff_gone' ELSE 'handoff_closed' END, ref, title, other, ''
	    FROM handoffs WHERE other_hidden OR NOT other_open
	  UNION ALL
	  SELECT 'mirror_unlinked', m.ref, m.title, '', ''
	    FROM mine m
	   WHERE (m.owner_project IS NOT NULL OR EXISTS (SELECT 1 FROM unnest(m.labels) x
	           WHERE lower(x) IN ('mirror', 'handoff') OR lower(x) LIKE 'handoff:%'))
	     AND NOT EXISTS (SELECT 1 FROM handoffs h WHERE h.task_id = m.task_id)
	  UNION ALL
	  SELECT DISTINCT ON (m.ref, k.key) 'duplicate_key', m.ref, m.title, op.name || '/T-' || o.num, k.key
	    FROM mine m CROSS JOIN LATERAL (SELECT lower(r) AS key FROM unnest(m.refs) r WHERE r ~* '^jira:[a-z][a-z0-9]*-[0-9]+$') k
	    JOIN projection.task o ON o.project_id <> $1 AND o.open AND NOT o.hidden
	     AND EXISTS (SELECT 1 FROM unnest(o.refs) r2 WHERE lower(r2) = k.key)
	    JOIN projection.project op ON op.project_id = o.project_id
	   WHERE NOT EXISTS (SELECT 1 FROM projection.link l WHERE l.relation = 'handoff' AND NOT l.retired AND l.from_type = 'task'
	           AND ((l.from_id = m.task_id AND l.to_id = o.task_id) OR (l.from_id = o.task_id AND l.to_id = m.task_id)))
	  ORDER BY 1, 2, 4`, projectID)
	if err != nil {
		return out, err
	}
	out.Findings, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (CrossFinding, error) {
		var f CrossFinding
		err := r.Scan(&f.Kind, &f.Task, &f.Title, &f.Other, &f.Key)
		f.Hint = crossHints[f.Kind]
		return f, err
	})
	if err != nil {
		return out, err
	}
	rows, err = s.DB.Query(ctx, `SELECT p.name || '/T-' || t.num, t.title, mp.name || '/T-' || m.num
	   FROM projection.task t JOIN projection.project p ON p.project_id = t.project_id
	   JOIN projection.link l ON l.to_type = 'task' AND l.to_id = t.task_id AND l.relation = 'handoff' AND NOT l.retired
	   JOIN projection.task m ON m.task_id = l.from_id AND m.open AND NOT m.hidden
	   JOIN projection.project mp ON mp.project_id = m.project_id
	  WHERE t.project_id = $1 AND t.open AND NOT t.hidden
	  ORDER BY t.num, mp.name, m.num`, projectID)
	if err != nil {
		return out, err
	}
	out.WaitingOnUs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (CrossWaiting, error) {
		var w CrossWaiting
		return w, r.Scan(&w.Task, &w.Title, &w.From)
	})
	return out, err
}
