// Package query implements the slice-1 QUERY verbs of domain/verbs.yaml.
//
// Reads touch only projection.* and the journal (for provenance), never write, and apply the
// built-in slice-1 policy «власник бачить усе своє»: a principal sees the rows authored by any
// actor of the same root human (a human and the agents acting on its behalf). Hidden (policy)
// lists stay empty in slice 1: other humans' rows are simply not visible, not "hidden with reason".
package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/internal/pg"
)

// DB is a read connection (*pgxpool.Pool as litopys_app).
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Service struct {
	DB DB
	// Embedder, when set, adds meaning search to recall.search (search_vec.go); nil — lexical only.
	Embedder Embedder
}

// Principal mirrors verbs.Principal (the edge resolves it); Role feeds system.whoami.
type Principal struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	Role     string       // owner | admin | consumer; "" = owner
	Grant    policy.Grant // what the consumer was given; zero = full grant
}

// Error is a domain error with the code of verbs.yaml errors[].
type Error struct{ Code, Message string }

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func fail(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Code extracts the domain error code ("" for infrastructure errors).
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

type ActorRef struct {
	ActorID uuid.UUID `json:"actor_id"`
}

// Provenance is returned by EVERY read (invariant 5): the event that produced the row's current state.
type Provenance struct {
	EventID     uuid.UUID  `json:"event_id"`
	Seq         int64      `json:"seq"`
	RecordedAt  time.Time  `json:"recorded_at"`
	EffectiveAt time.Time  `json:"effective_at"`
	Actor       ActorRef   `json:"actor"`
	RunID       *uuid.UUID `json:"run_id,omitempty"`
	CausationID *uuid.UUID `json:"causation_id,omitempty"`
	ContextID   *uuid.UUID `json:"context_id,omitempty"`
}

// provCols / provJoin: provenance of a projection row whose last event seq is in column <seq>.
func provJoin(alias, tenantCol, seqCol string) string {
	return fmt.Sprintf(" JOIN journal %s ON %s.tenant_id = %s AND %s.seq = %s ", alias, alias, tenantCol, alias, seqCol)
}

func provCols(a string) string {
	return fmt.Sprintf("%[1]s.event_id, %[1]s.seq, %[1]s.recorded_at, %[1]s.effective_at, %[1]s.actor_id, %[1]s.run_id, %[1]s.causation_id, %[1]s.context_id", a)
}

func (p *Provenance) dest() []any {
	return []any{&p.EventID, &p.Seq, &p.RecordedAt, &p.EffectiveAt, &p.Actor.ActorID, &p.RunID, &p.CausationID, &p.ContextID}
}

// Hidden is what the policy hid, aggregated: rule, entity type, count — no ids or names.
type Hidden = policy.Hidden

// hiddenOf is the hidden[] of the request's policy decision.
func hiddenOf(ctx context.Context) []Hidden {
	if d, ok := policy.From(ctx); ok && len(d.Hidden) > 0 {
		return d.Hidden
	}
	return []Hidden{}
}

// enter resolves the principal's root human (COALESCE(on_behalf_of, actor_id)) and marks the
// context with it, so every connection of the query carries litopys.owner (RLS of 0008).
// The explicit owner filters of the queries stay: RLS is the safety net under them.
func (s *Service) enter(ctx context.Context, p Principal) (context.Context, uuid.UUID, error) {
	if p.TenantID == uuid.Nil || p.ActorID == uuid.Nil {
		return ctx, uuid.Nil, fail("unauthenticated", "principal without tenant or actor")
	}
	var r *uuid.UUID
	if err := s.DB.QueryRow(ctx, `SELECT litopys_root($1)`, p.ActorID).Scan(&r); err != nil {
		return ctx, uuid.Nil, err
	}
	if r == nil {
		return ctx, uuid.Nil, fail("actor_not_registered", "actor %s", p.ActorID)
	}
	if d, ok := policy.From(ctx); ok && d.Grant.TokenID == p.Grant.TokenID { // nested call: the decision is made
		return ctx, *r, nil
	}
	ctx = pg.WithOwner(ctx, *r)
	d, err := policy.Decide(ctx, s.DB, p.TenantID, *r, p.Grant)
	if err != nil {
		return ctx, uuid.Nil, err
	}
	return policy.Scoped(ctx, d), *r, nil
}

// mineSQL is a predicate "actor column <col> belongs to the root human in placeholder <ph>".
func mineSQL(col, ph string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM projection.actor ma WHERE ma.actor_id = %s AND COALESCE(ma.on_behalf_of, ma.actor_id) = %s)`, col, ph)
}

func (s *Service) projectID(ctx context.Context, p Principal, name string) (uuid.UUID, error) {
	var id uuid.UUID
	me, _ := pg.Owner(ctx) // set by enter
	err := s.DB.QueryRow(ctx, `SELECT project_id FROM projection.project WHERE tenant_id = $1 AND owner_id = $2 AND name = $3`,
		p.TenantID, uuid.UUID(me), name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fail("project_not_found", "project %q", name)
	}
	return id, err
}

// args builds positional parameters: add returns "$n".
type args []any

func (a *args) add(v any) string { *a = append(*a, v); return fmt.Sprintf("$%d", len(*a)) }

func clamp(v, def, lo, hi int) int {
	if v == 0 {
		v = def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ExplainOut is policy.explain (domain/verbs.yaml).
type ExplainOut struct {
	Grant    policy.Grant              `json:"grant"`
	Projects []policy.ExplainedProject `json:"projects"`
}

// PolicyExplain details why projects are visible or hidden for a grant — the current one, or another
// token of the same person. Only an unrestricted grant of the data's owner, or an admin, may ask:
// a restricted consumer must not learn the names it is not shown (variant b).
func (s *Service) PolicyExplain(ctx context.Context, p Principal, project string, tokenID *uuid.UUID) (ExplainOut, error) {
	ctx, me, err := s.enter(ctx, p)
	if err != nil {
		return ExplainOut{}, err
	}
	if d, _ := policy.From(ctx); d.Restricted && p.Role != "admin" {
		return ExplainOut{}, fail("forbidden", "policy.explain needs an unrestricted grant of the owner or an admin")
	}
	g := p.Grant
	if tokenID != nil {
		var vendor *string
		err := s.DB.QueryRow(ctx, `SELECT token_id, profile, project_allowlist, vendor FROM auth.token
		        WHERE token_id = $1 AND tenant_id = $2 AND litopys_root(actor_id) = $3`, *tokenID, p.TenantID, me).
			Scan(&g.TokenID, &g.Profile, &g.ProjectAllowlist, &vendor)
		if errors.Is(err, pgx.ErrNoRows) {
			return ExplainOut{}, fail("forbidden", "token %s is not a grant of yours", *tokenID)
		}
		if err != nil {
			return ExplainOut{}, err
		}
		g.Vendor = ""
		if vendor != nil {
			g.Vendor = *vendor
		}
	}
	d, err := policy.Decide(pg.AllProjects(ctx), s.DB, p.TenantID, me, g)
	if err != nil {
		return ExplainOut{}, err
	}
	projects, err := d.Explain(project)
	if err != nil {
		return ExplainOut{}, fail("project_not_found", "project %q", project)
	}
	return ExplainOut{Grant: g, Projects: projects}, nil
}
