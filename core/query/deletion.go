package query

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Impact before deletion: who depends on an entity — the live links that point at it, grouped by
// relation as the entity sees them (mentioned_by, blocks…), the outcomes about it and, for a project, what it holds.
// Read only; a deletion proposal carries it and a GO is given on it. Deletion never cascades: the
// entities listed here are not changed.

const impactSample = 20

// ImpactEnd is one entity on the other end of a link.
type ImpactEnd struct {
	Type  string    `json:"entity_type"`
	ID    uuid.UUID `json:"entity_id"`
	Ref   *string   `json:"ref,omitempty"`
	State *string   `json:"state,omitempty"` // retracted / purged when no longer live
}

// ImpactGroup: the incoming links of one relation — how many, and the newest of them.
type ImpactGroup struct {
	Relation string      `json:"relation"` // as seen from the target: mentions_by, depends_on_by, handoff_by…
	Count    int         `json:"count"`
	Sample   []ImpactEnd `json:"sample"`
}

// ImpactContents: what a project holds (for a project target only).
type ImpactContents struct {
	Statements int `json:"statements"`
	Tasks      int `json:"tasks"`
	Workflows  int `json:"workflows"`
}

type ImpactOut struct {
	Target   ImpactEnd       `json:"target"`
	Incoming []ImpactGroup   `json:"incoming"`
	Outcomes int             `json:"outcomes"` // outcomes of sessions about the target
	Contents *ImpactContents `json:"contents,omitempty"`
	Hidden   []Hidden        `json:"hidden"`
}

