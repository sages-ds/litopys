package query

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/vocab"
)

// IndexRow is statements_current.index_row: compact, no body.
type IndexRow struct {
	StatementID   uuid.UUID  `json:"statement_id"`
	Ref           string     `json:"ref"` // R-<num>
	Project       string     `json:"project"`
	Type          string     `json:"type"`
	Title         string     `json:"title"`
	Status        string     `json:"status"`
	EffectiveAt   time.Time  `json:"effective_at"`
	RecordedAt    time.Time  `json:"recorded_at"`
	Revision      int        `json:"revision"`
	Score         *float64   `json:"score,omitempty"`
	Snippet       *string    `json:"snippet,omitempty"`
	SupersededBy  *uuid.UUID `json:"superseded_by,omitempty"`
	Supersedes    *uuid.UUID `json:"supersedes,omitempty"`
	Confirmations int        `json:"confirmations,omitempty"`
	Files         []string   `json:"files,omitempty"`
	Concepts      []string   `json:"concepts,omitempty"`
	ExternalRef   *string    `json:"external_ref,omitempty"`
	// LegacyType: the old type of an imported record where the migration mapped it to another
	LegacyType *string   `json:"legacy_type,omitempty"`
	AssertedAt time.Time `json:"asserted_at"` // valid time of the first version (the contract's created_at)
	// Authority: "agent" when the statement was asserted in a run of a sub-agent (client.agent),
	// "owner" otherwise. Facade rows ignore it.
	Authority  string     `json:"authority"`
	Provenance Provenance `json:"provenance"`
	// Match (search v2): which words and identifiers of the query this row holds and which it
	// misses — the honest signal of a partial match, readable without the score.
	Match *Match `json:"match,omitempty"`
	// ResolvedBy: refs (R-N) of the records whose live resolves link answers this one — an answered question, a
	// removed risk; status stays current, the answer is a link (roast r2 Р2-2)
	ResolvedBy []string `json:"resolved_by,omitempty"`
}

// Match: coverage "full" when the row holds every word and identifier of the query (a word through its typo
// variant counts), "partial" otherwise; Matched and Missing name them as typed — identifiers too (a canonical
// form instead of the typed one reads as a distortion); Canonical gives the canonical form of an identifier typed otherwise.
type Match struct {
	Coverage  string            `json:"coverage"`
	Matched   []string          `json:"matched,omitempty"`
	Missing   []string          `json:"missing,omitempty"`
	Canonical map[string]string `json:"canonical,omitempty"`
}

// authorityOf: the run that asserted the statement names a sub-agent or not.
const authorityOf = `COALESCE((SELECT CASE WHEN ar.client ? 'agent' THEN 'agent' END FROM journal aj
	JOIN projection.run ar ON ar.run_id = aj.run_id WHERE aj.tenant_id = s.tenant_id AND aj.seq = s.asserted_seq), 'owner')`

// assertedAt: valid time of the first version — "created_at" for clients of the memory_* contract
// (s.effective_at moves with every amendment).
const assertedAt = `(SELECT v.effective_at FROM projection.statement_version v WHERE v.statement_id = s.statement_id AND v.revision = 1)`

const indexCols = `s.statement_id, 'R-' || s.num, p.name, s.type, s.title, s.status, s.effective_at, s.recorded_at, s.revision,
	s.superseded_by, s.supersedes, s.confirmations, s.files, s.concepts, s.external_ref, s.legacy_type, ` + assertedAt + `, ` + authorityOf + `, ` // + provenance cols

func (r *IndexRow) dest() []any {
	return append([]any{&r.StatementID, &r.Ref, &r.Project, &r.Type, &r.Title, &r.Status, &r.EffectiveAt, &r.RecordedAt,
		&r.Revision, &r.SupersededBy, &r.Supersedes, &r.Confirmations, &r.Files, &r.Concepts, &r.ExternalRef, &r.LegacyType, &r.AssertedAt, &r.Authority},
		r.Provenance.dest()...)
}

