package query

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/policy"
)

// ---------------------------------------------------------------------------
// projects · facts · tasks · consumers · contexts
// ---------------------------------------------------------------------------

type ProjectRow struct {
	ProjectID      uuid.UUID  `json:"project_id"`
	Name           string     `json:"name"`
	Title          string     `json:"title"` // the slug when none was given
	Summary        *string    `json:"summary,omitempty"`
	Scope          *string    `json:"scope,omitempty"` // 0041
	RepoURL        *string    `json:"repo_url,omitempty"`
	Status         string     `json:"status"`
	Labels         []string   `json:"labels"` // space / global mark the layers; policy labels (0010)
	UpdatedAt      time.Time  `json:"updated_at"`
	StatementCount int        `json:"statement_count"`
	Current        *string    `json:"-"` // context_bundle.current
	Provenance     Provenance `json:"provenance"`
}

const projectCols = `pr.project_id, pr.name, COALESCE(pr.title, pr.name), pr.summary, pr.scope, pr.repo_url, pr.status, pr.labels, pr.updated_at, pr.statement_count, pr.current, `

func (r *ProjectRow) dest() []any {
	return append([]any{&r.ProjectID, &r.Name, &r.Title, &r.Summary, &r.Scope, &r.RepoURL, &r.Status, &r.Labels, &r.UpdatedAt, &r.StatementCount, &r.Current},
		r.Provenance.dest()...)
}

// ProjectList: projects of the tenant, updated_at descending (current contract order).
// Slice 1 has no project owner column: projects are tenant-wide (one human per personal tenant).
func (s *Service) ProjectList(ctx context.Context, p Principal, status string) ([]ProjectRow, error) {
	ctx, me, err := s.enter(ctx, p)
	if err != nil {
		return nil, err
	}
	var a args
	where := "pr.tenant_id = " + a.add(p.TenantID) + " AND pr.owner_id = " + a.add(me)
	if status != "" {
		where += " AND pr.status = " + a.add(status)
	}
	rows, err := s.DB.Query(ctx, `SELECT `+projectCols+provCols("j")+`
		  FROM projection.project pr`+provJoin("j", "pr.tenant_id", "pr.last_seq")+`
		 WHERE `+where+` ORDER BY pr.updated_at DESC, pr.name`, a...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProjectRow, error) {
		var x ProjectRow
		return x, r.Scan(x.dest()...)
	})
}

