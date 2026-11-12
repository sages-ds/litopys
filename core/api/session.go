package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"

	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/verbs"
)

// ---------------------------------------------------------------------------
// projects: slug or title
// ---------------------------------------------------------------------------

var slugRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// project resolves a slug or a human title to the project's slug and id: the exact slug first, then a
// title that matches without regard to case — exactly one, else project_ambiguous.
func (a *API) project(ctx context.Context, c *Caller, ref string) (string, uuid.UUID, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", uuid.Nil, invalid("project: empty")
	}
	var id uuid.UUID
	err := a.DB.QueryRow(ctx, `SELECT project_id FROM projection.project WHERE tenant_id = $1 AND owner_id = $2 AND name = $3`,
		c.TenantID, c.Owner, ref).Scan(&id)
	if err == nil {
		return ref, id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", uuid.Nil, err
	}
	rows, err := a.DB.Query(ctx, `SELECT project_id, name FROM projection.project
	         WHERE tenant_id = $1 AND owner_id = $2 AND lower(COALESCE(title, name)) = lower($3) ORDER BY name LIMIT 6`,
		c.TenantID, c.Owner, strings.Join(strings.Fields(ref), " "))
	if err != nil {
		return "", uuid.Nil, err
	}
	type hit struct {
		id   uuid.UUID
		name string
	}
	hits, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (hit, error) {
		var h hit
		return h, r.Scan(&h.id, &h.name)
	})
	if err != nil {
		return "", uuid.Nil, err
	}
	switch len(hits) {
	case 0:
		return "", uuid.Nil, errorf("project_not_found", "no project with slug or title %q", ref)
	case 1:
		return hits[0].name, hits[0].id, nil
	}
	names := make([]string, len(hits))
	for i, h := range hits {
		names[i] = h.name
	}
	return "", uuid.Nil, errorf("project_ambiguous", "title %q matches %s", ref, strings.Join(names, ", "))
}

// translit: Ukrainian romanization of 2010 (Cabinet resolution No. 55); the first-letter forms of є, ї,
// й, ю, я are handled by Slug. Russian-only letters get their common Latin forms.
var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "h", 'ґ': "g", 'д': "d", 'е': "e", 'є': "ie", 'ж': "zh", 'з': "z",
	'и': "y", 'і': "i", 'ї': "i", 'й': "i", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p",
	'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch",
	'ь': "", 'ю': "iu", 'я': "ia", '\'': "", '’': "", 'ʼ': "",
	'ы': "y", 'э': "e", 'ё': "io", 'ъ': "",
}

var wordInitial = map[rune]string{'є': "ye", 'ї': "yi", 'й': "y", 'ю': "yu", 'я': "ya"}

// Slug derives a project slug from a human title: transliteration, Latin diacritics
// dropped, lower case, everything else → "-", at most 60 characters. "" when nothing is left.
func Slug(title string) string {
	var b strings.Builder
	prevLetter := false
	runes := []rune(strings.ToLower(title))
	for i, r := range runes {
		if r == 'г' && i > 0 && runes[i-1] == 'з' { // зг → zgh, so it does not read as zh
			b.WriteString("gh")
			prevLetter = true
			continue
		}
		if s, ok := wordInitial[r]; ok && !prevLetter {
			b.WriteString(s)
			prevLetter = true
			continue
		}
		if s, ok := translit[r]; ok {
			b.WriteString(s)
			prevLetter = true
			continue
		}
		for _, d := range norm.NFD.String(string(r)) { // é → e + ◌́
			switch {
			case d < unicode.MaxASCII && (unicode.IsLetter(d) || unicode.IsDigit(d)):
				b.WriteRune(d)
			case unicode.Is(unicode.Mn, d):
			default:
				b.WriteRune('-')
			}
		}
		prevLetter = unicode.IsLetter(r)
	}
	s := strings.Trim(regexp.MustCompile(`-+`).ReplaceAllString(b.String(), "-"), "-")
	if len(s) > 60 {
		s = strings.TrimRight(s[:60], "-")
	}
	return s
}

