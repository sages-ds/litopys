// Package verbs implements the slice-1 COMMAND verbs of domain/verbs.yaml.
//
// A command is one transaction: validate → idempotency check against the read models →
// journal.Append + projector.Apply for every event it emits → commit.
// Events of one command form a causation chain: the first is caused by the principal's
// run (or the caller-supplied cause), each next one by the previous event.
//
// Validation is hand-written from domain/types.yaml until the generators produce it.
package verbs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/journal"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/projector"
	"litopys.smart-gw.co.ua/internal/pg"
)

// DB is what commands need: transactions and reads (*pgxpool.Pool running as litopys_app).
type DB interface {
	pg.Beginner
	journal.Querier
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Service executes commands. Admin (a pool running as litopys_admin) is needed only by journal.purge.
type Service struct {
	DB    DB
	Admin DB
}

// Principal is who calls: resolved by the edge (auth) before a verb runs.
type Principal struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	RunID    uuid.UUID    // uuid.Nil: no run yet (context.open starts one)
	Cause    uuid.UUID    // optional causation of the first event; defaults to RunID
	Owner    uuid.UUID    // root human; uuid.Nil = resolved from ActorID by exec
	Role     string       // owner | admin (tenant-wide policy needs admin)
	Grant    policy.Grant // what the consumer was given; zero = full grant
}

// Error is a domain error with the code of verbs.yaml errors[].
type Error struct {
	Code    string
	Message string
}

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

// Ack is EventAck of verbs.yaml: the last event written, or why none was.
type Ack struct {
	EventID   uuid.UUID `json:"event_id"`
	Seq       int64     `json:"seq"`
	Already   bool      `json:"already,omitempty"`   // state was already like that — no event
	Deduped   bool      `json:"deduped,omitempty"`   // same request seen before — no event
	Unchanged bool      `json:"unchanged,omitempty"` // same value — no event
	// Warnings: accepted with a remark, e.g. type_legacy_alias. Not marshalled here: the edge puts them
	// into the warnings of its reply (api.Binding), a second "warnings" at the same depth would hide both.
	Warnings []string `json:"-"`
}

// ---------------------------------------------------------------------------
// command plumbing
// ---------------------------------------------------------------------------

type cmd struct {
	ctx    context.Context
	tx     pgx.Tx
	p      Principal
	run    uuid.UUID
	cause  uuid.UUID
	me     uuid.UUID // root human of the principal: owner of everything the command touches
	pol    policy.Decision
	events []*journal.Event
}

func (s *Service) exec(ctx context.Context, db DB, p Principal, fn func(c *cmd) error) error {
	if p.TenantID == uuid.Nil || p.ActorID == uuid.Nil {
		return fail("unauthenticated", "principal without tenant or actor")
	}
	me, err := Owner(ctx, db, p)
	if err != nil {
		return err
	}
	if p.Grant.Profile == policy.ProfileReadonly {
		return fail("policy_denied", "the grant is read-only (profile readonly)")
	}
	ctx = pg.WithOwner(ctx, me) // row-level security of 0008
	d, ok := policy.From(ctx)
	if !ok || d.Grant.TokenID != p.Grant.TokenID {
		if d, err = policy.Decide(ctx, db, p.TenantID, me, p.Grant); err != nil {
			return err
		}
		ctx = policy.Scoped(ctx, d) // hidden projects do not exist for the command either (0010)
	}
	return pg.InTx(ctx, db, func(tx pgx.Tx) error {
		if err := writable(ctx, tx); err != nil {
			return err
		}
		cause := p.Cause
		if cause == uuid.Nil {
			cause = p.RunID
		}
		return fn(&cmd{ctx: ctx, tx: tx, p: p, run: p.RunID, cause: cause, me: me, pol: d})
	})
}

// Owner returns the root human of the principal: p.Owner when the edge resolved it, else
// litopys_root(actor). An actor that is not registered has no owner.
func Owner(ctx context.Context, db interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, p Principal) (uuid.UUID, error) {
	if p.Owner != uuid.Nil {
		return p.Owner, nil
	}
	var root *uuid.UUID
	if err := db.QueryRow(ctx, `SELECT litopys_root($1)`, p.ActorID).Scan(&root); err != nil {
		return uuid.Nil, err
	}
	if root == nil {
		return uuid.Nil, fail("actor_not_registered", "actor %s", p.ActorID)
	}
	return *root, nil
}

// emit appends one event and projects it in the command's transaction.
func (c *cmd) emit(typ, entityType string, entity uuid.UUID, contextID *uuid.UUID, effective *time.Time, payload any) (*journal.Event, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	run := c.run
	if run == uuid.Nil { // bootstrap events (actor.registered by the system) have no run yet
		run = c.p.ActorID
	}
	cause := c.cause
	if cause == uuid.Nil {
		cause = run
	}
	corr := run
	e := &journal.Event{TenantID: c.p.TenantID, Type: typ, EffectiveAt: effective,
		ActorID: c.p.ActorID, RunID: run, CausationID: cause, CorrelationID: corr,
		EntityType: entityType, EntityID: entity, ContextID: contextID, Payload: b}
	if err := journal.Append(c.ctx, c.tx, e); err != nil {
		return nil, err
	}
	if err := projector.Apply(c.ctx, c.tx, e); err != nil {
		return nil, err
	}
	c.cause = e.EventID
	c.events = append(c.events, e)
	return e, nil
}

func ack(e *journal.Event) Ack { return Ack{EventID: e.EventID, Seq: e.Seq} }

