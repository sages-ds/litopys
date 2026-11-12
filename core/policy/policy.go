// Package policy is the computed layer that answers "may this content go to this consumer".
// It decides; the database applies the decision through
// litopys.projects and the RLS of 0010, so no query can forget it. Every hidden thing is explained:
// aggregated in hidden[] (rule, type, count — no ids or names), in detail only by Explain.
package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/internal/pg"
)

// Profiles of a grant (D5-A).
const (
	ProfileFull     = "full"
	ProfileCore     = "core"
	ProfileChat     = "chat"
	ProfileReadonly = "readonly"
)

// Grant is what a consumer was given: the token's profile, project allowlist (nil = every project
// of the owner, empty = none — D9 default for non-Anthropic OAuth clients) and the client's vendor.
type Grant struct {
	TokenID          uuid.UUID `json:"token_id"`
	Profile          string    `json:"profile"`
	ProjectAllowlist []string  `json:"project_allowlist"`
	Vendor           string    `json:"vendor"`
	// DefaultDeny (D9): an OAuth grant of a non-Anthropic client sees no project until an owner (or
	// tenant) project_allowlist rule for its vendor opens some explicitly.
	DefaultDeny bool `json:"default_deny,omitempty"`
	// Derived (0022): a grant an orchestrator delegated to an executor for one step of one run.
	// Verbs bounds the tools (with the step's own ones added by the API), WriteMode "candidate" keeps the
	// executor to the step output, MaxDataClass is enforced with the data-class labels (Д9).
	Derived      bool       `json:"derived,omitempty"`
	Depth        int        `json:"depth,omitempty"`
	ExecutionID  *uuid.UUID `json:"execution_id,omitempty"`
	StepKey      string     `json:"step_key,omitempty"`
	Verbs        []string   `json:"verbs,omitempty"`
	WriteMode    string     `json:"write_mode,omitempty"`
	MaxDataClass string     `json:"max_data_class,omitempty"`
}

func (g Grant) profile() string {
	if g.Profile == "" {
		return ProfileFull
	}
	return g.Profile
}

func (g Grant) vendor() string {
	if g.Vendor == "" {
		return "unknown"
	}
	return g.Vendor
}

