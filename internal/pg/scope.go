package pg

import (
	"context"
	"encoding/hex"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Who is asking, for the row-level security of 0008: every connection taken from a pool
// gets the session settings litopys.owner and litopys.scope from the request context. They are set
// on every acquire, empty values included, so a connection never carries the previous caller.
// No owner in the context means zero visible rows (fail closed).

// Scopes widen what litopys_app may read. The projector scope is set by
// projector.Apply itself (SET LOCAL), never through the context.
const (
	ScopeSystem = "system" // maintenance: import, rebuild, CLI — reads every owner
	ScopeAdmin  = "admin"  // directory of humans for admin tools — actors and their registration events
)

type ctxKey int

const (
	ownerKey ctxKey = iota
	scopeKey
	projectsKey
	maxClassKey
)

// WithOwner marks the request as made for the root human id (a uuid.UUID converts implicitly).
func WithOwner(ctx context.Context, id [16]byte) context.Context {
	return context.WithValue(ctx, ownerKey, id)
}

// WithScope sets the read scope of the request (ScopeSystem, ScopeAdmin).
func WithScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, scopeKey, scope)
}

// WithProjects restricts the request to the projects the policy layer decided are visible (0010);
// an empty slice means none. Without it the policy hides nothing.
func WithProjects(ctx context.Context, ids [][16]byte) context.Context {
	if ids == nil {
		ids = [][16]byte{}
	}
	return context.WithValue(ctx, projectsKey, ids)
}

// WithMaxClass hides rows whose data class is above class (public < internal < local; 0023).
// Empty means no limit.
func WithMaxClass(ctx context.Context, class string) context.Context {
	return context.WithValue(ctx, maxClassKey, class)
}

type allProjects struct{}

// AllProjects lifts a project restriction of the request (policy.explain must see what it explains).
func AllProjects(ctx context.Context) context.Context {
	return context.WithValue(ctx, projectsKey, allProjects{})
}

// Owner returns the root human of the request and whether one is set.
func Owner(ctx context.Context) ([16]byte, bool) {
	id, ok := ctx.Value(ownerKey).([16]byte)
	return id, ok && id != [16]byte{}
}

func uuidString(b [16]byte) string {
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:], b[10:])
	return string(s[:])
}

// PrepareConn is the pgxpool hook that applies the request's owner and scope to the connection.
func PrepareConn(ctx context.Context, c *pgx.Conn) (bool, error) {
	owner := ""
	if id, ok := Owner(ctx); ok {
		owner = uuidString(id)
	}
	scope, _ := ctx.Value(scopeKey).(string)
	projects := "" // unrestricted
	if ids, ok := ctx.Value(projectsKey).([][16]byte); ok {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = uuidString(id)
		}
		projects = "{" + strings.Join(parts, ",") + "}"
	}
	maxClass, _ := ctx.Value(maxClassKey).(string)
	if _, err := c.Exec(ctx, `SELECT set_config('litopys.owner', $1, false), set_config('litopys.scope', $2, false),
	        set_config('litopys.projects', $3, false), set_config('litopys.max_class', $4, false)`, owner, scope, projects, maxClass); err != nil {
		return false, err
	}
	return true, nil
}