// ---------------------------------------------------------------------------
// lookups
// ---------------------------------------------------------------------------

func (c *cmd) projectID(name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := c.tx.QueryRow(c.ctx, `SELECT project_id FROM projection.project WHERE tenant_id = $1 AND owner_id = $2 AND name = $3`,
		c.p.TenantID, c.me, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fail("project_not_found", "project %q", name)
	}
	return id, err
}

// allowNewProject: a restricted grant creates only projects its allowlists name.
func (c *cmd) allowNewProject(name string) error {
	if !c.pol.AllowsNewProject(name) {
		return fail("policy_denied", "project %q is not allowed for this client: an explicit allowlist (the token's project_allowlist, or an owner rule for its vendor) does not name it — the owner can add it", name)
	}
	return nil
}

// owner is the root human of an actor: on_behalf_of for delegated actors, the actor itself otherwise.
// litopys_root sees actors of every person (an actor of someone else must read as "not mine",
// not as "not registered").
func (c *cmd) owner(actor uuid.UUID) (uuid.UUID, error) {
	var root *uuid.UUID
	if err := c.tx.QueryRow(c.ctx, `SELECT litopys_root($1)`, actor).Scan(&root); err != nil {
		return uuid.Nil, err
	}
	if root == nil {
		return uuid.Nil, fail("actor_not_registered", "actor %s", actor)
	}
	return *root, nil
}

// mine: the principal and the author share the root human (built-in slice-1 policy
// «власник бачить усе своє»; delegated agents act for their human).
func (c *cmd) mine(author uuid.UUID) error {
	me, err := c.owner(c.p.ActorID)
	if err != nil {
		return err
	}
	them, err := c.owner(author)
	if err != nil {
		return err
	}
	if me != them {
		return fail("forbidden", "not the owner")
	}
	return nil
}

func (c *cmd) requireLiveActor() error {
	var revoked *time.Time
	err := c.tx.QueryRow(c.ctx, `SELECT revoked_at FROM projection.actor WHERE actor_id = $1`, c.p.ActorID).Scan(&revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return fail("actor_not_registered", "actor %s", c.p.ActorID)
	}
	if err != nil {
		return err
	}
	if revoked != nil {
		return fail("actor_revoked", "actor %s", c.p.ActorID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// validation (domain/types.yaml)
// ---------------------------------------------------------------------------

var (
	projectNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	labelRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)
	policyNameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	consumerNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

func invalid(field, format string, args ...any) *Error {
	return fail("invalid_input", "%s: "+format, append([]any{field}, args...)...)
}

func checkLen(field, s string, min, max int) error {
	n := utf8.RuneCountInString(s)
	if n < min || n > max {
		return invalid(field, "length %d not in [%d, %d]", n, min, max)
	}
	return nil
}

func checkEnum(field, v string, allowed ...string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return invalid(field, "%q not in %v", v, allowed)
}

func checkProjectName(field, v string) error {
	if err := checkLen(field, v, 1, 200); err != nil {
		return err
	}
	if !projectNameRe.MatchString(v) {
		return invalid(field, "%q does not match %s", v, projectNameRe)
	}
	return nil
}

var (
	statementTypes = []string{"bugfix", "feature", "refactor", "decision", "discovery", "config", "note", "question", "proposal", "document"}
	projectStatus  = []string{"active", "paused", "archived"}
	taskPriorities = []string{"high", "medium", "low"}
	taskOutcomes   = []string{"done", "dropped"}
	factKinds      = []string{"architecture", "credential", "access", "note", "tech_stack", "binding", "instruction", "style", "agent", "skill", "retention", "vocabulary"}
	runOutcomes    = []string{"ok", "failed", "aborted", "timeout"}
	entityTypes    = []string{"actor", "run", "context", "statement", "summary", "link", "project", "task", "fact",
		"consumer", "referent", "subject", "edge", "policy", "publication", "workflow"}
	// relations and their ends: linktypes.go (generated table)
)

func checkList(field string, v []string, maxItems, maxLen int) error {
	if len(v) > maxItems {
		return invalid(field, "%d items, max %d", len(v), maxItems)
	}
	for i, s := range v {
		if err := checkLen(fmt.Sprintf("%s[%d]", field, i), s, 1, maxLen); err != nil {
			return err
		}
	}
	return nil
}

func optLen(field string, s *string, max int) error {
	if s == nil {
		return nil
	}
	return checkLen(field, *s, 0, max)
}

// Deterministic ids: entities addressed by a natural key get a UUIDv5 of that key, so every
// event about the same key carries the same entity_id (fact.retired, consumer cursor).
var (
	nsFact     = uuid.MustParse("6f1c1a7e-2d7b-5e0b-9a51-3a9c7b1f0a01")
	nsConsumer = uuid.MustParse("6f1c1a7e-2d7b-5e0b-9a51-3a9c7b1f0a02")
)

// FactID is the deterministic entity id of a fact: every event about (project, kind, key) shares it.
func FactID(project uuid.UUID, kind, key string) uuid.UUID {
	return uuid.NewSHA1(nsFact, []byte(project.String()+"\x00"+kind+"\x00"+key))
}

// ConsumerID is the deterministic entity id of a named consumer of one person (cursors are per
// owner since 0008).
func ConsumerID(tenant, owner uuid.UUID, name string) uuid.UUID {
	return uuid.NewSHA1(nsConsumer, []byte(tenant.String()+"\x00"+owner.String()+"\x00"+name))
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