// declareFromTitle creates a project named by a human title (or a slug): the slug is derived and made
// unique with -2, -3…; the title is kept when it differs from the slug.
func (a *API) declareFromTitle(ctx context.Context, c *Caller, ref string, summary *string) (string, bool, error) {
	title := strings.Join(strings.Fields(ref), " ")
	base := title
	if !slugRe.MatchString(title) {
		base = Slug(title)
	}
	if base == "" {
		return "", false, invalid("project: %q gives no usable slug — pass `name` explicitly", ref)
	}
	for n := 1; n <= 50; n++ {
		slug := base
		if n > 1 {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		var t *string
		if title != slug {
			t = &title
		}
		var ack verbs.Ack
		err := a.inClientRun(ctx, c, func() (err error) {
			_, ack, err = a.V.ProjectDeclare(ctx, c.verbs(), verbs.ProjectDeclareIn{Name: slug, Title: t, Summary: summary})
			return err
		})
		if err != nil {
			return "", false, err
		}
		if !ack.Already {
			return slug, true, nil
		}
		// the slug is taken: by this very title (a repeated call) or by another project
		var same bool
		if err := a.DB.QueryRow(ctx, `SELECT COALESCE(title, name) = $3 FROM projection.project WHERE tenant_id = $1 AND name = $2`,
			c.TenantID, slug, title).Scan(&same); err != nil {
			return "", false, err
		}
		if same {
			return slug, false, nil
		}
	}
	return "", false, invalid("project: no free slug for %q", ref)
}

// ---------------------------------------------------------------------------
// sessions by client
// ---------------------------------------------------------------------------

// SessionRef is $defs/SessionRef of verbs.yaml.
type SessionRef struct {
	SessionID uuid.UUID  `json:"session_id"`
	Project   string     `json:"project"`
	OpenedAt  *time.Time `json:"opened_at,omitempty"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
	Reused    bool       `json:"reused,omitempty"`
}

// Binding says where a write landed (WriteAck of verbs.yaml).
type Binding struct {
	SessionID *uuid.UUID `json:"session_id,omitempty"`
	Inherited bool       `json:"session_inherited,omitempty"`
	Miss      string     `json:"session_miss,omitempty"`
	Warnings  []string   `json:"warnings,omitempty"`
}

// grant identifies the client: the OAuth grant or the LTP_ token (policy.Grant.TokenID).
func (c *Caller) grant() string {
	if c.Grant.TokenID == uuid.Nil {
		return ""
	}
	return c.Grant.TokenID.String()
}

type openSession struct {
	id, run  uuid.UUID
	project  uuid.UUID
	opened   time.Time
	closed   *time.Time
	grant    string
	projName string
}

func (a *API) sessionByID(ctx context.Context, c *Caller, id uuid.UUID) (*openSession, error) {
	var s openSession
	err := a.DB.QueryRow(ctx, `SELECT x.context_id, x.run_id, x.project_id, x.opened_at, x.closed_at, COALESCE(x.client->>'grant', ''), p.name
	   FROM projection.context x JOIN projection.project p ON p.project_id = x.project_id
	  WHERE x.context_id = $1 AND x.owner_id = $2`, id, c.Owner).
		Scan(&s.id, &s.run, &s.project, &s.opened, &s.closed, &s.grant, &s.projName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

// openOfGrant: the open sessions of this client in a project, newest first.
func (a *API) openOfGrant(ctx context.Context, c *Caller, projectID uuid.UUID) ([]openSession, error) {
	g := c.grant()
	if g == "" {
		return nil, nil
	}
	rows, err := a.DB.Query(ctx, `SELECT x.context_id, x.run_id, x.project_id, x.opened_at, p.name
	   FROM projection.context x JOIN projection.project p ON p.project_id = x.project_id
	  WHERE x.project_id = $1 AND x.owner_id = $2 AND x.closed_at IS NULL AND x.client->>'grant' = $3
	  ORDER BY x.opened_at DESC LIMIT 10`, projectID, c.Owner, g)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (openSession, error) {
		s := openSession{grant: g}
		return s, r.Scan(&s.id, &s.run, &s.project, &s.opened, &s.projName)
	})
}

// bind decides the session of a write in a project and sets the caller's run to it:
// an explicit open session of the person → it (another client of the same person may pass its id);
// closed or unknown → session_miss and the automatic rule; automatic: exactly one open session of this
// grant in the project → it, several → none plus a warning, none → none. A write no session takes goes
// to the client's detached run.
func (a *API) bind(ctx context.Context, c *Caller, projectID uuid.UUID, explicit *uuid.UUID) (Binding, error) {
	var b Binding
	if explicit != nil {
		s, err := a.sessionByID(ctx, c, *explicit)
		if err != nil {
			return b, err
		}
		switch {
		case s == nil:
			b.Miss = "not_found"
		case s.closed != nil:
			b.Miss = "closed"
		default:
			c.run, b.SessionID = s.run, &s.id
			return b, nil
		}
	}
	open, err := a.openOfGrant(ctx, c, projectID)
	if err != nil {
		return b, err
	}
	switch len(open) {
	case 0:
	case 1:
		c.run, b.SessionID, b.Inherited = open[0].run, &open[0].id, true
		return b, nil
	default:
		b.Warnings = append(b.Warnings, fmt.Sprintf("%d open sessions of this client in the project: the write is not bound to any — pass session_id", len(open)))
	}
	// unbound: the write still belongs to this client, through its detached run
	c.run, err = a.detachedRun(ctx, c)
	return b, err
}

// inClientRun runs a write that precedes any session (a project declaration) in the client's detached run,
// so it keeps the client's provenance; the caller's run is restored afterwards, so a session opened
// next starts its own run.
func (a *API) inClientRun(ctx context.Context, c *Caller, fn func() error) error {
	if c.run != uuid.Nil {
		return fn()
	}
	run, err := a.detachedRun(ctx, c)
	if err != nil {
		return err
	}
	c.run = run
	defer func() { c.run = uuid.Nil }()
	return fn()
}

// detachedPurpose marks the run that carries a client's writes outside any session.
const detachedPurpose = "detached"

// detachedRun is the run of this client's writes that no session takes (closed, several open, none):
// the newest unfinished detached run of the grant, else a new one. It carries the client info a session's
// run would (grant, provider, surface), so such writes keep their provenance; without it the command
// would fall back to the bootstrap rule run = actor. No grant (a system caller) — no run.
func (a *API) detachedRun(ctx context.Context, c *Caller) (uuid.UUID, error) {
	g := c.grant()
	if g == "" {
		return uuid.Nil, nil
	}
	var id uuid.UUID
	err := a.DB.QueryRow(ctx, `SELECT run_id FROM projection.run
	  WHERE owner_id = $1 AND actor_id = $2 AND finished_at IS NULL AND purpose = $3 AND client->>'grant' = $4
	  ORDER BY started_at DESC LIMIT 1`, c.Owner, c.ActorID, detachedPurpose, g).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	purpose := detachedPurpose
	return a.V.RunStart(ctx, c.verbs(), verbs.RunStartIn{Client: c.clientInfo(nil, ""), Purpose: &purpose})
}

// clientInfo of a session this API opens: what the client declared, plus the grant and the agent role.
func (c *Caller) clientInfo(declared json.RawMessage, agent string) json.RawMessage {
	m := map[string]any{}
	if len(declared) > 0 {
		_ = json.Unmarshal(declared, &m)
	}
	// a surface the client names for itself ("claude.ai-web") is kept as its label, not lost (roast r1)
	if sf, ok := m["surface"].(string); ok && sf != "" {
		if _, has := m["label"]; !has {
			m["label"] = sf
		}
	}
	for _, k := range []string{"grant", "agent", "surface", "app"} { // server-set fields are never taken from the client
		delete(m, k)
	}
	m["surface"] = "api/v1"
	// the app the client said it is (MCP clientInfo or the client header): declarative, decides nothing
	if app := c.ClientLabel(); app != "" {
		m["app"] = app
	}
	if g := c.grant(); g != "" {
		m["grant"] = g
	}
	if agent != "" {
		m["agent"] = agent
	}
	if c.Grant.Vendor != "" {
		m["provider"] = c.Grant.Vendor
	}
	b, _ := json.Marshal(m)
	return b
}

// attach reuses the newest open session of this client in the project or opens one (resume, open).
// A read-only grant gets no session (nil, nil).
func (a *API) attach(ctx context.Context, c *Caller, slug string, projectID uuid.UUID, agent string, declared json.RawMessage) (*SessionRef, error) {
	if c.profile() == policy.ProfileReadonly {
		return nil, nil
	}
	if agent == "" {
		open, err := a.openOfGrant(ctx, c, projectID)
		if err != nil {
			return nil, err
		}
		if len(open) > 0 {
			c.run = open[0].run
			o := open[0].opened
			return &SessionRef{SessionID: open[0].id, Project: open[0].projName, OpenedAt: &o, Reused: true}, nil
		}
	}
	out, err := a.V.ContextOpen(ctx, c.verbs(), verbs.ContextOpenIn{Project: slug, Attach: "none", Client: c.clientInfo(declared, agent)})
	if err != nil {
		return nil, err
	}
	c.run = out.RunID
	now := time.Now().UTC()
	return &SessionRef{SessionID: out.ContextID, Project: slug, OpenedAt: &now}, nil
}

// ---------------------------------------------------------------------------
// agent roles
// ---------------------------------------------------------------------------

var (
	agentRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	modelLike  = regexp.MustCompile(`^(claude|gpt|chatgpt|openai|anthropic|gemini|google|codex|copilot|llama|mistral|mixtral|qwen|deepseek|grok|xai|sonnet|opus|haiku|fable|bard|cursor|o[1-9])([-0-9.]|$)`)
	modelInner = regexp.MustCompile(`(^|-)(gpt|claude|gemini|llama)(-|$)`)
)

// checkAgent: a sub-agent names its role; a model or vendor name is refused with a hint.
func checkAgent(agent string) error {
	if agent == "" {
		return nil
	}
	if !agentRe.MatchString(agent) {
		return &Error{Code: "invalid_input", Message: fmt.Sprintf("agent %q: lower-case letters, digits and '-', at most 63", agent)}
	}
	if modelLike.MatchString(agent) || modelInner.MatchString(agent) {
		return &Error{Code: "invalid_input", Message: fmt.Sprintf("agent %q is a model or vendor name, not a role", agent),
			Hint: `name the sub-agent's role (e.g. "reviewer", "planner"), not the model; the main assistant talking with the user passes no agent`}
	}
	return nil
}
