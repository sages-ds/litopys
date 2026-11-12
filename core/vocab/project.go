package vocab

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Querier reads projections (a pool or a transaction).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ProjectSet is the set of vocabularies of a project: facts of kind
// vocabulary along the stack of layers — the owner's global project, the spaces the project is member_of (by name,
// the later wins), the project itself — the narrower layer winning per key, "on" enabling and "off" disabling what a
// wider layer enabled. As in the stack of layers (query/layers.go), only facts the owner wrote count; a fact written
// in a run of a sub-agent is a proposal.
func ProjectSet(ctx context.Context, q Querier, projectID uuid.UUID) (Set, error) {
	rows, err := q.Query(ctx, `
		WITH p AS (SELECT project_id, tenant_id, owner_id, name FROM projection.project WHERE project_id = $1),
		src AS (
		  (SELECT g.project_id, 0 AS ord FROM projection.project g, p
		    WHERE g.tenant_id = p.tenant_id AND g.owner_id = p.owner_id AND 'global' = ANY(g.labels) AND g.name <> p.name
		    ORDER BY g.name LIMIT 1)
		  UNION ALL
		  SELECT k.to_id, 1 + row_number() OVER (ORDER BY sp.name) FROM projection.link k
		    JOIN projection.project sp ON sp.project_id = k.to_id
		   WHERE k.from_id = $1 AND k.relation = 'member_of' AND NOT k.retired
		  UNION ALL
		  SELECT $1, 1000000)
		SELECT f.key, f.value FROM src
		  JOIN projection.fact f ON f.project_id = src.project_id AND f.kind = 'vocabulary' AND NOT f.retired
		  JOIN journal j ON j.tenant_id = f.tenant_id AND j.seq = f.last_seq
		  LEFT JOIN projection.run r ON r.run_id = j.run_id
		 WHERE j.actor_id = (SELECT owner_id FROM p) AND NOT COALESCE(r.client ? 'agent', false)
		 ORDER BY src.ord, f.key`, projectID)
	if err != nil {
		return nil, err
	}
	type fact struct {
		key   string
		value *string
	}
	facts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (fact, error) {
		var f fact
		return f, r.Scan(&f.key, &f.value)
	})
	if err != nil {
		return nil, err
	}
	on := map[string]bool{}
	var order []string
	for _, f := range facts {
		if _, seen := on[f.key]; !seen {
			order = append(order, f.key)
		}
		on[f.key] = f.value != nil && *f.value == "on"
	}
	var enabled []string
	for _, k := range order {
		if on[k] {
			enabled = append(enabled, k)
		}
	}
	return NewSet(enabled...), nil
}
