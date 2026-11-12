// Package auth issues and verifies Litopys bearer tokens.
//
// Token format: "LTP_" + base64url(32 random bytes). Only sha256(token) is stored (auth.token);
// the raw value is returned once by Issue. Tokens bind to a human actor and a role (owner | admin).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/internal/pg"
)

const Prefix = "LTP_"

// DB is the app-role connection (auth.token is readable/writable by litopys_app).
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

var (
	ErrInvalid = errors.New("invalid or revoked token")
	roles      = map[string]bool{"owner": true, "admin": true}
)

// Principal is who a valid token speaks for, with the grant it carries (profile, project
// allowlist, vendor of the client).
type Principal struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	Role      string
	TokenID   uuid.UUID
	Grant     policy.Grant
	ExpiresAt *time.Time // nil: a legacy token from before 0011
	OAuth     bool       // an access token of the broker-IdP (the native API)
	// Human: the person the token row belongs to. ActorID is the same person, except for a derived token
	// (0022), which speaks as its executor actor on behalf of Human.
	Human uuid.UUID
}

// Token lifetimes: every new token expires; admin tokens are short-lived.
var (
	DefaultTTL  = days("LITOPYS_TOKEN_TTL_DAYS", 90)
	MaxTTL      = 365 * 24 * time.Hour
	MaxAdminTTL = days("LITOPYS_ADMIN_TOKEN_TTL_DAYS", 7)
)

func days(env string, def int) time.Duration {
	if v, err := strconv.Atoi(os.Getenv(env)); err == nil && v > 0 {
		return time.Duration(v) * 24 * time.Hour
	}
	return time.Duration(def) * 24 * time.Hour
}

// ClampTTL: 0 = the default; never above the maximum of the role.
func ClampTTL(role string, ttl time.Duration) time.Duration {
	max := MaxTTL
	if role == "admin" {
		max = MaxAdminTTL
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > max {
		ttl = max
	}
	return ttl
}

func hash(raw string) []byte {
	h := sha256.Sum256([]byte(raw))
	return h[:]
}

// Issue creates a token for a human actor and returns the raw value — the only time it exists.
func Issue(ctx context.Context, db DB, tenant, actor uuid.UUID, role, label string, createdBy *uuid.UUID) (raw string, tokenID uuid.UUID, err error) {
	return IssueGrant(ctx, db, tenant, actor, role, label, createdBy, policy.Grant{}, 0)
}

// IssueGrant issues a token with grant attributes: profile (empty = full), project
// allowlist (nil = every project of the owner), the client's vendor (empty = unknown) and a lifetime
// (ClampTTL: 0 = default, capped by the role's maximum).
func IssueGrant(ctx context.Context, db DB, tenant, actor uuid.UUID, role, label string, createdBy *uuid.UUID, g policy.Grant, ttl time.Duration) (raw string, tokenID uuid.UUID, err error) {
	return IssueGrantUntil(ctx, db, tenant, actor, role, label, createdBy, g, ttl, nil)
}

// IssueGrantUntil is IssueGrant whose token also never outlives notAfter (nil = no bound): a token issued by
// another one ends no later than it. The bound is applied by the database, on the same clock as the
// lifetime — a lifetime computed from the application's clock (time.Until) let the new token outlive the issuing one
// by the skew between the two clocks.
func IssueGrantUntil(ctx context.Context, db DB, tenant, actor uuid.UUID, role, label string, createdBy *uuid.UUID, g policy.Grant, ttl time.Duration, notAfter *time.Time) (raw string, tokenID uuid.UUID, err error) {
	// the lifetime is counted by the database clock — the same clock Verify checks it with (expires_at > now()); an
	// application clock ahead of the database's would keep a just-expired token alive
	life := ClampTTL(role, ttl).Microseconds()
	if g.Profile == "" {
		g.Profile = policy.ProfileFull
	}
	var vendor *string
	if g.Vendor != "" {
		vendor = &g.Vendor
	}
	if !roles[role] {
		return "", uuid.Nil, errors.New("role must be owner or admin")
	}
	if len(label) > 100 {
		return "", uuid.Nil, errors.New("label longer than 100")
	}
	var kind string
	// the edge checks the actor before any owner is known: system scope (RLS of 0008)
	err = db.QueryRow(pg.WithScope(ctx, pg.ScopeSystem), `SELECT kind FROM projection.actor WHERE actor_id = $1 AND tenant_id = $2 AND revoked_at IS NULL`,
		actor, tenant).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", uuid.Nil, errors.New("actor not registered or revoked")
	}
	if err != nil {
		return "", uuid.Nil, err
	}
	if kind != "human" {
		return "", uuid.Nil, errors.New("tokens are issued to humans; agents act through their human's token")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", uuid.Nil, err
	}
	raw = Prefix + base64.RawURLEncoding.EncodeToString(b)
	h := hash(raw)
	var lbl *string
	if label != "" {
		lbl = &label
	}
	err = db.QueryRow(ctx, `INSERT INTO auth.token (tenant_id, actor_id, role, label, hash, fingerprint, created_by, profile, project_allowlist, vendor, expires_at)
	        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, LEAST(now() + $11::bigint * interval '1 microsecond', $12::timestamptz))
	        RETURNING token_id`,
		tenant, actor, role, lbl, h, hex.EncodeToString(h)[:12], createdBy, g.Profile, g.ProjectAllowlist, vendor, life, notAfter).Scan(&tokenID)
	return raw, tokenID, err
}