// Impact: the impact of deleting the entity typ/id. The entity must exist; visibility is the person's own (rows
// security), so the counts are of what the person can see.
func (s *Service) Impact(ctx context.Context, p Principal, typ string, id uuid.UUID) (ImpactOut, error) {
	out := ImpactOut{Incoming: []ImpactGroup{}, Hidden: []Hidden{}}
	ctx, _, err := s.enter(ctx, p)
	if err != nil {
		return out, err
	}
	out.Hidden = hiddenOf(ctx)
	out.Target = ImpactEnd{Type: typ, ID: id}
	if err := s.DB.QueryRow(ctx, `SELECT `+entityRefSQL("$1::text", "$2::uuid")+`, `+entityStateSQL("$1::text", "$2::uuid"),
		typ, id).Scan(&out.Target.Ref, &out.Target.State); err != nil {
		return out, err
	}
	rows, err := s.DB.Query(ctx, `
		SELECT l.relation, count(*) OVER (PARTITION BY l.relation), l.from_type, l.from_id,
		       `+entityRefSQL("l.from_type", "l.from_id")+`, `+entityStateSQL("l.from_type", "l.from_id")+`
		  FROM (SELECT *, row_number() OVER (PARTITION BY relation ORDER BY asserted_seq DESC) AS rn
		          FROM projection.link WHERE to_type = $1 AND to_id = $2 AND NOT retired) l
		 WHERE l.rn <= $3
		 ORDER BY l.relation, l.rn`, typ, id, impactSample)
	if err != nil {
		return out, err
	}
	var rel string
	var count int
	var end ImpactEnd
	if _, err := pgx.ForEachRow(rows, []any{&rel, &count, &end.Type, &end.ID, &end.Ref, &end.State}, func() error {
		if n := len(out.Incoming); n == 0 || out.Incoming[n-1].Relation != inverseName(rel) {
			out.Incoming = append(out.Incoming, ImpactGroup{Relation: inverseName(rel), Count: count})
		}
		g := &out.Incoming[len(out.Incoming)-1]
		g.Sample = append(g.Sample, end)
		return nil
	}); err != nil {
		return out, err
	}
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM projection.statement s
	        WHERE s.type = 'outcome' AND s.outcome->>'subject_id' = $1 AND NOT s.hidden AND s.status <> 'retracted'`,
		id.String()).Scan(&out.Outcomes); err != nil {
		return out, err
	}
	if typ == "project" {
		var c ImpactContents
		if err := s.DB.QueryRow(ctx, `SELECT
		        (SELECT count(*) FROM projection.statement WHERE project_id = $1 AND NOT hidden AND status <> 'retracted'),
		        (SELECT count(*) FROM projection.task WHERE project_id = $1 AND NOT hidden),
		        (SELECT count(*) FROM projection.workflow WHERE project_id = $1)`, id).Scan(&c.Statements, &c.Tasks, &c.Workflows); err != nil {
			return out, err
		}
		out.Contents = &c
	}
	return out, nil
}

// inverseName: an incoming relation as the target sees it — <relation>_by, as recall_read names incoming links.
func inverseName(rel string) string { return rel + "_by" }

// ---------------------------------------------------------------------------
// deletion.list
// ---------------------------------------------------------------------------

// DeletionRow is a deletion proposal D-N as reads show it: the target by its ref and state, never its title.
type DeletionRow struct {
	DeletionID uuid.UUID       `json:"deletion_id"`
	Ref        string          `json:"ref"` // <project>/D-N
	Target     ImpactEnd       `json:"target"`
	Action     string          `json:"action"`
	Reason     string          `json:"reason"`
	Secret     bool            `json:"secret,omitempty"`
	Impact     json.RawMessage `json:"impact"`
	State      string          `json:"state"`
	ProposedBy ActorRef        `json:"proposed_by"`
	ProposedAt time.Time       `json:"proposed_at"`
	DecidedBy  *ActorRef       `json:"decided_by,omitempty"`
	DecidedAt  *time.Time      `json:"decided_at,omitempty"`
	Note       *string         `json:"note,omitempty"`
}

type DeletionListIn struct {
	Project string `json:"project,omitempty"`
	State   string `json:"state,omitempty"` // waiting (open + approved, default) | open | approved | done | rejected | all
	Limit   int    `json:"limit,omitempty"`
}

type DeletionListOut struct {
	Items  []DeletionRow `json:"items"`
	Hidden []Hidden      `json:"hidden"`
}

// DeletionList: the person's deletion proposals, waiting ones by default, oldest first (what waits longest comes first).
func (s *Service) DeletionList(ctx context.Context, p Principal, in DeletionListIn) (DeletionListOut, error) {
	out := DeletionListOut{Items: []DeletionRow{}, Hidden: []Hidden{}}
	ctx, _, err := s.enter(ctx, p)
	if err != nil {
		return out, err
	}
	out.Hidden = hiddenOf(ctx)
	var projectID *uuid.UUID
	if in.Project != "" {
		id, err := s.projectID(ctx, p, in.Project)
		if err != nil {
			return out, err
		}
		projectID = &id
	}
	states := map[string][]string{"": {"open", "approved"}, "waiting": {"open", "approved"}, "open": {"open"},
		"approved": {"approved"}, "done": {"done"}, "rejected": {"rejected"}, "all": {"open", "approved", "done", "rejected"}}[in.State]
	if states == nil {
		return out, fail("invalid_input", "state: waiting | open | approved | done | rejected | all")
	}
	rows, err := s.DB.Query(ctx, `
		SELECT d.deletion_id, p.name || '/D-' || d.num, d.target_type, d.target_id, `+entityRefSQL("d.target_type", "d.target_id")+`,
		       `+entityStateSQL("d.target_type", "d.target_id")+`, d.action, d.reason, d.secret, d.impact, d.state,
		       d.proposed_by, d.proposed_at, d.decided_by, d.decided_at, d.note
		  FROM projection.deletion d JOIN projection.project p ON p.project_id = d.project_id
		 WHERE d.tenant_id = $1 AND d.state = ANY($2) AND ($3::uuid IS NULL OR d.project_id = $3)
		 ORDER BY d.proposed_seq LIMIT $4`, p.TenantID, states, projectID, clamp(in.Limit, 50, 1, 200))
	if err != nil {
		return out, err
	}
	out.Items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (DeletionRow, error) {
		var x DeletionRow
		var by *uuid.UUID
		err := r.Scan(&x.DeletionID, &x.Ref, &x.Target.Type, &x.Target.ID, &x.Target.Ref, &x.Target.State, &x.Action, &x.Reason,
			&x.Secret, &x.Impact, &x.State, &x.ProposedBy.ActorID, &x.ProposedAt, &by, &x.DecidedAt, &x.Note)
		if by != nil {
			x.DecidedBy = &ActorRef{ActorID: *by}
		}
		return x, err
	})
	return out, err
}
