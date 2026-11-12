package query

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/internal/pg"
)

// ActorListIn is actor.list: who has access to this tenant (owner decision 2026-10-30).
type ActorListIn struct {
	Query          string `json:"query,omitempty"` // substring of the name or the e-mail, case-insensitive
	Kind           string `json:"kind,omitempty"`  // human | agent | service | automation
	IncludeRevoked bool   `json:"include_revoked,omitempty"`
	Limit          int    `json:"limit,omitempty"`
}

// ActorRow is one actor with what it can use: its tokens and OAuth grants.
type ActorRow struct {
	ActorID      uuid.UUID    `json:"actor_id"`
	Kind         string       `json:"kind"`
	DisplayName  string       `json:"display_name"`
	Email        string       `json:"email,omitempty"`
	OnBehalfOf   *uuid.UUID   `json:"on_behalf_of,omitempty"`
	RegisteredAt time.Time    `json:"registered_at"`
	RevokedAt    *time.Time   `json:"revoked_at,omitempty"`
	Role         string       `json:"role,omitempty"` // admin | owner from the live tokens; "" without one
	Tokens       ActorTokens  `json:"tokens"`
	OAuth        []ActorOAuth `json:"oauth"`
}

// ActorTokens counts the live static tokens (LTP_) of an actor.
type ActorTokens struct {
	Active     int        `json:"active"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// ActorOAuth is one live OAuth grant: which client of which vendor, with what profile.
type ActorOAuth struct {
	ClientID   string     `json:"client_id"`
	Vendor     string     `json:"vendor"`
	Profile    string     `json:"profile"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// ActorListOut is the page; Truncated: more actors match than the limit.
type ActorListOut struct {
	Items     []ActorRow `json:"items"`
	Truncated bool       `json:"truncated,omitempty"`
}

// ActorList reads the directory across people, so it runs in the admin scope (RLS of 0008, as the
// admin tools of the 1.19 facade) and only for an admin principal. Read-only: no role changes here.
func (s *Service) ActorList(ctx context.Context, p Principal, in ActorListIn) (ActorListOut, error) {
	ctx, _, err := s.enter(ctx, p)
	if err != nil {
		return ActorListOut{}, err
	}
	if p.Role != "admin" {
		return ActorListOut{}, fail("forbidden", "actor.list is for an admin")
	}
	switch in.Kind {
	case "", "human", "agent", "service", "automation":
	default:
		return ActorListOut{}, fail("invalid_input", "kind: human | agent | service | automation")
	}
	limit := in.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(in.Query) + "%"
	ctx = pg.WithScope(ctx, pg.ScopeAdmin)
	rows, err := s.DB.Query(ctx, `SELECT a.actor_id, a.kind, a.display_name, COALESCE(a.external_identity, ''), a.on_behalf_of,
	        j.recorded_at, a.revoked_at,
	        CASE WHEN bool_or(t.role = 'admin') THEN 'admin' WHEN count(t.token_id) > 0 THEN 'owner' ELSE '' END,
	        count(t.token_id), max(t.last_used_at),
	        COALESCE((SELECT jsonb_agg(jsonb_build_object('client_id', g.client_id, 'vendor', g.vendor, 'profile', g.profile,
	                                   'last_used_at', g.last_used_at) ORDER BY g.client_id)
	                    FROM auth.oauth_grant g WHERE g.actor_id = a.actor_id AND g.revoked_at IS NULL), '[]')
	   FROM projection.actor a
	   JOIN journal j ON j.tenant_id = a.tenant_id AND j.seq = a.registered_seq
	   LEFT JOIN auth.token t ON t.actor_id = a.actor_id AND t.revoked_at IS NULL
	  WHERE a.tenant_id = $1 AND ($2 = '' OR a.kind = $2) AND ($3 OR a.revoked_at IS NULL)
	    AND ($4 = '%%' OR a.display_name ILIKE $4 OR a.external_identity ILIKE $4)
	  GROUP BY a.actor_id, j.recorded_at
	  ORDER BY a.kind = 'human' DESC, a.kind, lower(a.display_name), a.actor_id
	  LIMIT $5`, p.TenantID, in.Kind, in.IncludeRevoked, like, limit+1)
	if err != nil {
		return ActorListOut{}, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ActorRow, error) {
		var x ActorRow
		err := r.Scan(&x.ActorID, &x.Kind, &x.DisplayName, &x.Email, &x.OnBehalfOf, &x.RegisteredAt, &x.RevokedAt,
			&x.Role, &x.Tokens.Active, &x.Tokens.LastUsedAt, &x.OAuth)
		return x, err
	})
	if err != nil {
		return ActorListOut{}, err
	}
	out := ActorListOut{Items: items}
	if len(items) > limit {
		out.Items, out.Truncated = items[:limit], true
	}
	return out, nil
}