type FactRow struct {
	Project   string  `json:"project"`
	Kind      string  `json:"kind"`
	Key       string  `json:"key"`
	Value     *string `json:"value,omitempty"`
	SecretRef *string `json:"secret_ref,omitempty"`
	// Source: the record it was taken from (R-N); Stale — that record was superseded or retracted, ReplacedBy — by
	// which: update the fact or drop it
	Source     *string    `json:"source,omitempty"`
	Stale      bool       `json:"stale,omitempty"`
	ReplacedBy *string    `json:"replaced_by,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// LegacyFactKind: the kinds the facade contract 1.19 knows as project_items; binding and the layer kinds of
// the Claude Code plugin (0015) stay out of it.
func LegacyFactKind(kind string) bool {
	switch kind {
	case "architecture", "credential", "access", "note", "tech_stack":
		return true
	}
	return false
}

// FactList: live facts; query matches key or value (case-insensitive substring).
func (s *Service) FactList(ctx context.Context, p Principal, project, kind, query string) ([]FactRow, error) {
	return s.factList(ctx, p, project, kind, query, nil)
}

// FactsAbout: live facts of a project whose key or value holds any of the words (stems of a topic).
func (s *Service) FactsAbout(ctx context.Context, p Principal, project string, words []string) ([]FactRow, error) {
	if len(words) == 0 {
		return []FactRow{}, nil
	}
	return s.factList(ctx, p, project, "", "", words)
}

func (s *Service) factList(ctx context.Context, p Principal, project, kind, query string, words []string) ([]FactRow, error) {
	ctx, me, err := s.enter(ctx, p)
	if err != nil {
		return nil, err
	}
	var a args
	where := []string{"f.tenant_id = " + a.add(p.TenantID), "f.owner_id = " + a.add(me), "NOT f.retired"}
	if project != "" {
		where = append(where, "pr.name = "+a.add(project))
	}
	if kind != "" {
		where = append(where, "f.kind = "+a.add(kind))
	}
	d, _ := policy.From(ctx)
	if query != "" {
		q := a.add("%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(query) + "%")
		if d.RedactsValues() { // a redacted value must not be guessable by matching it
			where = append(where, "f.key ILIKE "+q)
		} else {
			where = append(where, "(f.key ILIKE "+q+" OR f.value ILIKE "+q+")")
		}
	}
	if len(words) > 0 {
		cols := []string{"f.key", "f.value"}
		if d.RedactsValues() {
			cols = cols[:1]
		}
		where = append(where, anyLike(&a, cols, words))
	}
	rows, err := s.DB.Query(ctx, `SELECT pr.name, f.kind, f.key, f.value, f.secret_ref,
		       'R-' || src.num, COALESCE(src.status IN ('superseded', 'retracted'), false), 'R-' || succ.num, `+provCols("j")+`
		  FROM projection.fact f JOIN projection.project pr ON pr.project_id = f.project_id
		  LEFT JOIN projection.statement src ON src.statement_id = f.source_id
		  LEFT JOIN projection.statement succ ON succ.statement_id = src.superseded_by`+
		provJoin("j", "f.tenant_id", "f.last_seq")+`
		 WHERE `+strings.Join(where, " AND ")+` ORDER BY pr.name, f.kind, f.key`, a...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (FactRow, error) {
		var x FactRow
		err := r.Scan(append([]any{&x.Project, &x.Kind, &x.Key, &x.Value, &x.SecretRef, &x.Source, &x.Stale, &x.ReplacedBy}, x.Provenance.dest()...)...)
		x.Value, x.SecretRef, _ = d.RedactFact(x.Kind, x.Value, x.SecretRef)
		return x, err
	})
}

type ConsumerRow struct {
	Consumer  string    `json:"consumer"`
	Seq       int64     `json:"seq"`
	Pending   int64     `json:"pending"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ConsumerListOut struct {
	Items   []ConsumerRow `json:"items"`
	LastSeq int64         `json:"last_seq"`
}

// ConsumerList: named consumers with their debt (pending = last_seq − seq).
func (s *Service) ConsumerList(ctx context.Context, p Principal, consumer string) (ConsumerListOut, error) {
	ctx, me, err := s.enter(ctx, p)
	if err != nil {
		return ConsumerListOut{}, err
	}
	out := ConsumerListOut{Items: []ConsumerRow{}}
	if err := s.DB.QueryRow(ctx, `SELECT last_seq FROM journal_head WHERE tenant_id = $1`, p.TenantID).Scan(&out.LastSeq); err != nil {
		return out, err
	}
	var a args
	where := "tenant_id = " + a.add(p.TenantID) + " AND owner_id = " + a.add(me)
	if consumer != "" {
		where += " AND consumer = " + a.add(consumer)
	}
	rows, err := s.DB.Query(ctx, `SELECT consumer, seq, updated_at FROM projection.consumer WHERE `+where+` ORDER BY consumer`, a...)
	if err != nil {
		return out, err
	}
	out.Items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ConsumerRow, error) {
		var x ConsumerRow
		err := r.Scan(&x.Consumer, &x.Seq, &x.UpdatedAt)
		x.Pending = out.LastSeq - x.Seq
		return x, err
	})
	return out, err
}

type ContextRow struct {
	ContextID      uuid.UUID       `json:"context_id"`
	Project        string          `json:"project"`
	OpenedAt       time.Time       `json:"opened_at"`
	LastEventAt    time.Time       `json:"last_event_at"`
	ClosedAt       *time.Time      `json:"closed_at,omitempty"`
	Summary        *string         `json:"summary,omitempty"`
	Client         json.RawMessage `json:"client,omitempty"`
	HasContent     bool            `json:"has_content"`
	StatementCount int             `json:"statement_count"`
	Provenance     Provenance      `json:"provenance"`
}

const contextCols = `c.context_id, pr.name, c.opened_at, c.last_event_at, c.closed_at, c.summary, c.client, c.has_content, c.statement_count, `

func (r *ContextRow) dest() []any {
	return append([]any{&r.ContextID, &r.Project, &r.OpenedAt, &r.LastEventAt, &r.ClosedAt, &r.Summary, &r.Client, &r.HasContent, &r.StatementCount},
		r.Provenance.dest()...)
}