// Rule is the vocabulary of slice 1 (domain/types.yaml PolicyRule).
type Rule struct {
	Type      string   `json:"type"`
	Projects  []string `json:"projects,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Fields    []string `json:"fields,omitempty"`
	Kinds     []string `json:"kinds,omitempty"`
	ClientIDs []string `json:"client_ids,omitempty"`
	Vendors   []string `json:"vendors,omitempty"`
	Profiles  []string `json:"profiles,omitempty"`
}

// Rule types.
const (
	ProjectAllowlist = "project_allowlist"
	ProjectLabelDeny = "project_label_deny"
	FieldRedaction   = "field_redaction"
	ClientAllowlist  = "client_allowlist"
)

var ruleTypes = []string{ProjectAllowlist, ProjectLabelDeny, FieldRedaction, ClientAllowlist}

// Validate checks a rule against the vocabulary and its scope.
func (r Rule) Validate(scope string) error {
	if !slices.Contains(ruleTypes, r.Type) {
		return fmt.Errorf("rule.type %q not in %v", r.Type, ruleTypes)
	}
	switch r.Type {
	case ProjectAllowlist:
		if scope != "owner" {
			return errors.New("project_allowlist names projects of one person: scope owner only")
		}
	case ProjectLabelDeny:
		if len(r.Labels) == 0 {
			return errors.New("project_label_deny needs labels")
		}
	case FieldRedaction:
		if len(r.Fields) == 0 {
			return errors.New("field_redaction needs fields")
		}
		for _, f := range r.Fields {
			if f != "fact.secret_ref" && f != "fact.value" {
				return fmt.Errorf("field %q: fact.secret_ref | fact.value", f)
			}
		}
	case ClientAllowlist:
		if scope != "tenant" {
			return errors.New("client_allowlist: scope tenant only")
		}
	}
	return nil
}

// applies: empty selectors match any vendor / profile.
func (r Rule) applies(g Grant) bool {
	return (len(r.Vendors) == 0 || slices.Contains(r.Vendors, g.vendor())) &&
		(len(r.Profiles) == 0 || slices.Contains(r.Profiles, g.profile()))
}

// Stored is a rule as projected from policy.set.
type Stored struct {
	PolicyID   uuid.UUID `json:"policy_id"`
	Scope      string    `json:"scope"`
	Name       string    `json:"name"`
	Rule       Rule      `json:"rule"`
	Reason     *string   `json:"reason,omitempty"`
	SetSeq     int64     `json:"set_seq"`
	SetEventID uuid.UUID `json:"set_event_id"`
}

func (s Stored) by() string { return "policy." + s.Scope + "." + s.Name }

// Hidden is the aggregated report of what a decision hides (domain/types.yaml Hidden).
type Hidden struct {
	HiddenBy   string `json:"hidden_by"`
	EntityType string `json:"entity_type"`
	Count      int    `json:"count"`
	Reason     string `json:"reason"`
}

// Redaction blanks fact fields for this grant.
type Redaction struct {
	Fields []string
	Kinds  []string
	By     string
}

// Decision is the policy's answer for one request.
type Decision struct {
	Grant      Grant
	Restricted bool        // some project is hidden: Visible is the whole visible set
	Visible    []uuid.UUID // valid when Restricted
	Hidden     []Hidden
	Redactions []Redaction
	rules      []Stored
	projects   []project
}

// RedactFact applies the field redactions to a fact of kind; reports whether anything was blanked.
func (d Decision) RedactFact(kind string, value, secretRef *string) (*string, *string, bool) {
	hit := false
	for _, r := range d.Redactions {
		if len(r.Kinds) > 0 && !slices.Contains(r.Kinds, kind) {
			continue
		}
		if slices.Contains(r.Fields, "fact.value") && value != nil {
			value, hit = nil, true
		}
		if slices.Contains(r.Fields, "fact.secret_ref") && secretRef != nil {
			secretRef, hit = nil, true
		}
	}
	return value, secretRef, hit
}

// RedactsValues: some redaction blanks fact values (then values must not be matched either).
func (d Decision) RedactsValues() bool {
	for _, r := range d.Redactions {
		if slices.Contains(r.Fields, "fact.value") {
			return true
		}
	}
	return false
}

// Readonly: the grant may run queries only.
func (d Decision) Readonly() bool { return d.Grant.profile() == ProfileReadonly }

// hasAllowlistRule: some project_allowlist rule applies to this grant.
func (d Decision) hasAllowlistRule() bool {
	for _, s := range d.rules {
		if s.Rule.Type == ProjectAllowlist && s.Rule.applies(d.Grant) {
			return true
		}
	}
	return false
}

// AllowsNewProject: a restricted grant may create only projects its allowlists name.
func (d Decision) AllowsNewProject(name string) bool {
	if d.Grant.DefaultDeny && !d.hasAllowlistRule() {
		return false
	}
	if d.Grant.ProjectAllowlist != nil && !slices.Contains(d.Grant.ProjectAllowlist, name) {
		return false
	}
	for _, s := range d.rules {
		if s.Rule.Type == ProjectAllowlist && s.Rule.applies(d.Grant) && !slices.Contains(s.Rule.Projects, name) {
			return false
		}
	}
	return true
}

// DB is what the policy reads with: projections under the owner (litopys.owner set, no project
// restriction yet).
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type project struct {
	id        uuid.UUID
	name      string
	labels    []string
	dataClass string
}

// Data classes, least to most restricted (0023).
var dataClasses = []string{"public", "internal", "local"}

// ClassRank orders data classes; an unknown or empty class ranks as public.
func ClassRank(c string) int { return max(slices.Index(dataClasses, c), 0) }

// verdict for one project: the first rule that hides it, or "" when visible.
type verdict struct {
	by, reason string
	rules      []Explained
}

// Explained is one rule's effect on one project (Explain).
type Explained struct {
	By         string    `json:"by"`
	Type       string    `json:"type"`
	Effect     string    `json:"effect"` // allow | deny | n/a
	SetSeq     int64     `json:"set_seq,omitempty"`
	SetEventID uuid.UUID `json:"set_event_id,omitempty"`
	Reason     *string   `json:"reason,omitempty"`
}

func (d Decision) judge(p project) verdict {
	var v verdict
	deny := func(by, reason string) {
		if v.by == "" {
			v.by, v.reason = by, reason
		}
	}
	if d.Grant.DefaultDeny && !d.hasAllowlistRule() {
		v.rules = append(v.rules, Explained{By: "grant.default_deny", Type: ProjectAllowlist, Effect: "deny"})
		deny("grant.default_deny", "projects are opened explicitly for this client (D9)")
	}
	if mc := d.Grant.MaxDataClass; mc != "" {
		if ClassRank(p.dataClass) > ClassRank(mc) {
			v.rules = append(v.rules, Explained{By: "grant.max_data_class", Type: "data_class", Effect: "deny"})
			deny("grant.max_data_class", "project data class "+p.dataClass+" is above the grant's "+mc)
		} else {
			v.rules = append(v.rules, Explained{By: "grant.max_data_class", Type: "data_class", Effect: "allow"})
		}
	}
	if al := d.Grant.ProjectAllowlist; al != nil {
		if slices.Contains(al, p.name) {
			v.rules = append(v.rules, Explained{By: "grant.project_allowlist", Type: ProjectAllowlist, Effect: "allow"})
		} else {
			v.rules = append(v.rules, Explained{By: "grant.project_allowlist", Type: ProjectAllowlist, Effect: "deny"})
			deny("grant.project_allowlist", "the grant names the projects it may see")
		}
	}
	for _, s := range d.rules {
		if !s.Rule.applies(d.Grant) {
			continue
		}
		e := Explained{By: s.by(), Type: s.Rule.Type, Effect: "n/a", SetSeq: s.SetSeq, SetEventID: s.SetEventID, Reason: s.Reason}
		switch s.Rule.Type {
		case ProjectAllowlist:
			e.Effect = "allow"
			if !slices.Contains(s.Rule.Projects, p.name) {
				e.Effect = "deny"
				deny(s.by(), "project not in the allowlist for this consumer")
			}
		case ProjectLabelDeny:
			for _, l := range p.labels {
				if slices.Contains(s.Rule.Labels, l) {
					e.Effect = "deny"
					deny(s.by(), "project label "+l+" is denied for this consumer")
					break
				}
			}
		default:
			continue // field / client rules do not decide visibility
		}
		v.rules = append(v.rules, e)
	}
	return v
}

// Decide computes the decision for the owner's request under grant g.
func Decide(ctx context.Context, db DB, tenant, owner uuid.UUID, g Grant) (Decision, error) {
	d := Decision{Grant: g, Hidden: []Hidden{}}
	rows, err := db.Query(ctx, `SELECT policy_id, scope, name, rule, reason, set_seq, set_event_id FROM projection.policy
	        WHERE tenant_id = $1 AND active AND (scope = 'tenant' OR owner_id = $2) ORDER BY scope DESC, name`, tenant, owner)
	if err != nil {
		return d, err
	}
	d.rules, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Stored, error) {
		var s Stored
		var raw []byte
		if err := r.Scan(&s.PolicyID, &s.Scope, &s.Name, &raw, &s.Reason, &s.SetSeq, &s.SetEventID); err != nil {
			return s, err
		}
		return s, json.Unmarshal(raw, &s.Rule)
	})
	if err != nil {
		return d, err
	}
	for _, s := range d.rules {
		if s.Rule.Type == FieldRedaction && s.Rule.applies(g) {
			d.Redactions = append(d.Redactions, Redaction{Fields: s.Rule.Fields, Kinds: s.Rule.Kinds, By: s.by()})
		}
	}
	rows, err = db.Query(ctx, `SELECT project_id, name, labels, data_class FROM projection.project
	        WHERE tenant_id = $1 AND owner_id = $2 ORDER BY name`, tenant, owner)
	if err != nil {
		return d, err
	}
	d.projects, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (project, error) {
		var p project
		return p, r.Scan(&p.id, &p.name, &p.labels, &p.dataClass)
	})
	if err != nil {
		return d, err
	}
	counts := map[string]*Hidden{}
	var order []string
	for _, p := range d.projects {
		v := d.judge(p)
		if v.by == "" {
			d.Visible = append(d.Visible, p.id)
			continue
		}
		h, ok := counts[v.by]
		if !ok {
			h = &Hidden{HiddenBy: v.by, EntityType: "project", Reason: v.reason}
			counts[v.by] = h
			order = append(order, v.by)
		}
		h.Count++
	}
	for _, by := range order {
		d.Hidden = append(d.Hidden, *counts[by])
	}
	// a grant with an allowlist is restricted even before its projects exist (new names stay hidden)
	d.Restricted = len(d.Hidden) > 0 || g.ProjectAllowlist != nil || g.DefaultDeny
	if d.Restricted && d.Visible == nil {
		d.Visible = []uuid.UUID{}
	}
	return d, nil
}

// ExplainedProject is the detail Explain gives about one project.
type ExplainedProject struct {
	Project   string      `json:"project"`
	Labels    []string    `json:"labels"`
	DataClass string      `json:"data_class"`
	Visible   bool        `json:"visible"`
	HiddenBy  string      `json:"hidden_by,omitempty"`
	Rules     []Explained `json:"rules"`
}

// Explain details the decision per project (all of the owner's, or one by name). Only the owner of
// the data (or an admin) may call it — the caller enforces that.
func (d Decision) Explain(name string) ([]ExplainedProject, error) {
	out := []ExplainedProject{}
	for _, p := range d.projects {
		if name != "" && p.name != name {
			continue
		}
		v := d.judge(p)
		rules := v.rules
		if rules == nil {
			rules = []Explained{}
		}
		out = append(out, ExplainedProject{Project: p.name, Labels: p.labels, DataClass: p.dataClass, Visible: v.by == "", HiddenBy: v.by, Rules: rules})
	}
	if name != "" && len(out) == 0 {
		return nil, fmt.Errorf("project %q not found", name)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// the decision travels with the request
// ---------------------------------------------------------------------------

type ctxKey struct{}

// With stores the decision of the request.
func With(ctx context.Context, d Decision) context.Context {
	return context.WithValue(ctx, ctxKey{}, d)
}

// Scoped carries a decision with the request; the visible projects go to litopys.projects, so the RLS
// of 0010 applies it to every query of the request (internal/pg.PrepareConn). A grant's data class goes to
// litopys.max_class: the RLS of 0023 also hides records raised above it inside visible projects.
func Scoped(ctx context.Context, d Decision) context.Context {
	ctx = With(ctx, d)
	if d.Grant.MaxDataClass != "" {
		ctx = pg.WithMaxClass(ctx, d.Grant.MaxDataClass)
	}
	if d.Restricted {
		ids := make([][16]byte, len(d.Visible))
		for i, id := range d.Visible {
			ids[i] = id
		}
		ctx = pg.WithProjects(ctx, ids)
	}
	return ctx
}

// From returns the decision of the request and whether one was made.
func From(ctx context.Context) (Decision, bool) {
	d, ok := ctx.Value(ctxKey{}).(Decision)
	return d, ok
}

// PolicyID is the deterministic entity id of a rule: re-setting the same name replaces it.
func PolicyID(tenant uuid.UUID, scope string, owner uuid.UUID, name string) uuid.UUID {
	if scope == "tenant" {
		owner = uuid.Nil
	}
	return uuid.NewSHA1(nsPolicy, []byte(tenant.String()+"\x00"+scope+"\x00"+owner.String()+"\x00"+name))
}

var nsPolicy = uuid.MustParse("6f1c1a7e-2d7b-5e0b-9a51-3a9c7b1f0a03")

// ClientAllowed applies the tenant's client_allowlist rules (D9): without such a rule every client the
// IdP admitted is allowed; with one, only the listed client ids (exact) are.
func ClientAllowed(ctx context.Context, db DB, tenant uuid.UUID, clientID string) (bool, error) {
	rows, err := db.Query(ctx, `SELECT rule FROM projection.policy
	        WHERE tenant_id = $1 AND scope = 'tenant' AND active AND rule->>'type' = 'client_allowlist'`, tenant)
	if err != nil {
		return false, err
	}
	raws, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return false, err
	}
	if len(raws) == 0 {
		return true, nil
	}
	for _, raw := range raws {
		var r Rule
		if json.Unmarshal(raw, &r) == nil && slices.Contains(r.ClientIDs, clientID) {
			return true, nil
		}
	}
	return false, nil
}

// ExecutorClass is the data class an executor profile (projection.actor.executor) may receive for a project: its
// max_data_class, or every class when a person widened its access to all projects or to this one (owner's decision
// 2026-11-01). nil profile (a session's subagent) and no class — "" (no limit of its own).
func ExecutorClass(profile []byte, project string) string {
	if len(profile) == 0 {
		return ""
	}
	var x struct {
		MaxDataClass string `json:"max_data_class"`
		Access       *struct {
			Mode     string   `json:"mode"`
			Projects []string `json:"projects"`
		} `json:"access"`
	}
	if json.Unmarshal(profile, &x) != nil {
		return "public" // an unreadable profile gets the narrowest class, never more
	}
	if a := x.Access; a != nil && (a.Mode == "all" || (a.Mode == "projects" && slices.Contains(a.Projects, project))) {
		return dataClasses[len(dataClasses)-1]
	}
	return x.MaxDataClass
}