// Verify resolves a raw token. Revoked, unknown and malformed tokens are indistinguishable (ErrInvalid).
func Verify(ctx context.Context, db DB, raw string) (Principal, error) {
	var p Principal
	if !strings.HasPrefix(raw, Prefix) || len(raw) != len(Prefix)+43 {
		return p, ErrInvalid
	}
	var vendor, writeMode, dataClass, stepKey *string
	var kind string
	var executor, execution *uuid.UUID
	err := db.QueryRow(ctx, `UPDATE auth.token SET last_used_at = now()
	        WHERE hash = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())
	        RETURNING tenant_id, actor_id, role, token_id, profile, project_allowlist, vendor, expires_at,
	                  kind, depth, executor_actor_id, execution_id, step_key, verbs, write_mode, max_data_class`, hash(raw)).
		Scan(&p.TenantID, &p.ActorID, &p.Role, &p.TokenID, &p.Grant.Profile, &p.Grant.ProjectAllowlist, &vendor, &p.ExpiresAt,
			&kind, &p.Grant.Depth, &executor, &execution, &stepKey, &p.Grant.Verbs, &writeMode, &dataClass)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrInvalid
	}
	if err != nil {
		return p, err
	}
	p.Human = p.ActorID
	p.Grant.TokenID = p.TokenID
	if vendor != nil {
		p.Grant.Vendor = *vendor
	}
	if kind == "derived" { // the executor speaks; the chain and the run must still be alive
		alive, err := derivedAlive(ctx, db, p.TokenID, *execution)
		if err != nil {
			return p, err
		}
		if !alive {
			return Principal{}, ErrInvalid
		}
		p.ActorID = *executor
		p.Grant.Derived, p.Grant.ExecutionID, p.Grant.StepKey = true, execution, *stepKey
		p.Grant.WriteMode = *writeMode
		if dataClass != nil {
			p.Grant.MaxDataClass = *dataClass
		}
	}
	return p, nil
}

// Expired tells an expired token from an unknown or revoked one — only for the WWW-Authenticate
// description of a 401; never a reason to accept anything.
func Expired(ctx context.Context, db DB, raw string) bool {
	var expired bool
	err := db.QueryRow(ctx, `SELECT expires_at <= now() FROM auth.token WHERE hash = $1 AND revoked_at IS NULL AND expires_at IS NOT NULL`,
		hash(raw)).Scan(&expired)
	return err == nil && expired
}

// Revoke revokes a token of the tenant; revoking a revoked token is a no-op (false).
// Tokens derived from it are revoked with it (0022).
func Revoke(ctx context.Context, db DB, tenant, tokenID uuid.UUID) (bool, error) {
	var live bool
	if err := db.QueryRow(ctx, `SELECT revoked_at IS NULL FROM auth.token WHERE token_id = $1 AND tenant_id = $2`, tokenID, tenant).Scan(&live); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if !live {
		return false, nil
	}
	_, err := RevokeCascade(ctx, db, tenant, tokenID)
	return err == nil, err
}

// Token is listing metadata — never the raw value (it does not exist anywhere).
type Token struct {
	TokenID     uuid.UUID  `json:"tokenId"`
	ActorID     uuid.UUID  `json:"actorId"`
	Role        string     `json:"role"`
	Label       *string    `json:"label"`
	Fingerprint string     `json:"fingerprint"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastUsedAt  *time.Time `json:"lastUsedAt"`
	RevokedAt   *time.Time `json:"revokedAt"`
	ExpiresAt   *time.Time `json:"expiresAt"`
	Profile     string     `json:"profile"`
	Allowlist   []string   `json:"projectAllowlist"`
}

// List returns tokens of the tenant (optionally of one actor), newest first.
func List(ctx context.Context, db DB, tenant uuid.UUID, actor *uuid.UUID, includeRevoked bool) ([]Token, error) {
	rows, err := db.Query(ctx, `SELECT token_id, actor_id, role, label, fingerprint, created_at, last_used_at, revoked_at, expires_at, profile, project_allowlist
	        FROM auth.token WHERE tenant_id = $1 AND ($2::uuid IS NULL OR actor_id = $2) AND ($3 OR revoked_at IS NULL)
	        ORDER BY created_at DESC`, tenant, actor, includeRevoked)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Token, error) {
		var t Token
		return t, r.Scan(&t.TokenID, &t.ActorID, &t.Role, &t.Label, &t.Fingerprint, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt, &t.ExpiresAt, &t.Profile, &t.Allowlist)
	})
}