// ---------------------------------------------------------------------------
// recall.search: search v2 over projection.statement_search (search_v2.go)
// ---------------------------------------------------------------------------

type SearchIn struct {
	Query   string `json:"query,omitempty"`
	Project string `json:"project,omitempty"`
	Type    string `json:"type,omitempty"`
	Family  string `json:"family,omitempty"` // one of the eight families
	// Types, when set, replaces Type: the stored names a read type resolved to (the edge)
	Types []string `json:"-"`
	// Legacy: the old type of the facade's filter — also matches records imported with it (legacy_type, 0040)
	Legacy            string     `json:"-"`
	Concepts          []string   `json:"concepts,omitempty"`
	Files             []string   `json:"files,omitempty"`
	ExternalRef       string     `json:"external_ref,omitempty"`
	DateFrom          *time.Time `json:"date_from,omitempty"`
	DateTo            *time.Time `json:"date_to,omitempty"`
	IncludeSnippet    bool       `json:"include_snippet,omitempty"`
	ExcludeSuperseded bool       `json:"-"` // include_superseded defaults to true
	// ByCreation: date filters, ties and id_before run on the statement's creation time (revision 1) —
	// the "created_at" of the memory_* contract — instead of the valid time of its last amendment.
	ByCreation bool `json:"-"`
	// IncludeNoise (search v2): also legacy hook drafts and todo snapshots, which are out by default.
	IncludeNoise bool       `json:"include_noise,omitempty"`
	Limit        int        `json:"limit,omitempty"`
	IDBefore     *uuid.UUID `json:"id_before,omitempty"`
}

type SearchOut struct {
	Items   []IndexRow `json:"items"`
	Relaxed bool       `json:"relaxed,omitempty"`
	Hidden  []Hidden   `json:"hidden"`
	// NoiseHidden (search v2): matches left out as noise; include_noise shows them.
	NoiseHidden int `json:"noise_hidden,omitempty"`
	// Coverage (search v2): "full" when some row holds the whole query, "partial" when rows hold only part of it
	// (then Relaxed is true, as the old engine marked its OR phase), "none" when nothing matched.
	Coverage string `json:"coverage,omitempty"`
	// UnknownWords (search v2): words of the query no record of the person holds, not even through a typo variant —
	// a sign the answer may not be in memory at all.
	UnknownWords []string `json:"unknown_words,omitempty"`
	// Meaning: what the meaning search did — used (it added or ranked rows), words_only (nothing near enough,
	// the rows are word matches — roast r2 Р2-1: "no_match" beside full word rows read as "not found"), no_match
	// (nothing by words or meaning), model_unavailable (the embedding model did not answer: words only), error;
	// absent when it is off
	Meaning string `json:"meaning,omitempty"`
	// Entity: the task, workflow or execution a number query names; its mentions lead the items.
	Entity *EntityHit `json:"entity,omitempty"`
	// MentionedBy: for a number query, the tasks, workflows and executions that mention the entity — they are
	// not records, so they come beside the items, newest first.
	MentionedBy []EntityHit `json:"mentioned_by,omitempty"`
}

var syntaxRe = regexp.MustCompile(`["]|(^|\s)-\S|\bOR\b`)

