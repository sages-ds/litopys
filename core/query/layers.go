package query

import (
	"context"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// layerKinds are the fact kinds that make up a layer (migration 0015).
var layerKinds = map[string]bool{"instruction": true, "style": true, "agent": true, "skill": true}

// LayerKind: a fact of this kind belongs to the stack of layers, not to the project's own facts.
func LayerKind(kind string) bool { return layerKinds[kind] }

// LayerRule is one rule of the resolved stack with the layer and project it comes from.
type LayerRule struct {
	Kind   string  `json:"kind"`
	Key    string  `json:"key"`
	Value  *string `json:"value,omitempty"`
	Layer  string  `json:"layer"`  // global | projects | project
	Source string  `json:"source"` // the project (or space) that holds the rule
}

// Layers is the resolved stack global → projects → <project> of the start package:
// a narrower layer wins (R > P > G); among several spaces the one later by name wins.
// Only rules written by the owner enter the stack: a rule from an agent is a proposal.
type Layers struct {
	Global     string      `json:"global,omitempty"`
	Spaces     []string    `json:"spaces"`
	Stack      []LayerRule `json:"stack"`
	Overridden int         `json:"overridden,omitempty"`
}

// Layers is project.layers: the resolved stack of a project without opening a context.
func (s *Service) Layers(ctx context.Context, p Principal, project string) (Layers, error) {
	ctx, root, err := s.enter(ctx, p)
	if err != nil {
		return Layers{}, err
	}
	id, err := s.projectID(ctx, p, project)
	if err != nil {
		return Layers{}, err
	}
	return s.layers(ctx, p, root, id, project)
}

// layers resolves the stack of a project. global: the owner's project labelled "global"; spaces: the
// projects this one is member_of.
func (s *Service) layers(ctx context.Context, p Principal, root uuid.UUID, projectID uuid.UUID, project string) (Layers, error) {
	l := Layers{Spaces: []string{}, Stack: []LayerRule{}}
	err := s.DB.QueryRow(ctx, `SELECT name FROM projection.project
		 WHERE tenant_id = $1 AND owner_id = $2 AND 'global' = ANY(labels) AND name <> $3 ORDER BY name LIMIT 1`,
		p.TenantID, root, project).Scan(&l.Global)
	if err != nil && !isNoRows(err) {
		return l, err
	}
	rows, err := s.DB.Query(ctx, `SELECT pr.name FROM projection.link k JOIN projection.project pr ON pr.project_id = k.to_id
		 WHERE k.from_id = $1 AND k.relation = 'member_of' AND NOT k.retired ORDER BY pr.name`, projectID)
	if err != nil {
		return l, err
	}
	if l.Spaces, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return l, err
	}
	type src struct{ layer, project string }
	order := []src{}
	if l.Global != "" {
		order = append(order, src{"global", l.Global})
	}
	for _, sp := range l.Spaces {
		order = append(order, src{"projects", sp})
	}
	order = append(order, src{"project", project})

	byKey := map[string]int{}
	for _, o := range order {
		facts, err := s.FactList(ctx, p, o.project, "", "")
		if err != nil {
			return l, err
		}
		// a rule recorded in a run of a sub-agent (client.agent) is a proposal, like an agent's statement
		// (authority): it does not enter the stack
		runs := []uuid.UUID{}
		for _, f := range facts {
			if LayerKind(f.Kind) && f.Provenance.RunID != nil {
				runs = append(runs, *f.Provenance.RunID)
			}
		}
		agentRuns := map[uuid.UUID]bool{}
		if len(runs) > 0 {
			rows, err := s.DB.Query(ctx, `SELECT run_id FROM projection.run WHERE run_id = ANY($1) AND client ? 'agent'`, runs)
			if err != nil {
				return l, err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			if err != nil {
				return l, err
			}
			for _, id := range ids {
				agentRuns[id] = true
			}
		}
		for _, f := range facts {
			if !LayerKind(f.Kind) || f.Provenance.Actor.ActorID != root || (f.Provenance.RunID != nil && agentRuns[*f.Provenance.RunID]) {
				continue
			}
			r := LayerRule{Kind: f.Kind, Key: f.Key, Value: f.Value, Layer: o.layer, Source: o.project}
			k := f.Kind + "/" + f.Key
			if i, ok := byKey[k]; ok {
				l.Stack[i] = r
				l.Overridden++
				continue
			}
			byKey[k] = len(l.Stack)
			l.Stack = append(l.Stack, r)
		}
	}
	sort.SliceStable(l.Stack, func(i, j int) bool {
		if l.Stack[i].Kind != l.Stack[j].Kind {
			return l.Stack[i].Kind < l.Stack[j].Kind
		}
		return l.Stack[i].Key < l.Stack[j].Key
	})
	return l, nil
}
