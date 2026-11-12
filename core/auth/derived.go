package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/internal/pg"
)

// Derived grants (0022): an orchestrator delegates one step of one run to an executor.
// The token is never wider than its parent: the parent's profile, the run's project within the parent's
// allowlist, the step's verbs (within the parent's when the parent is derived itself), at most the parent's
// lifetime and MaxDerivedTTL, depth at most MaxDepth. It lives while every ancestor lives and the run is open.

const (
	MaxDepth      = 3
	MaxDerivedTTL = 24 * time.Hour
	// DelegateVerb lets a derived token delegate further (when its step lists it).
	DelegateVerb = "step.delegate"
)

// ErrDelegation: the parent cannot delegate this (the message says why; safe to show the caller).
type ErrDelegation struct{ Reason string }

func (e *ErrDelegation) Error() string { return e.Reason }

func refuse(format string, a ...any) error { return &ErrDelegation{Reason: fmt.Sprintf(format, a...)} }

// DerivedIn is what a derived token is bound to.
type DerivedIn struct {
	ExecutorActorID uuid.UUID
	ExecutionID     uuid.UUID
	Project         string // the run's project (slug): the only project the token sees
	StepKey         string
	Verbs           []string
	WriteMode       string // direct | candidate
	MaxDataClass    string // "" = not set
	TTL             time.Duration
}

// IssueDerived issues a derived token from the parent principal inside the caller's transaction and returns
// the raw value (shown once), its id and expiry.
func IssueDerived(ctx context.Context, db DB, parent Principal, in DerivedIn) (raw string, tokenID uuid.UUID, expires time.Time, err error) {
	if parent.OAuth || parent.TokenID == uuid.Nil {
		return "", uuid.Nil, expires, refuse("delegation needs a Litopys token (LTP_) of the orchestrator; an OAuth connection cannot delegate yet")
	}
	switch parent.Grant.Profile {
	case "", policy.ProfileFull, "core":
	default:
		return "", uuid.Nil, expires, refuse("profile %s cannot delegate: core or full is needed", parent.Grant.Profile)
	}
	depth := parent.Grant.Depth + 1
	if depth > MaxDepth {
		return "", uuid.Nil, expires, refuse("delegation depth %d is above the limit %d", depth, MaxDepth)
	}
	if parent.Grant.Derived {
		if !slices.Contains(parent.Grant.Verbs, DelegateVerb) {
			return "", uuid.Nil, expires, refuse("this executor's step does not allow delegating further")
		}
		for _, v := range in.Verbs {
			if !slices.Contains(parent.Grant.Verbs, v) {
				return "", uuid.Nil, expires, refuse("verb %s is wider than the delegating grant", v)
			}
		}
		if parent.Grant.WriteMode == "candidate" && in.WriteMode != "candidate" {
			in.WriteMode = "candidate" // narrowing only
		}
	}
	if parent.Grant.ProjectAllowlist != nil && !slices.Contains(parent.Grant.ProjectAllowlist, in.Project) {
		return "", uuid.Nil, expires, refuse("project %s is outside the delegating grant", in.Project)
	}
	if in.WriteMode == "" {
		in.WriteMode = "direct"
	}
	ttl := in.TTL
	if ttl <= 0 || ttl > MaxDerivedTTL {
		ttl = MaxDerivedTTL
	}
	// counted from the database clock — the clock the chain check uses (expires_at <= now()), not the application's
	var parentExp *time.Time
	var dbNow time.Time
	if err := db.QueryRow(pg.WithScope(ctx, pg.ScopeSystem), `SELECT expires_at, now() FROM auth.token WHERE token_id = $1`, parent.TokenID).Scan(&parentExp, &dbNow); err != nil {
		return "", uuid.Nil, expires, err
	}
	expires = dbNow.Add(ttl).Truncate(time.Second)
	if parentExp != nil && parentExp.Before(expires) {
		expires = *parentExp
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", uuid.Nil, expires, err
	}
	raw = Prefix + base64.RawURLEncoding.EncodeToString(b)
	h := hash(raw)
	profile := parent.Grant.Profile
	if profile == "" {
		profile = policy.ProfileFull
	}
	var dc *string
	if in.MaxDataClass != "" {
		dc = &in.MaxDataClass
	}
	label := "derived " + in.StepKey
	err = db.QueryRow(ctx, `INSERT INTO auth.token (tenant_id, actor_id, role, label, hash, fingerprint, created_by, profile, project_allowlist,
	        vendor, expires_at, kind, parent_token_id, depth, executor_actor_id, execution_id, step_key, verbs, write_mode, max_data_class)
	        VALUES ($1, $2, 'owner', $3, $4, $5, $6, $7, $8, 'unknown', $9, 'derived', $10, $11, $12, $13, $14, $15, $16, $17) RETURNING token_id`,
		parent.TenantID, ownerHuman(parent), label, h, hex.EncodeToString(h)[:12], parent.ActorID, profile, []string{in.Project},
		expires, parent.TokenID, depth, in.ExecutorActorID, in.ExecutionID, in.StepKey, in.Verbs, in.WriteMode, dc).Scan(&tokenID)
	return raw, tokenID, expires, err
}

// ownerHuman: the human a token row belongs to — the parent's, also when the parent is derived.
func ownerHuman(p Principal) uuid.UUID {
	if p.Human != uuid.Nil {
		return p.Human
	}
	return p.ActorID
}

// derivedAlive: every ancestor is unrevoked and unexpired, and the run is open (system scope: the edge has no
// owner yet; RLS of 0008).
func derivedAlive(ctx context.Context, db DB, tokenID, executionID uuid.UUID) (bool, error) {
	var ok bool
	err := db.QueryRow(pg.WithScope(ctx, pg.ScopeSystem), `WITH RECURSIVE chain AS (
	        SELECT token_id, parent_token_id, revoked_at, expires_at, 0 AS hops FROM auth.token WHERE token_id = $1
	        UNION ALL
	        SELECT t.token_id, t.parent_token_id, t.revoked_at, t.expires_at, c.hops + 1 FROM auth.token t
	          JOIN chain c ON t.token_id = c.parent_token_id WHERE c.hops < 8)
	      SELECT NOT EXISTS (SELECT 1 FROM chain WHERE revoked_at IS NOT NULL OR (expires_at IS NOT NULL AND expires_at <= now()))
	         AND EXISTS (SELECT 1 FROM projection.execution WHERE execution_id = $2 AND state = 'working')`, tokenID, executionID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// RevokeCascade revokes a token and every token derived from it.
func RevokeCascade(ctx context.Context, db DB, tenant, tokenID uuid.UUID) (int64, error) {
	tag, err := db.Exec(ctx, `WITH RECURSIVE tree AS (
	        SELECT token_id FROM auth.token WHERE token_id = $1 AND tenant_id = $2
	        UNION ALL
	        SELECT t.token_id FROM auth.token t JOIN tree ON t.parent_token_id = tree.token_id)
	      UPDATE auth.token SET revoked_at = now() WHERE token_id IN (SELECT token_id FROM tree) AND revoked_at IS NULL`, tokenID, tenant)
	return tag.RowsAffected(), err
}