func hasQuerySyntax(q string) bool { return syntaxRe.MatchString(q) }
func uniq(v []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range v {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// Search runs search and names, on each row a live resolves link answers, the records that answer it: an answered
// question read as plain "current" in the start pack (roast r2, Р2-2).
func (s *Service) Search(ctx context.Context, p Principal, in SearchIn) (SearchOut, error) {
	out, err := s.search(ctx, p, in)
	if err != nil || len(out.Items) == 0 {
		return out, err
	}
	sctx, _, err := s.enter(ctx, p) // the tenant scope: links are row-level secured
	if err != nil {
		return out, err
	}
	by, err := s.ResolvedBy(sctx, statementIDs(out.Items))
	for i := range out.Items {
		out.Items[i].ResolvedBy = by[out.Items[i].StatementID]
	}
	return out, err
}

func (s *Service) search(ctx context.Context, p Principal, in SearchIn) (SearchOut, error) {
	out := SearchOut{Items: []IndexRow{}, Hidden: []Hidden{}}
	if utf8.RuneCountInString(in.Query) > 2000 {
		return out, fail("invalid_input", "query longer than 2000")
	}
	ctx, root, err := s.enter(ctx, p)
	if err != nil {
		return out, err
	}
	out.Hidden = hiddenOf(ctx)
	var searched bool
	var head []IndexRow
	var number bool
	if in.IDBefore == nil { // the first page only: later pages continue the usual list
		if head, out.Entity, out.MentionedBy, number, err = s.numberHead(ctx, p, root, in); err != nil {
			return out, err
		}
	}
	out.Items, out.NoiseHidden, out.UnknownWords, searched, err = s.searchV2(ctx, p, root, in)
	if out.Items == nil {
		out.Items = []IndexRow{}
	}
	// meaning beside the words: the first page of a query in words, when an embedder is set; a model that
	// does not answer leaves the lexical list as it is
	if err == nil && searched && s.Embedder != nil && in.IDBefore == nil && strings.TrimSpace(in.Query) != "" && !hasQuerySyntax(in.Query) {
		q, eerr := s.Embedder.EmbedQuery(ctx, in.Query)
		switch {
		case eerr != nil:
			out.Meaning = "model_unavailable"
			slog.WarnContext(ctx, "meaning search: the model did not answer", "err", eerr)
		default:
			limit := clamp(in.Limit, 20, 1, 50)
			vec, verr := s.vectorRows(ctx, p, root, in, q, limit)
			switch {
			case verr != nil:
				out.Meaning = "error"
				slog.WarnContext(ctx, "meaning search", "err", verr)
			case len(vec) == 0 && len(out.Items) > 0:
				out.Meaning = "words_only"
			case len(vec) == 0:
				out.Meaning = "no_match"
			default:
				out.Meaning = "used"
				out.Items = fuse(out.Items, vec, limit)
				out.UnknownWords = nil
			}
		}
	}
	if number { // the entity and who mentions it first, then the rest without repeats
		seen := map[uuid.UUID]bool{}
		for _, r := range head {
			seen[r.StatementID] = true
		}
		for _, r := range out.Items {
			if !seen[r.StatementID] {
				head = append(head, r)
			}
		}
		if limit := clamp(in.Limit, 20, 1, 50); len(head) > limit {
			head = head[:limit]
		}
		out.Items, out.UnknownWords = head, nil
	}
	if searched { // a query with something to search: say how well the list covers it
		out.Coverage = "none"
		for _, r := range out.Items {
			if r.Match != nil && r.Match.Coverage == "full" {
				out.Coverage = "full"
				break
			}
			if r.Match != nil && r.Match.Coverage == "semantic" {
				out.Coverage = "semantic"
			} else if out.Coverage != "semantic" {
				out.Coverage = "partial"
			}
		}
		out.Relaxed = out.Coverage == "partial"
	}
	return out, err
}

// typeTerms: the type and family filter of a read on column col (s.type or v.type): types, when the edge resolved
// the given type, else the type as given; family on the statement's family column, or — for a past
// version (v.type), whose family may differ from the current one after an amend — on the type names of the family.
func typeTerms(col, typ string, types []string, legacy, family string, a *args) []string {
	var out []string
	switch {
	case len(types) > 0 && legacy != "":
		// the facade's old name, as vocab.OldNameOf shows it: an imported record by the old type it came with, any
		// other by the stored names of that old type (note must not find config mapped to note)
		lt := strings.TrimSuffix(col, "type") + "legacy_type"
		out = append(out, "("+lt+" = "+a.add(legacy)+" OR ("+lt+" IS NULL AND "+col+" = ANY("+a.add(types)+"::text[])))")
	case len(types) > 0:
		out = append(out, col+" = ANY("+a.add(types)+"::text[])")
	case typ != "":
		out = append(out, col+" = "+a.add(typ))
	}
	switch {
	case family == "":
	case col == "v.type":
		out = append(out, col+" = ANY("+a.add(vocab.TypesOf(family))+"::text[])")
	default:
		out = append(out, "s.family = "+a.add(family))
	}
	return out
}

// searchFilters: the WHERE terms every search engine shares (owner, status, project, type, refs, files, concepts,
// dates, paging) and the recency order that breaks ties.
func searchFilters(in SearchIn, p Principal, root uuid.UUID, a *args) (where []string, tie string) {
	where = []string{"s.tenant_id = " + a.add(p.TenantID), mineSQL("s.actor_id", a.add(root)), "s.status <> 'retracted'", "NOT s.hidden"}
	if in.ExcludeSuperseded {
		where = append(where, "s.status <> 'superseded'")
	}
	if in.Project != "" {
		where = append(where, "p.name = "+a.add(in.Project))
	}
	where = append(where, typeTerms("s.type", in.Type, in.Types, in.Legacy, in.Family, a)...)
	if in.ExternalRef != "" {
		where = append(where, "s.external_ref = "+a.add(in.ExternalRef))
	}
	if len(in.Files) > 0 {
		where = append(where, "s.files && "+a.add(in.Files)+"::text[]")
	}
	if len(in.Concepts) > 0 {
		where = append(where, "s.concepts && "+a.add(in.Concepts)+"::text[]")
	}
	dateCol := "s.effective_at"
	tie = "s.effective_at DESC, s.statement_id DESC"
	if in.ByCreation {
		dateCol, tie = assertedAt, assertedAt+" DESC, s.asserted_seq DESC"
	}
	if in.DateFrom != nil {
		where = append(where, dateCol+" >= "+a.add(*in.DateFrom))
	}
	if in.DateTo != nil {
		where = append(where, dateCol+" <= "+a.add(*in.DateTo))
	}
	if in.IDBefore != nil {
		id := a.add(*in.IDBefore)
		if in.ByCreation {
			where = append(where, "s.asserted_seq < (SELECT asserted_seq FROM projection.statement WHERE statement_id = "+id+")")
		} else {
			where = append(where, "(s.effective_at, s.statement_id) < (SELECT effective_at, statement_id FROM projection.statement WHERE statement_id = "+id+")")
		}
	}
	return where, tie
}

// collectIndex runs a search query whose columns are indexCols + provenance + score + snippet.
func (s *Service) collectIndex(ctx context.Context, sql string, a args) ([]IndexRow, error) {
	rows, err := s.DB.Query(ctx, sql, a...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (IndexRow, error) {
		var x IndexRow
		var score *float32
		err := r.Scan(append(x.dest(), &score, &x.Snippet)...)
		if score != nil {
			f := float64(*score)
			x.Score = &f
		}
		return x, err
	})
}

// ---------------------------------------------------------------------------
// recall.read
// ---------------------------------------------------------------------------

type LinkOut struct {
	Relation string         `json:"relation"`
	To       map[string]any `json:"to"`
}

type Version struct {
	Revision    int        `json:"revision"`
	Title       string     `json:"title"`
	Body        *string    `json:"body,omitempty"`
	Type        string     `json:"type"`
	LegacyType  *string    `json:"legacy_type,omitempty"` // the old type of an imported version (0040)
	Concepts    []string   `json:"concepts,omitempty"`
	Files       []string   `json:"files,omitempty"`
	Refs        []string   `json:"refs,omitempty"` // the list this revision had (0032)
	Status      string     `json:"status"`
	Reason      *string    `json:"reason,omitempty"`
	EffectiveAt time.Time  `json:"effective_at"`
	FromSeq     int64      `json:"from_seq"`
	ToSeq       *int64     `json:"to_seq,omitempty"`
	EventID     uuid.UUID  `json:"event_id"`
	Actor       ActorRef   `json:"actor"`
	RecordedAt  *time.Time `json:"recorded_at,omitempty"`
}

// quarantinedTitle stands for the title of a quarantined record in reads (a purged one has "[purged]" in its row).
const quarantinedTitle = "[quarantined]"

// FullRow is statements_current.full_row.
type FullRow struct {
	IndexRow
	Body      *string    `json:"body,omitempty"`
	ContextID *uuid.UUID `json:"context_id,omitempty"`
	Purged    bool       `json:"purged,omitempty"`
	// Quarantined: the row keeps its ref, project, type, status and links; title, body, concepts, files,
	// external_ref, refs and history are withheld until a human releases it.
	Quarantined bool       `json:"quarantined,omitempty"`
	Tombstone   *Tombstone `json:"tombstone,omitempty"` // a purged or quarantined statement: when, by whom, why
	Refs        []string   `json:"refs,omitempty"`      // as given; internal ones are also links "mentions"
	Retention   *Retention `json:"retention,omitempty"` // of this record (0043)
	// Fields: structured fields of the type (0046)
	Fields  map[string]any `json:"fields,omitempty"`
	Links   []LinkOut      `json:"links"`
	History []Version      `json:"history,omitempty"`
}

// Retention of one record: from retain_until (Europe/Kyiv) on_expiry applies — retract by the worker, purge as a
// deletion proposal waiting for a human's GO.
type Retention struct {
	RetainUntil string `json:"retain_until"`
	OnExpiry    string `json:"on_expiry"`
}

type ReadOut struct {
	Items  []FullRow `json:"items"`
	Hidden []Hidden  `json:"hidden"`
}

// Read returns full rows by id (<= 20). Retracted come with their status (not 404), purged and quarantined with their
// tombstone and without content;
// ids that do not exist or are not visible are simply absent.
func (s *Service) Read(ctx context.Context, p Principal, ids []uuid.UUID, history bool) (ReadOut, error) {
	out := ReadOut{Items: []FullRow{}, Hidden: []Hidden{}}
	if len(ids) < 1 || len(ids) > 20 {
		return out, fail("invalid_input", "ids: 1..20, got %d", len(ids))
	}
	ctx, root, err := s.enter(ctx, p)
	if err != nil {
		return out, err
	}
	out.Hidden = hiddenOf(ctx)
	rows, err := s.DB.Query(ctx, `SELECT `+indexCols+provCols("j")+`, s.body, s.context_id, s.purged, s.quarantined, s.refs,
		       to_char(s.retain_until, 'YYYY-MM-DD'), s.on_expiry, s.fields
		  FROM projection.statement s
		  JOIN projection.project p ON p.project_id = s.project_id`+provJoin("j", "s.tenant_id", "s.last_seq")+`
		 WHERE s.tenant_id = $1 AND s.statement_id = ANY($2) AND `+mineSQL("s.actor_id", "$3")+`
		 ORDER BY array_position($2::uuid[], s.statement_id)`, p.TenantID, ids, root)
	if err != nil {
		return out, err
	}
	out.Items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (FullRow, error) {
		var x FullRow
		var until, onExpiry *string
		err := r.Scan(append(x.IndexRow.dest(), &x.Body, &x.ContextID, &x.Purged, &x.Quarantined, &x.Refs, &until, &onExpiry, &x.Fields)...)
		if until != nil && onExpiry != nil && !x.Purged {
			x.Retention = &Retention{RetainUntil: *until, OnExpiry: *onExpiry}
		}
		x.Links = []LinkOut{}
		if x.Quarantined && !x.Purged {
			x.Title, x.Body, x.Concepts, x.Files, x.ExternalRef, x.Refs, x.Fields = quarantinedTitle, nil, nil, nil, nil, nil, nil
		}
		return x, err
	})
	if err != nil {
		return out, err
	}
	var gone []uuid.UUID
	for _, x := range out.Items {
		if x.Purged || x.Quarantined {
			gone = append(gone, x.StatementID)
		}
	}
	stones, err := s.tombstones(ctx, p.TenantID, "statement", gone)
	if err != nil {
		return out, err
	}
	for i := range out.Items {
		out.Items[i].Tombstone = stones[out.Items[i].StatementID]
		if out.Items[i].Links, err = s.links(ctx, out.Items[i].StatementID); err != nil {
			return out, err
		}
		if history && !out.Items[i].Quarantined {
			if out.Items[i].History, err = s.history(ctx, p, out.Items[i].StatementID); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// links: live edges touching the statement, oriented from it (an incoming edge is reported as
// the relation seen from the other end, e.g. "confirmed_by" for an incoming confirms). The other end
// carries its ref (R-N, <project>/T-, W-, X-N, a project's name) when it has one, and its state when it
// is no longer live — retracted or purged.
func (s *Service) links(ctx context.Context, id uuid.UUID) ([]LinkOut, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT e.rel, e.typ, e.id, `+entityRefSQL("e.typ", "e.id")+`, `+entityStateSQL("e.typ", "e.id")+` FROM (
		  SELECT relation AS rel, to_type AS typ, to_id AS id FROM projection.link WHERE from_id = $1 AND NOT retired
		  UNION ALL
		  SELECT relation || '_by', from_type, from_id FROM projection.link WHERE to_id = $1 AND NOT retired) e
		ORDER BY 1, 3`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LinkOut, error) {
		var rel, typ string
		var to uuid.UUID
		var ref, state *string
		err := r.Scan(&rel, &typ, &to, &ref, &state)
		out := LinkOut{Relation: rel, To: map[string]any{"entity_type": typ, "entity_id": to}}
		if ref != nil {
			out.To["ref"] = *ref
		}
		if state != nil {
			out.To["state"] = *state
		}
		return out, err
	})
}

// entityStateSQL: the state of an entity that is no longer live — 'purged', 'quarantined' or 'retracted' — by its type and id
// columns; NULL for a live one.
func entityStateSQL(typ, id string) string {
	return `CASE ` + typ + `
	  WHEN 'statement' THEN (SELECT CASE WHEN x.purged THEN 'purged' WHEN x.quarantined THEN 'quarantined' WHEN x.status = 'retracted' THEN 'retracted' END
	                           FROM projection.statement x WHERE x.statement_id = ` + id + `)
	  WHEN 'task'      THEN (SELECT CASE WHEN x.purged THEN 'purged' WHEN x.quarantined THEN 'quarantined' WHEN x.retracted THEN 'retracted' END FROM projection.task x WHERE x.task_id = ` + id + `)
	END`
}

// entityRefSQL: the ref of an entity by its type and id columns — R-N, <project>/T-, W-, X-N, a project's
// name; NULL for a kind without a number.
func entityRefSQL(typ, id string) string {
	return `CASE ` + typ + `
	  WHEN 'statement' THEN (SELECT 'R-' || x.num FROM projection.statement x WHERE x.statement_id = ` + id + `)
	  WHEN 'task'      THEN (SELECT p.name || '/T-' || x.num FROM projection.task x JOIN projection.project p ON p.project_id = x.project_id WHERE x.task_id = ` + id + `)
	  WHEN 'workflow'  THEN (SELECT p.name || '/W-' || x.num FROM projection.workflow x JOIN projection.project p ON p.project_id = x.project_id WHERE x.workflow_id = ` + id + `)
	  WHEN 'execution' THEN (SELECT p.name || '/X-' || x.num FROM projection.execution x JOIN projection.project p ON p.project_id = x.project_id WHERE x.execution_id = ` + id + `)
	  WHEN 'project'   THEN (SELECT p.name FROM projection.project p WHERE p.project_id = ` + id + `)
	END`
}

func (s *Service) history(ctx context.Context, p Principal, id uuid.UUID) ([]Version, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT v.revision, v.title, v.body, v.type, v.legacy_type, v.concepts, v.files, v.refs, v.status, v.reason, v.effective_at,
		       v.from_seq, v.to_seq, v.event_id, v.actor_id, j.recorded_at
		  FROM projection.statement_version v
		  JOIN journal j ON j.tenant_id = $2 AND j.seq = v.from_seq
		 WHERE v.statement_id = $1 ORDER BY v.revision`, id, p.TenantID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Version, error) {
		var v Version
		err := r.Scan(&v.Revision, &v.Title, &v.Body, &v.Type, &v.LegacyType, &v.Concepts, &v.Files, &v.Refs, &v.Status, &v.Reason,
			&v.EffectiveAt, &v.FromSeq, &v.ToSeq, &v.EventID, &v.Actor.ActorID, &v.RecordedAt)
		return v, err
	})
}

// ---------------------------------------------------------------------------
// recall.as_of — two time axes
// ---------------------------------------------------------------------------

type AsOfIn struct {
	Project     string     `json:"project"`
	RecordedAt  *time.Time `json:"recorded_at,omitempty"`  // «у що вірили на T»
	EffectiveAt *time.Time `json:"effective_at,omitempty"` // «що було правдою на T, як відомо зараз»
	Type        string     `json:"type,omitempty"`
	Family      string     `json:"family,omitempty"`
	Types       []string   `json:"-"`
	Limit       int        `json:"limit,omitempty"`
}

type AsOfOut struct {
	Items  []IndexRow        `json:"items"`
	Axes   map[string]string `json:"axes"`
	Hidden []Hidden          `json:"hidden"`
}

// AsOf is the slice of statements that were current under one or both time axes.
// Transaction axis: T -> seq_T = max(seq) with recorded_at <= T, then the version valid at seq_T.
func (s *Service) AsOf(ctx context.Context, p Principal, in AsOfIn) (AsOfOut, error) {
	out := AsOfOut{Items: []IndexRow{}, Axes: map[string]string{}, Hidden: []Hidden{}}
	ctx, root, err := s.enter(ctx, p)
	if err != nil {
		return out, err
	}
	out.Hidden = hiddenOf(ctx)
	projectID, err := s.projectID(ctx, p, in.Project)
	if err != nil {
		return out, err
	}
	var a args
	where := []string{"s.project_id = " + a.add(projectID), mineSQL("s.actor_id", a.add(root)), "NOT s.hidden", "v.status = 'current'"}
	// superseded_by as of the point, not as of now: a record current then has no successor yet (roast r1)
	supersededBy := "s.superseded_by"
	if in.RecordedAt != nil {
		var seqT *int64
		if err := s.DB.QueryRow(ctx, `SELECT max(seq) FROM journal WHERE tenant_id = $1 AND recorded_at <= $2`,
			p.TenantID, *in.RecordedAt).Scan(&seqT); err != nil {
			return out, err
		}
		if seqT == nil {
			out.Axes["recorded_at"] = in.RecordedAt.UTC().Format(time.RFC3339Nano)
			return out, nil // nothing was recorded yet
		}
		t := a.add(*seqT)
		where = append(where, "v.from_seq <= "+t, "(v.to_seq IS NULL OR v.to_seq > "+t+")")
		supersededBy = `(SELECT s.superseded_by WHERE EXISTS (SELECT 1 FROM projection.statement_version nv
		        WHERE nv.statement_id = s.superseded_by AND nv.revision = 1 AND nv.from_seq <= ` + t + `))`
		out.Axes["recorded_at"] = in.RecordedAt.UTC().Format(time.RFC3339Nano)
	} else {
		where = append(where, "v.to_seq IS NULL")
	}
	if in.EffectiveAt != nil {
		where = append(where, "v.effective_at <= "+a.add(*in.EffectiveAt))
		out.Axes["effective_at"] = in.EffectiveAt.UTC().Format(time.RFC3339Nano)
	}
	where = append(where, typeTerms("v.type", in.Type, in.Types, "", in.Family, &a)...)
	limit := a.add(clamp(in.Limit, 50, 1, 100))
	rows, err := s.DB.Query(ctx, `
		SELECT s.statement_id, 'R-' || s.num, p.name, v.type, v.title, v.status, v.effective_at, j.recorded_at, v.revision,
		       `+supersededBy+`, s.supersedes, s.confirmations, v.files, v.concepts, s.external_ref, v.legacy_type, `+assertedAt+`, `+authorityOf+`, `+provCols("j")+`
		  FROM projection.statement_version v
		  JOIN projection.statement s ON s.statement_id = v.statement_id
		  JOIN projection.project p ON p.project_id = s.project_id
		  JOIN journal j ON j.tenant_id = s.tenant_id AND j.seq = v.from_seq
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY v.effective_at DESC, s.statement_id DESC LIMIT `+limit, a...)
	if err != nil {
		return out, err
	}
	out.Items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (IndexRow, error) {
		var x IndexRow
		return x, r.Scan(x.dest()...)
	})
	return out, err
}

// ---------------------------------------------------------------------------
// recall.timeline
// ---------------------------------------------------------------------------

type TimelineIn struct {
	Project  string     `json:"project"`
	DateFrom *time.Time `json:"date_from,omitempty"`
	DateTo   *time.Time `json:"date_to,omitempty"`
	Type     string     `json:"type,omitempty"`
	Family   string     `json:"family,omitempty"`
	Types    []string   `json:"-"`
	Limit    int        `json:"limit,omitempty"`
}

type TimelineItem struct {
	Row   IndexRow  `json:"row"`
	Links []LinkOut `json:"links"`
}

type TimelineOut struct {
	Items  []TimelineItem `json:"items"`
	Hidden []Hidden       `json:"hidden"`
}

// Timeline is the ordered strip of a project's statements (effective_at ascending) with explicit
// edges: supersedes / superseded_by and the live links.
func (s *Service) Timeline(ctx context.Context, p Principal, in TimelineIn) (TimelineOut, error) {
	out := TimelineOut{Items: []TimelineItem{}, Hidden: []Hidden{}}
	ctx, root, err := s.enter(ctx, p)
	if err != nil {
		return out, err
	}
	out.Hidden = hiddenOf(ctx)
	if _, err := s.projectID(ctx, p, in.Project); err != nil {
		return out, err
	}
	var a args
	where := []string{"s.tenant_id = " + a.add(p.TenantID), "p.name = " + a.add(in.Project), "s.status <> 'retracted'", "NOT s.hidden",
		mineSQL("s.actor_id", a.add(root))}
	if in.DateFrom != nil {
		where = append(where, "s.effective_at >= "+a.add(*in.DateFrom))
	}
	if in.DateTo != nil {
		where = append(where, "s.effective_at <= "+a.add(*in.DateTo))
	}
	where = append(where, typeTerms("s.type", in.Type, in.Types, "", in.Family, &a)...)
	limit := a.add(clamp(in.Limit, 50, 1, 200))
	rows, err := s.DB.Query(ctx, `SELECT `+indexCols+provCols("j")+`
		  FROM projection.statement s
		  JOIN projection.project p ON p.project_id = s.project_id`+provJoin("j", "s.tenant_id", "s.last_seq")+`
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY s.effective_at, s.statement_id LIMIT `+limit, a...)
	if err != nil {
		return out, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (IndexRow, error) {
		var x IndexRow
		return x, r.Scan(x.dest()...)
	})
	if err != nil {
		return out, err
	}
	for _, row := range list {
		links, err := s.links(ctx, row.StatementID)
		if err != nil {
			return out, err
		}
		if row.Supersedes != nil {
			links = append(links, LinkOut{Relation: "supersedes", To: map[string]any{"entity_type": "statement", "entity_id": *row.Supersedes}})
		}
		if row.SupersededBy != nil {
			links = append(links, LinkOut{Relation: "superseded_by", To: map[string]any{"entity_type": "statement", "entity_id": *row.SupersededBy}})
		}
		out.Items = append(out.Items, TimelineItem{Row: row, Links: links})
	}
	return out, nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