// ContextList: my OPEN contexts, newest activity first.
func (s *Service) ContextList(ctx context.Context, p Principal, project string) ([]ContextRow, error) {
	ctx, root, err := s.enter(ctx, p)
	if err != nil {
		return nil, err
	}
	var a args
	where := []string{"c.tenant_id = " + a.add(p.TenantID), "c.closed_at IS NULL", mineSQL("c.actor_id", a.add(root))}
	if project != "" {
		where = append(where, "pr.name = "+a.add(project))
	}
	rows, err := s.DB.Query(ctx, `SELECT `+contextCols+provCols("j")+`
		  FROM projection.context c JOIN projection.project pr ON pr.project_id = c.project_id`+
		provJoin("j", "c.tenant_id", "c.opened_seq")+`
		 WHERE `+strings.Join(where, " AND ")+` ORDER BY c.last_event_at DESC`, a...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ContextRow, error) {
		var x ContextRow
		return x, r.Scan(x.dest()...)
	})
}

// ---------------------------------------------------------------------------
// work.open_items
// ---------------------------------------------------------------------------

type OpenItemsOut struct {
	Tasks     []TaskRow     `json:"tasks"`
	Questions []IndexRow    `json:"questions"`
	Consumers []ConsumerRow `json:"consumers"`
	Deletions []DeletionRow `json:"deletions"` // proposals waiting for the person's decision
	// mirrors and handoffs to fix, tasks other projects wait on; nil when there is nothing
	CrossProject *CrossProject `json:"cross_project,omitempty"`
}

// WorkOpenItems: open tasks by priority, current statements of the family question (any vocabulary), consumers in debt, deletion proposals
// waiting for a decision.
func (s *Service) WorkOpenItems(ctx context.Context, p Principal, project string) (OpenItemsOut, error) {
	ctx, _, err := s.enter(ctx, p)
	if err != nil {
		return OpenItemsOut{}, err
	}
	out := OpenItemsOut{Questions: []IndexRow{}, Consumers: []ConsumerRow{}}
	projectID, err := s.projectID(ctx, p, project)
	if err != nil {
		return out, err
	}
	if out.Tasks, err = s.tasks(ctx, p, projectID, true); err != nil {
		return out, err
	}
	q, err := s.Search(ctx, p, SearchIn{Project: project, Family: "question", ExcludeSuperseded: true, Limit: 50})
	if err != nil {
		return out, err
	}
	// an answered question (a live resolves link to it) is not open any more (roast r1)
	resolved, err := s.Resolved(ctx, statementIDs(q.Items))
	if err != nil {
		return out, err
	}
	for _, r := range q.Items {
		if !resolved[r.StatementID] {
			out.Questions = append(out.Questions, r)
		}
	}
	d, err := s.DeletionList(ctx, p, DeletionListIn{Project: project, State: "open"})
	if err != nil {
		return out, err
	}
	out.Deletions = d.Items
	cp, err := s.crossProject(ctx, projectID)
	if err != nil {
		return out, err
	}
	if len(cp.Findings)+len(cp.WaitingOnUs) > 0 {
		out.CrossProject = &cp
	}
	c, err := s.ConsumerList(ctx, p, "")
	if err != nil {
		return out, err
	}
	for _, x := range c.Items {
		if x.Pending > 0 {
			out.Consumers = append(out.Consumers, x)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// journal.changes
// ---------------------------------------------------------------------------

type ChangesIn struct {
	SinceSeq    int64    `json:"since_seq"`
	Limit       int      `json:"limit,omitempty"`
	Types       []string `json:"types,omitempty"`
	Project     string   `json:"project,omitempty"`
	WithPayload *bool    `json:"with_payload,omitempty"` // default true
}

type Change struct {
	Seq           int64           `json:"seq"`
	EventID       uuid.UUID       `json:"event_id"`
	Type          string          `json:"type"`
	SchemaVersion int16           `json:"schema_version"`
	RecordedAt    time.Time       `json:"recorded_at"`
	EffectiveAt   time.Time       `json:"effective_at"`
	ActorID       uuid.UUID       `json:"actor_id"`
	RunID         uuid.UUID       `json:"run_id"`
	CausationID   uuid.UUID       `json:"causation_id"`
	CorrelationID uuid.UUID       `json:"correlation_id"`
	TenantID      uuid.UUID       `json:"tenant_id"`
	EntityType    string          `json:"entity_type"`
	EntityID      uuid.UUID       `json:"entity_id"`
	ContextID     *uuid.UUID      `json:"context_id,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

type ChangesOut struct {
	Events  []Change `json:"events"`
	NextSeq int64    `json:"next_seq"`
	LastSeq int64    `json:"last_seq"`
}

// Changes pages the journal strictly by seq. Filters (types, project, ownership) only drop events,
// the cursor still advances past them: next_seq is the last seq SCANNED, so a filtered consumer
// never re-reads and never skips. journal.purged, journal.quarantined and journal.released arrive as ordinary
// events and pass any types filter (the project filter still applies); the payload of a quarantined entity's events is
// masked until it is released.
func (s *Service) Changes(ctx context.Context, p Principal, in ChangesIn) (ChangesOut, error) {
	out := ChangesOut{Events: []Change{}, NextSeq: in.SinceSeq}
	if in.SinceSeq < 0 {
		return out, fail("invalid_input", "since_seq must be >= 0")
	}
	ctx, root, err := s.enter(ctx, p)
	if err != nil {
		return out, err
	}
	if err := s.DB.QueryRow(ctx, `SELECT last_seq FROM journal_head WHERE tenant_id = $1`, p.TenantID).Scan(&out.LastSeq); err != nil {
		return out, err
	}
	var projectID *uuid.UUID
	if in.Project != "" {
		id, err := s.projectID(ctx, p, in.Project)
		if err != nil {
			return out, err
		}
		projectID = &id
	}
	limit := clamp(in.Limit, 100, 1, 500)
	withPayload := in.WithPayload == nil || *in.WithPayload
	// Scan a window of `limit` seqs, then filter: the cursor contract needs the scanned bound.
	rows, err := s.DB.Query(ctx, `
		WITH w AS (
		  SELECT * FROM journal WHERE tenant_id = $1 AND seq > $2 ORDER BY seq LIMIT $3)
		SELECT w.seq, w.event_id, w.type, w.schema_version, w.recorded_at, w.effective_at, w.actor_id, w.run_id,
		       w.causation_id, w.correlation_id, w.tenant_id, w.entity_type, w.entity_id, w.context_id,
		       CASE WHEN NOT $4 THEN NULL
		            -- quarantine: the content of a hidden entity's events is masked as a purge marker would
		            -- be, structural keys kept, until journal.released; journal.* events carry no content
		            WHEN w.type NOT LIKE 'journal.%' AND CASE w.entity_type
		                   WHEN 'statement' THEN EXISTS (SELECT 1 FROM projection.statement x WHERE x.statement_id = w.entity_id AND x.quarantined)
		                   WHEN 'task'      THEN EXISTS (SELECT 1 FROM projection.task      x WHERE x.task_id      = w.entity_id AND x.quarantined)
		                   ELSE false END
		            THEN jsonb_build_object('purged', true, 'quarantined', true) || jsonb_strip_nulls(jsonb_build_object(
		                   'project_id', w.payload->'project_id', 'type', w.payload->'type', 'num', w.payload->'num'))
		            ELSE w.payload END,
		       (`+mineSQL("w.actor_id", "$5")+`)
		         -- events of erasure always pass a types filter: a consumer that forgot them would keep a secret
		         AND (cardinality($6::text[]) = 0 OR w.type = ANY($6) OR w.type IN ('journal.purged', 'journal.quarantined', 'journal.released'))
		         AND ($7::uuid IS NULL OR COALESCE(w.payload->>'project_id' = $7::text, false) -- NULL would make keep NULL
		              OR (w.entity_type = 'project' AND w.entity_id = $7)
		              OR (w.entity_type = 'statement' AND EXISTS (SELECT 1 FROM projection.statement x WHERE x.statement_id = w.entity_id AND x.project_id = $7))
		              OR (w.entity_type = 'context'   AND EXISTS (SELECT 1 FROM projection.context   x WHERE x.context_id   = w.entity_id AND x.project_id = $7))
		              OR (w.entity_type = 'task'      AND EXISTS (SELECT 1 FROM projection.task      x WHERE x.task_id      = w.entity_id AND x.project_id = $7)))
         -- policy: under a restricted decision an event passes only when its project is visible;
         -- project-scoped events that cannot be placed are dropped, events of no project pass
         AND (litopys_projects() IS NULL
              OR CASE
                   WHEN w.payload ? 'project_id' THEN (w.payload->>'project_id')::uuid = ANY(litopys_projects())
                   WHEN w.entity_type = 'project' THEN w.entity_id = ANY(litopys_projects())
                   WHEN w.entity_type = 'statement' THEN EXISTS (SELECT 1 FROM projection.statement x WHERE x.statement_id = w.entity_id)
                   WHEN w.entity_type = 'context'   THEN EXISTS (SELECT 1 FROM projection.context   x WHERE x.context_id   = w.entity_id)
                   WHEN w.entity_type = 'task'      THEN EXISTS (SELECT 1 FROM projection.task      x WHERE x.task_id      = w.entity_id)
                   WHEN w.entity_type IN ('fact', 'link', 'summary') THEN false
                   ELSE true
                 END)
         -- data class (0023): under a class limit a statement or task event passes only when its row is visible,
         -- so a record raised above the grant's class does not reach it through its events either
         AND (COALESCE(current_setting('litopys.max_class', true), '') = ''
              OR CASE
                   WHEN w.entity_type = 'statement' THEN EXISTS (SELECT 1 FROM projection.statement x WHERE x.statement_id = w.entity_id)
                   WHEN w.entity_type = 'task'      THEN EXISTS (SELECT 1 FROM projection.task      x WHERE x.task_id      = w.entity_id)
                   ELSE true
                 END) AS keep
		  FROM w ORDER BY w.seq`,
		p.TenantID, in.SinceSeq, limit, withPayload, root, nonNil(in.Types), projectID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Change
		var payload []byte
		var keep bool
		if err := rows.Scan(&c.Seq, &c.EventID, &c.Type, &c.SchemaVersion, &c.RecordedAt, &c.EffectiveAt, &c.ActorID, &c.RunID,
			&c.CausationID, &c.CorrelationID, &c.TenantID, &c.EntityType, &c.EntityID, &c.ContextID, &payload, &keep); err != nil {
			return out, err
		}
		out.NextSeq = c.Seq
		if keep {
			c.Payload = payload
			out.Events = append(out.Events, c)
		}
	}
	return out, rows.Err()
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// ---------------------------------------------------------------------------
// system.*
// ---------------------------------------------------------------------------

type WhoamiOut struct {
	Actor       ActorRef  `json:"actor"`
	TenantID    uuid.UUID `json:"tenant_id"`
	Role        string    `json:"role"`
	Email       string    `json:"email,omitempty"`
	DisplayName string    `json:"display_name,omitempty"`
	Kind        string    `json:"kind,omitempty"`
}

func (s *Service) Whoami(ctx context.Context, p Principal) (WhoamiOut, error) {
	ctx, _, err := s.enter(ctx, p)
	if err != nil {
		return WhoamiOut{}, err
	}
	out := WhoamiOut{Actor: ActorRef{ActorID: p.ActorID}, TenantID: p.TenantID, Role: p.Role}
	if out.Role == "" {
		out.Role = "owner"
	}
	var ext *string
	err = s.DB.QueryRow(ctx, `SELECT a.display_name, a.kind, COALESCE(h.external_identity, a.external_identity)
		  FROM projection.actor a LEFT JOIN projection.actor h ON h.actor_id = a.on_behalf_of
		 WHERE a.actor_id = $1 AND a.tenant_id = $2`, p.ActorID, p.TenantID).Scan(&out.DisplayName, &out.Kind, &ext)
	if isNoRows(err) {
		return out, fail("actor_not_registered", "actor %s", p.ActorID)
	}
	if ext != nil && strings.Contains(*ext, "@") {
		out.Email = *ext
	}
	return out, err
}

type CapabilitiesOut struct {
	SpecVersion     int      `json:"spec_version"`
	EnvelopeVersion int      `json:"envelope_version"`
	Slices          []int    `json:"slices"`
	Verbs           []string `json:"verbs"`
	Modules         []string `json:"modules"`
	Facades         []string `json:"facades"`
}

// Verbs implemented by this core (commands in core/verbs, queries here).
var Verbs = []string{
	"actor.amend", "actor.list", "actor.register", "actor.revoke", "consumer.checkpoint", "consumer.list", "context.checkpoint", "context.close",
	"context.list", "context.open", "context.pack", "fact.list", "fact.record", "fact.retire", "journal.changes",
	"journal.purge", "link.assert", "link.retire", "project.amend", "project.declare", "project.list",
	"project.transition", "recall.as_of", "recall.read", "recall.search", "recall.timeline", "run.finish", "run.start",
	"statement.amend", "statement.assert", "statement.reinstate", "statement.retract", "statement.supersede",
	"system.capabilities", "system.guide", "system.health", "system.whoami", "task.amend", "task.close", "task.declare", "task.list", "task.reopen", "task.retract",
	"work.open_items",
}

// Facades is filled by the edge when a facade is mounted (e.g. "memory@1.0").
var Facades = []string{}

func (s *Service) Capabilities() CapabilitiesOut {
	return CapabilitiesOut{SpecVersion: 1, EnvelopeVersion: 1, Slices: []int{1}, Verbs: Verbs, Modules: []string{}, Facades: Facades}
}

type HealthOut struct {
	OK              bool             `json:"ok"`
	LastSeq         int64            `json:"last_seq"`
	ProjectorLagSeq int64            `json:"projector_lag_seq"`
	ConsumersLag    map[string]int64 `json:"consumers_lag"`
	DB              map[string]any   `json:"db"`
}

// Health: the main signal is projector_lag_seq (0 for the synchronous projector; >0 = unfinished
// rebuild or a defect). ProjectorVersion mismatch also means "rebuild needed".
func (s *Service) Health(ctx context.Context, p Principal, projectorVersion string) (HealthOut, error) {
	ctx, _, err := s.enter(ctx, p)
	if err != nil {
		return HealthOut{}, err
	}
	out := HealthOut{ConsumersLag: map[string]int64{}, DB: map[string]any{}}
	var applied int64
	var version *string
	err = s.DB.QueryRow(ctx, `SELECT h.last_seq, COALESCE(ps.last_seq, 0), ps.version
		  FROM journal_head h LEFT JOIN projection.projector_state ps ON ps.tenant_id = h.tenant_id
		 WHERE h.tenant_id = $1`, p.TenantID).Scan(&out.LastSeq, &applied, &version)
	if err != nil {
		return out, err
	}
	out.ProjectorLagSeq = out.LastSeq - applied
	var dbVersion string
	if err := s.DB.QueryRow(ctx, `SHOW server_version`).Scan(&dbVersion); err != nil {
		return out, err
	}
	out.DB["server_version"] = dbVersion
	out.DB["projector_version"] = version
	c, err := s.ConsumerList(ctx, p, "")
	if err != nil {
		return out, err
	}
	for _, x := range c.Items {
		out.ConsumersLag[x.Consumer] = x.Pending
	}
	out.OK = out.ProjectorLagSeq == 0 && (version == nil || *version == projectorVersion)
	return out, nil
}

// anyLike: one of the words is a case-insensitive substring of one of the columns.
func anyLike(a *args, cols, words []string) string {
	esc := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	var or []string
	for _, w := range words {
		q := a.add("%" + esc.Replace(w) + "%")
		for _, c := range cols {
			or = append(or, c+" ILIKE "+q)
		}
	}
	return "(" + strings.Join(or, " OR ") + ")"
}

func statementIDs(rows []IndexRow) []uuid.UUID {
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.StatementID
	}
	return ids
}

// Resolved: which of the statements a live resolves link points to — an answered question, a removed risk.
func (s *Service) Resolved(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	by, err := s.ResolvedBy(ctx, ids)
	out := make(map[uuid.UUID]bool, len(by))
	for id := range by {
		out[id] = true
	}
	return out, err
}

// ResolvedBy: for the statements a live resolves link points to, the refs (R-N) of the records that resolve them.
func (s *Service) ResolvedBy(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]string, error) {
	out := map[uuid.UUID][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT l.to_id, coalesce('R-' || f.num, l.from_type || ':' || l.from_id) FROM projection.link l
	    LEFT JOIN projection.statement f ON l.from_type = 'statement' AND f.statement_id = l.from_id
	  WHERE l.relation = 'resolves' AND NOT l.retired AND l.to_type = 'statement' AND l.to_id = ANY($1::uuid[])
	  ORDER BY 1, 2`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var ref string
		if err := rows.Scan(&id, &ref); err != nil {
			return nil, err
		}
		out[id] = append(out[id], ref)
	}
	return out, rows.Err()
}
