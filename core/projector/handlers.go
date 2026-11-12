package projector

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/journal"
	"litopys.smart-gw.co.ua/core/textnorm"
	"litopys.smart-gw.co.ua/core/vocab"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func decode(e *journal.Event, v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	return nil
}

// effective is the valid time of the event: the stored effective_at, or recorded_at when the
// live path passed none (journal_append defaults it to recorded_at).
func effective(e *journal.Event) time.Time {
	if e.EffectiveAt != nil {
		return *e.EffectiveAt
	}
	return e.RecordedAt
}

// workClock is the time an event moves the work clock (project updated_at, context last_event_at): its
// effective time, but never later than when it was recorded — a payment dated next week is not work done
// next week, and the project must not look updated in the future (roast r1).
func workClock(e *journal.Event) time.Time {
	if t := effective(e); t.Before(e.RecordedAt) {
		return t
	}
	return e.RecordedAt
}

// jsonb turns a raw JSON value into a jsonb parameter; absent -> NULL.
func jsonb(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return string(raw)
}

func strs(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// exec1 runs a statement that must touch exactly one row.
func exec1(ctx context.Context, tx pgx.Tx, what string, sql string, args ...any) error {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: %s (%d rows)", ErrMissing, what, tag.RowsAffected())
	}
	return nil
}

// touchProject records that an event touched the project (updated_at / last_seq).
func touchProject(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, e *journal.Event) error {
	return exec1(ctx, tx, "project "+projectID.String(),
		`UPDATE projection.project SET updated_at = GREATEST(updated_at, $2), last_seq = $3 WHERE project_id = $1`,
		projectID, workClock(e), e.Seq)
}

// ---------------------------------------------------------------------------
// actors · runs · contexts
// ---------------------------------------------------------------------------

func actorRegistered(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Kind             string          `json:"kind"`
		DisplayName      string          `json:"display_name"`
		OnBehalfOf       *uuid.UUID      `json:"on_behalf_of"`
		Client           json.RawMessage `json:"client"`
		ExternalIdentity *string         `json:"external_identity"`
		Executor         json.RawMessage `json:"executor"` // 0024
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO projection.actor (actor_id, tenant_id, kind, display_name, on_behalf_of, external_identity, client, registered_seq, owner_id, executor)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,COALESCE($5::uuid,$1::uuid),$9)`,
		e.EntityID, e.TenantID, p.Kind, p.DisplayName, p.OnBehalfOf, p.ExternalIdentity, jsonb(p.Client), e.Seq, jsonb(p.Executor))
	return err
}

// actorAmended: a new name and / or the whole executor profile (0024); "executor": null removes the profile.
func actorAmended(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p map[string]json.RawMessage
	if err := decode(e, &p); err != nil {
		return err
	}
	var name *string
	if raw, ok := p["display_name"]; ok {
		if err := json.Unmarshal(raw, &name); err != nil {
			return fmt.Errorf("payload display_name: %w", err)
		}
	}
	raw, setExec := p["executor"]
	return exec1(ctx, tx, "actor "+e.EntityID.String(), `
		UPDATE projection.actor SET display_name = COALESCE($2, display_name),
		       executor = CASE WHEN $3 THEN $4::jsonb ELSE executor END
		 WHERE actor_id = $1`, e.EntityID, name, setExec, jsonb(raw))
}

func actorRevoked(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	return exec1(ctx, tx, "actor "+e.EntityID.String(),
		`UPDATE projection.actor SET revoked_at = $2 WHERE actor_id = $1`, e.EntityID, effective(e))
}

func runStarted(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		ParentRunID *uuid.UUID      `json:"parent_run_id"`
		Client      json.RawMessage `json:"client"`
		Purpose     *string         `json:"purpose"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO projection.run (run_id, tenant_id, actor_id, parent_run_id, correlation_id, client, purpose, started_at, started_seq, owner_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,litopys_root($3))`,
		e.EntityID, e.TenantID, e.ActorID, p.ParentRunID, e.CorrelationID, jsonb(p.Client), p.Purpose, effective(e), e.Seq)
	return err
}

func runFinished(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Outcome string `json:"outcome"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	return exec1(ctx, tx, "run "+e.EntityID.String(),
		`UPDATE projection.run SET finished_at = $2, outcome = $3 WHERE run_id = $1`, e.EntityID, effective(e), p.Outcome)
}

func contextOpened(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		ProjectID  uuid.UUID  `json:"project_id"`
		AttachedTo *uuid.UUID `json:"attached_to"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	// client is a property of the run the context was opened in (ClientInfo lives on run.started).
	if _, err := tx.Exec(ctx, `
		INSERT INTO projection.context (context_id, tenant_id, project_id, actor_id, run_id, attached_to, client,
		                                opened_at, last_event_at, opened_seq, owner_id)
		VALUES ($1,$2,$3,$4,$5,$6,(SELECT client FROM projection.run WHERE run_id = $5),$7,$7,$8,litopys_root($4))`,
		e.EntityID, e.TenantID, p.ProjectID, e.ActorID, e.RunID, p.AttachedTo, effective(e), e.Seq); err != nil {
		return err
	}
	// Opening a session is not work on the project: hook starts must not move updated_at (the
	// current server's last_work_at excludes them). Only last_seq follows the event.
	return exec1(ctx, tx, "project "+p.ProjectID.String(),
		`UPDATE projection.project SET last_seq = $2 WHERE project_id = $1`, p.ProjectID, e.Seq)
}

func contextClosed(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Summary   *string  `json:"summary"`
		Auto      bool     `json:"auto"`
		NextSteps []string `json:"next_steps"` // 0029
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	var next []string
	if len(p.NextSteps) > 0 {
		next = p.NextSteps
	}
	return exec1(ctx, tx, "context "+e.EntityID.String(), `
		UPDATE projection.context
		   SET closed_at = $2, closed_auto = $3, summary = COALESCE($4, summary), next_steps = $5, last_event_at = GREATEST(last_event_at, $2)
		 WHERE context_id = $1`, e.EntityID, effective(e), p.Auto, p.Summary, next)
}

func contextCheckpointed(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Summary string  `json:"summary"`
		Current *string `json:"current"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	var projectID uuid.UUID
	err := tx.QueryRow(ctx, `
		UPDATE projection.context
		   SET summary = $2, current = COALESCE($3, current), checkpoint_count = checkpoint_count + 1, last_event_at = GREATEST(last_event_at, $4)
		 WHERE context_id = $1
		RETURNING project_id`, e.EntityID, p.Summary, p.Current, workClock(e)).Scan(&projectID)
	if err != nil {
		return fmt.Errorf("context %s: %w", e.EntityID, err)
	}
	return exec1(ctx, tx, "project "+projectID.String(), `
		UPDATE projection.project SET current = COALESCE($2, current), updated_at = GREATEST(updated_at, $3), last_seq = $4
		 WHERE project_id = $1`, projectID, p.Current, workClock(e), e.Seq)
}

// ---------------------------------------------------------------------------
// statements
// ---------------------------------------------------------------------------

// vocabTSV: the lexemes search_vocab is harvested from (current server's search_tsv).
const vocabTSV = `to_tsvector('simple', coalesce(s.title, '') || ' ' || coalesce(s.body, ''))`

// lexical rebuilds the R2 lexical row from the current statement:
// title + concepts weight A, body weight B, 'simple' configuration.
func lexical(ctx context.Context, tx pgx.Tx, statementID uuid.UUID, seq int64) error {
	// a purged or quarantined record has no place in search; journal.released calls lexical again
	var hidden bool
	if err := tx.QueryRow(ctx, `SELECT hidden FROM projection.statement WHERE statement_id = $1`, statementID).Scan(&hidden); err != nil {
		return err
	}
	if hidden {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO projection.statement_lexical (statement_id, tsv, last_seq, owner_id)
		SELECT statement_id,
		       setweight(to_tsvector('simple', title || ' ' || array_to_string(concepts, ' ')), 'A')
		       || setweight(to_tsvector('simple', coalesce(body, '')), 'B'),
		       $2, owner_id
		  FROM projection.statement WHERE statement_id = $1
		ON CONFLICT (statement_id) DO UPDATE SET tsv = EXCLUDED.tsv, last_seq = EXCLUDED.last_seq`,
		statementID, seq)
	if err != nil {
		return err
	}
	// search_vocab: lexemes of the corpus for word-form expansion (current server: 011 trigger).
	// Harvested from title + body only, exactly as the current server does: concept lexemes would
	// crowd the top-3 forms (E17: "міграція-пулу" displaced "міграції"). Words of earlier revisions
	// stay, as today; purge recomputes the dictionary (journalPurged).
	_, err = tx.Exec(ctx, `
		INSERT INTO projection.search_vocab (tenant_id, owner_id, word)
		SELECT s.tenant_id, s.owner_id, t.lexeme
		  FROM projection.statement s, unnest(`+vocabTSV+`) t
		 WHERE s.statement_id = $1
		ON CONFLICT DO NOTHING`, statementID)
	if err != nil {
		return err
	}
	return searchRow(ctx, tx, statementID, seq)
}

// searchRow writes the search projection v2 of a statement (0025): core/textnorm over title +
// concepts + body — canonical identifiers and uk/en stems — and the same of the title alone (0026), which search
// v2 uses to break ties. The same Analyze runs on queries.
func searchRow(ctx context.Context, tx pgx.Tx, statementID uuid.UUID, seq int64) error {
	var title, body string
	var concepts []string
	if err := tx.QueryRow(ctx, `SELECT coalesce(title, ''), coalesce(body, ''), coalesce(concepts, '{}')
		  FROM projection.statement WHERE statement_id = $1`, statementID).Scan(&title, &body, &concepts); err != nil {
		return err
	}
	d := textnorm.Analyze(title + "\n" + strings.Join(concepts, " ") + "\n" + body)
	if d.Idents == nil {
		d.Idents, d.IdentTF = []string{}, []int{} // a nil slice goes to PostgreSQL as NULL, and idents is NOT NULL
	}
	t := textnorm.Analyze(title)
	if t.Idents == nil {
		t.Idents = []string{}
	}
	stems := uniqueStems(d.Stems)
	if _, err := tx.Exec(ctx, `
		INSERT INTO projection.statement_search (statement_id, tenant_id, owner_id, idents, ident_tf, stems, title_idents, title_stems, norm_version, last_seq)
		SELECT statement_id, tenant_id, owner_id, $2, $3, array_to_tsvector($4::text[]), $5, $6, $7, $8
		  FROM projection.statement WHERE statement_id = $1
		ON CONFLICT (statement_id) DO UPDATE SET idents = EXCLUDED.idents, ident_tf = EXCLUDED.ident_tf, stems = EXCLUDED.stems,
		       title_idents = EXCLUDED.title_idents, title_stems = EXCLUDED.title_stems,
		       norm_version = EXCLUDED.norm_version, last_seq = EXCLUDED.last_seq`,
		statementID, d.Idents, d.IdentTF, stems, t.Idents, uniqueStems(t.Stems), textnorm.Version, seq); err != nil {
		return err
	}
	// stem_vocab (0028): the owner's dictionary of stems for the typo channel; a superset, as search_vocab
	_, err := tx.Exec(ctx, `
		INSERT INTO projection.stem_vocab (tenant_id, owner_id, stem)
		SELECT s.tenant_id, s.owner_id, t FROM projection.statement s, unnest($2::text[]) t
		 WHERE s.statement_id = $1
		ON CONFLICT DO NOTHING`, statementID, stems)
	return err
}

// uniqueStems: array_to_tsvector refuses duplicates and empty strings.
func uniqueStems(stems []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range stems {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func statementAsserted(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		ProjectID   uuid.UUID `json:"project_id"`
		Type        string    `json:"type"`
		Title       string    `json:"title"`
		Body        *string   `json:"body"`
		Concepts    []string  `json:"concepts"`
		Files       []string  `json:"files"`
		ExternalRef *string   `json:"external_ref"`
		Part        *struct {
			Index int `json:"index"`
			Total int `json:"total"`
		} `json:"part"`
		Purged    bool            `json:"purged"`     // replay of a purged statement: marker with structural keys only (0004)
		DataClass *string         `json:"data_class"` // 0023: raises the project's class; absent = the project's
		Outcome   json.RawMessage `json:"outcome"`    // 0029: the structure of a type=outcome statement
		Num       *int64          `json:"num"`        // 0030: R-<num> within the owner
		Refs      []string        `json:"refs"`       // 0031: as given; internal ones are also mentions links
		Family    *string         `json:"family"`     // 0038: absent in older events — the legacy map
		// 0040: the old type of an imported record, where the map of the migration changed it
		ImportedFrom *struct {
			Type *string `json:"type"`
		} `json:"imported_from"`
		// 0043: the retention of this record: both or neither
		RetainUntil *string `json:"retain_until"`
		OnExpiry    *string `json:"on_expiry"`
		// 0046: structured fields of the type
		Fields json.RawMessage `json:"fields"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	if p.Purged {
		p.Title = purgedTitle
	}
	family, err := familyOf(p.Type, p.Family)
	if err != nil {
		return err
	}
	num, err := recordNum(ctx, tx, e.EntityID, p.ProjectID, p.Num)
	if err != nil {
		return err
	}
	var partIndex, partTotal *int
	if p.Part != nil {
		partIndex, partTotal = &p.Part.Index, &p.Part.Total
	}
	eff := effective(e)
	var legacyType *string
	if p.ImportedFrom != nil {
		legacyType = p.ImportedFrom.Type
	}
	var outcome *string // jsonb; a purged replay carries none
	if len(p.Outcome) > 0 && string(p.Outcome) != "null" {
		o := string(p.Outcome)
		outcome = &o
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO projection.statement (statement_id, tenant_id, project_id, type, title, body, concepts, files,
		       external_ref, part_index, part_total, context_id, actor_id, effective_at, recorded_at,
		       asserted_event_id, asserted_seq, last_event_id, last_seq, purged, owner_id, data_class, outcome, num, refs, family, legacy_type,
		       retain_until, on_expiry, retain_by, retain_seq, fields)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$16,$17,$18,
		        (SELECT owner_id FROM projection.project WHERE project_id = $3), $19, $20::jsonb, $21, $22, $23, $24,
		        $25::date, $26, CASE WHEN $25::date IS NOT NULL THEN $13::uuid END, CASE WHEN $25::date IS NOT NULL THEN $17::bigint END, $27::jsonb)`,
		e.EntityID, e.TenantID, p.ProjectID, p.Type, p.Title, p.Body, strs(p.Concepts), strs(p.Files),
		p.ExternalRef, partIndex, partTotal, e.ContextID, e.ActorID, eff, e.RecordedAt, e.EventID, e.Seq, p.Purged, p.DataClass, outcome, num, strs(p.Refs),
		family, legacyType, retainDate(p.RetainUntil, p.OnExpiry), retainAction(p.RetainUntil, p.OnExpiry), fieldsOf(p.Fields, p.Purged)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO projection.statement_version (statement_id, revision, title, body, type, concepts, files, status,
		       effective_at, from_seq, event_id, actor_id, owner_id, refs, legacy_type, fields)
		VALUES ($1, 1, $2, $3, $4, $5, $6, 'current', $7, $8, $9, $10,
		        (SELECT owner_id FROM projection.statement WHERE statement_id = $1), $11, $12, $13::jsonb)`,
		e.EntityID, p.Title, p.Body, p.Type, strs(p.Concepts), strs(p.Files), eff, e.Seq, e.EventID, e.ActorID, strs(p.Refs), legacyType,
		fieldsOf(p.Fields, p.Purged)); err != nil {
		return err
	}
	// a draft of the raw-event stream (the service actor system:raw-events) is no work on the project: the current
	// server's worker writes it without touching projects.updated_at. Only that writer — other system:* writers
	// (system:session-state of the facade) go through tools that move the clock there too.
	if err := exec1(ctx, tx, "project "+p.ProjectID.String(), `
		UPDATE projection.project SET statement_count = statement_count + 1, last_seq = $3,
		       updated_at = CASE WHEN EXISTS (SELECT 1 FROM projection.actor WHERE actor_id = $4 AND kind = 'service'
		                                        AND external_identity = 'system:raw-events')
		                         THEN updated_at ELSE GREATEST(updated_at, $2) END
		 WHERE project_id = $1`, p.ProjectID, workClock(e), e.Seq, e.ActorID); err != nil {
		return err
	}
	if e.ContextID != nil {
		if err := exec1(ctx, tx, "context "+e.ContextID.String(), `
			UPDATE projection.context SET statement_count = statement_count + 1, last_event_at = GREATEST(last_event_at, $2)
			 WHERE context_id = $1`, *e.ContextID, workClock(e)); err != nil {
			return err
		}
	}
	return lexical(ctx, tx, e.EntityID, e.Seq)
}

// newVersion closes the open version of the statement at e.Seq and opens revision+1 from the
// statement row as it is NOW (callers update the row first). Returns the project id.
// work: the change is work on the project and moves its updated_at (the work clock).
func newVersion(ctx context.Context, tx pgx.Tx, e *journal.Event, reason *string, work bool) (uuid.UUID, error) {
	var projectID uuid.UUID
	var revision int
	err := tx.QueryRow(ctx, `
		UPDATE projection.statement SET revision = revision + 1, last_event_id = $2, last_seq = $3
		 WHERE statement_id = $1
		RETURNING project_id, revision`, e.EntityID, e.EventID, e.Seq).Scan(&projectID, &revision)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: statement %s: %v", ErrMissing, e.EntityID, err)
	}
	if err := exec1(ctx, tx, "open version of "+e.EntityID.String(), `
		UPDATE projection.statement_version SET to_seq = $2
		 WHERE statement_id = $1 AND to_seq IS NULL`, e.EntityID, e.Seq); err != nil {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO projection.statement_version (statement_id, revision, title, body, type, concepts, files, status,
		       reason, effective_at, from_seq, event_id, actor_id, owner_id, refs, legacy_type, fields)
		SELECT statement_id, revision, title, body, type, concepts, files, status, $2, $3, $4, $5, $6, owner_id, refs, legacy_type, fields
		  FROM projection.statement WHERE statement_id = $1`,
		e.EntityID, reason, effective(e), e.Seq, e.EventID, e.ActorID)
	if err != nil {
		return uuid.Nil, err
	}
	// The work clock (decision 2026-10-06 — a deliberate departure from the current
	// server, which counts creations only): an amend is work; retract, reinstate and supersede are
	// housekeeping and move last_seq only. The superseding statement moves the clock by its own event.
	if work {
		return projectID, touchProject(ctx, tx, projectID, e)
	}
	return projectID, exec1(ctx, tx, "project "+projectID.String(),
		`UPDATE projection.project SET last_seq = $2 WHERE project_id = $1`, projectID, e.Seq)
}

func statementAmended(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Title     *string  `json:"title"`
		Body      *string  `json:"body"`
		Type      *string  `json:"type"`
		Concepts  []string `json:"concepts"`
		Files     []string `json:"files"`
		Reason    *string  `json:"reason"`
		DataClass *string  `json:"data_class"` // 0023
		Refs      []string `json:"refs"`       // 0031: the new list replaces the old one
		Family    *string  `json:"family"`     // 0038: with a new type; absent in older events — the legacy map
		// 0040: the old type of an imported version; a new type without it clears the column
		LegacyType *string `json:"legacy_type"`
		// 0046: the whole new set of fields; {} clears them
		Fields json.RawMessage `json:"fields"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	var family *string
	if p.Type != nil {
		f, err := familyOf(*p.Type, p.Family)
		if err != nil {
			return err
		}
		family = &f
	}
	if err := exec1(ctx, tx, "statement "+e.EntityID.String(), `
		UPDATE projection.statement
		   SET title = COALESCE($2, title), body = COALESCE($3, body), type = COALESCE($4, type),
		       concepts = COALESCE($5, concepts), files = COALESCE($6, files), effective_at = $7,
		       data_class = COALESCE($8, data_class), refs = COALESCE($9, refs), family = COALESCE($10, family),
		       legacy_type = CASE WHEN $4::text IS NULL THEN legacy_type ELSE $11 END,
		       fields = CASE WHEN $12::boolean THEN NULLIF($13::jsonb, '{}'::jsonb) ELSE fields END
		 WHERE statement_id = $1`,
		e.EntityID, p.Title, p.Body, p.Type, p.Concepts, p.Files, effective(e), p.DataClass, p.Refs, family, p.LegacyType,
		len(p.Fields) > 0 && string(p.Fields) != "null", fieldsOf(p.Fields, false)); err != nil {
		return err
	}
	if _, err := newVersion(ctx, tx, e, p.Reason, true); err != nil {
		return err
	}
	return lexical(ctx, tx, e.EntityID, e.Seq)
}

// fieldsOf: the jsonb of a record's fields; none, null or a purged replay — NULL.
func fieldsOf(raw json.RawMessage, purged bool) *string {
	if purged || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	s := string(raw)
	return &s
}

// familyOf: the family an event carries (0038), or for an older event the family of its type by the fixed legacy map
// and the catalog — a type never changes its family, so replay gives the same result.
func familyOf(typ string, given *string) (string, error) {
	if given != nil {
		return *given, nil
	}
	if f, ok := vocab.FamilyOf(typ); ok {
		return f, nil
	}
	return "", fmt.Errorf("statement type %q has no family", typ)
}

func setStatus(ctx context.Context, tx pgx.Tx, e *journal.Event, status string, reason *string) error {
	if err := exec1(ctx, tx, "statement "+e.EntityID.String(),
		`UPDATE projection.statement SET status = $2 WHERE statement_id = $1`, e.EntityID, status); err != nil {
		return err
	}
	_, err := newVersion(ctx, tx, e, reason, false)
	return err
}

type reasonOnly struct {
	Reason *string `json:"reason"`
}

// retainDate and retainAction: a retention is whole or absent (a purged replay carries neither).
func retainDate(until, action *string) *string {
	if until == nil || *until == "" || action == nil {
		return nil
	}
	return until
}

func retainAction(until, action *string) *string {
	if retainDate(until, action) == nil {
		return nil
	}
	return action
}

// statementRetentionSet (0043): the retention of one record set, replaced or lifted (retain_until "").
// Not a new version of the content and not work on the project: last_seq moves, the revision does not.
func statementRetentionSet(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		RetainUntil *string `json:"retain_until"`
		OnExpiry    *string `json:"on_expiry"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	return exec1(ctx, tx, "statement "+e.EntityID.String(), `
		UPDATE projection.statement
		   SET retain_until = $2::date, on_expiry = $3,
		       retain_by = CASE WHEN $2::date IS NOT NULL THEN $4::uuid END,
		       retain_seq = CASE WHEN $2::date IS NOT NULL THEN $5::bigint END,
		       last_event_id = $6, last_seq = $5
		 WHERE statement_id = $1`, e.EntityID, retainDate(p.RetainUntil, p.OnExpiry), retainAction(p.RetainUntil, p.OnExpiry),
		e.ActorID, e.Seq, e.EventID)
}

func statementRetracted(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p reasonOnly
	if err := decode(e, &p); err != nil {
		return err
	}
	return setStatus(ctx, tx, e, "retracted", p.Reason)
}

func statementReinstated(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p reasonOnly
	if err := decode(e, &p); err != nil {
		return err
	}
	return setStatus(ctx, tx, e, "current", p.Reason)
}

// statementSuperseded is an event on the OLD statement. The new one is asserted by the same
// command BEFORE this event, so its row must exist: it gets supersedes = old.
func statementSuperseded(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		SupersededBy uuid.UUID `json:"superseded_by"`
		Reason       *string   `json:"reason"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	if err := exec1(ctx, tx, "statement "+e.EntityID.String(),
		`UPDATE projection.statement SET status = 'superseded', superseded_by = $2 WHERE statement_id = $1`,
		e.EntityID, p.SupersededBy); err != nil {
		return err
	}
	if _, err := newVersion(ctx, tx, e, p.Reason, false); err != nil {
		return err
	}
	return exec1(ctx, tx, "superseding statement "+p.SupersededBy.String(),
		`UPDATE projection.statement SET supersedes = $2 WHERE statement_id = $1`, p.SupersededBy, e.EntityID)
}

const purgedTitle = "[purged]"

// journalPurged erases content from the projections (invariant 1: the purge itself stays in history).
// Statements and tasks are erased here; the verb refuses other targets (verbs.purgeable).
//
// E7 contract: after this handler the rows must equal what replay builds from the purged markers
// (0004 keeps project_id, type, superseded_by, part). So everything the markers drop is cleared
// here too: title/body/concepts/files, external_ref and version reasons.
func journalPurged(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Target struct {
			EntityType string    `json:"entity_type"`
			EntityID   uuid.UUID `json:"entity_id"`
		} `json:"target"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	switch p.Target.EntityType {
	case "statement":
	case "task":
		return taskPurged(ctx, tx, e, p.Target.EntityID)
	default:
		return nil
	}
	id := p.Target.EntityID
	if err := exec1(ctx, tx, "statement "+id.String(), `
		UPDATE projection.statement
		   SET purged = true, title = $4, body = NULL, concepts = '{}', files = '{}', external_ref = NULL, outcome = NULL, refs = '{}',
		       retain_until = NULL, on_expiry = NULL, retain_by = NULL, retain_seq = NULL,
		       last_event_id = $2, last_seq = $3
		 WHERE statement_id = $1`, id, e.EventID, e.Seq, purgedTitle); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE projection.statement_version SET title = $2, body = NULL, concepts = '{}', files = '{}', reason = NULL, refs = '{}'
		 WHERE statement_id = $1`, id, purgedTitle); err != nil {
		return err
	}
	return unsearchable(ctx, tx, e.TenantID, id)
}

// unsearchable takes a purged or quarantined record out of search: its lexical and search rows, and the words and
// stems only it held.
func unsearchable(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM projection.statement_lexical WHERE statement_id = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM projection.statement_search WHERE statement_id = $1`, id); err != nil {
		return err
	}
	// Words of the hidden content must not survive in the dictionary: keep only lexemes still present
	// in some visible record of the owner. Purge and quarantine are rare, so the whole dictionary is recomputed;
	// the same step runs on replay, which keeps live == rebuilt (E7).
	// NOT IN over an uncorrelated subquery: a hashed subplan, the owner's lexemes computed once. A correlated
	// NOT EXISTS let the planner, without statistics right after a rebuild's TRUNCATE, recompute to_tsvector of
	// every statement for every dictionary word — over 10 min on the pilot. Lexemes are never NULL.
	if _, err := tx.Exec(ctx, `
		WITH o AS (SELECT owner_id FROM projection.statement WHERE statement_id = $2)
		DELETE FROM projection.search_vocab v USING o
		 WHERE v.tenant_id = $1 AND v.owner_id = o.owner_id
		   AND v.word NOT IN (SELECT t.lexeme FROM projection.statement s, o, unnest(`+vocabTSV+`) t
		                       WHERE s.tenant_id = $1 AND s.owner_id = o.owner_id AND NOT s.hidden)`,
		tenant, id); err != nil {
		return err
	}
	// the same for stem_vocab: only stems some search row of the owner still holds (the purged one is gone above)
	_, err := tx.Exec(ctx, `
		WITH o AS (SELECT owner_id FROM projection.statement WHERE statement_id = $2)
		DELETE FROM projection.stem_vocab v USING o
		 WHERE v.tenant_id = $1 AND v.owner_id = o.owner_id
		   AND v.stem NOT IN (SELECT t FROM projection.statement_search x, o, unnest(tsvector_to_array(x.stems)) t
		                       WHERE x.tenant_id = $1 AND x.owner_id = o.owner_id)`,
		tenant, id)
	return err
}

// journalQuarantined hides a record or a task from every read without erasing it: the row keeps its
// content, a record leaves search. The verb accepts the same targets as purge.
func journalQuarantined(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	return quarantine(ctx, tx, e, true)
}

// journalReleased undoes a quarantine: the record comes back to search from its current content.
func journalReleased(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	return quarantine(ctx, tx, e, false)
}

// The target is the event's own entity (the verb writes it so), not payload.target: a later purge of the target
// replaces the payload of every event of the entity, this one included, with the purge marker.
func quarantine(ctx context.Context, tx pgx.Tx, e *journal.Event, on bool) error {
	id := e.EntityID
	switch e.EntityType {
	case "statement":
		// last_seq moves, as on purge: consumers of a feed by last_seq see the record go and come back
		if err := exec1(ctx, tx, "statement "+id.String(),
			`UPDATE projection.statement SET quarantined = $2, last_seq = $3, last_event_id = $4 WHERE statement_id = $1`,
			id, on, e.Seq, e.EventID); err != nil {
			return err
		}
		if on {
			return unsearchable(ctx, tx, e.TenantID, id)
		}
		return lexical(ctx, tx, id, e.Seq)
	case "task":
		return exec1(ctx, tx, "task "+id.String(),
			`UPDATE projection.task SET quarantined = $2, last_seq = $3, last_event_id = $4 WHERE task_id = $1`, id, on, e.Seq, e.EventID)
	}
	return fmt.Errorf("%w: quarantine of a %s", ErrMissing, e.EntityType)
}

// ---------------------------------------------------------------------------
// links
// ---------------------------------------------------------------------------

type entityRef struct {
	EntityType string    `json:"entity_type"`
	EntityID   uuid.UUID `json:"entity_id"`
}

func confirmations(ctx context.Context, tx pgx.Tx, relation string, to entityRef, delta int) error {
	if relation != "confirms" || to.EntityType != "statement" {
		return nil
	}
	return exec1(ctx, tx, "confirmed statement "+to.EntityID.String(),
		`UPDATE projection.statement SET confirmations = confirmations + $2 WHERE statement_id = $1`, to.EntityID, delta)
}

func linkAsserted(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		From     entityRef `json:"from"`
		To       entityRef `json:"to"`
		Relation string    `json:"relation"`
		Note     *string   `json:"note"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	// Re-asserting a retired edge revives it under the new link id (UNIQUE from/to/relation).
	var wasActive bool
	err := tx.QueryRow(ctx, `
		WITH prev AS (
		  SELECT NOT retired AS active FROM projection.link
		   WHERE from_type = $3 AND from_id = $4 AND to_type = $5 AND to_id = $6 AND relation = $7)
		INSERT INTO projection.link (link_id, tenant_id, from_type, from_id, to_type, to_id, relation, note, actor_id, asserted_seq, owner_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,litopys_root($9))
		ON CONFLICT (from_type, from_id, to_type, to_id, relation) DO UPDATE
		   SET link_id = EXCLUDED.link_id, note = EXCLUDED.note, actor_id = EXCLUDED.actor_id,
		       retired = false, asserted_seq = EXCLUDED.asserted_seq
		RETURNING COALESCE((SELECT active FROM prev), false)`,
		e.EntityID, e.TenantID, p.From.EntityType, p.From.EntityID, p.To.EntityType, p.To.EntityID,
		p.Relation, p.Note, e.ActorID, e.Seq).Scan(&wasActive)
	if err != nil {
		return err
	}
	if wasActive {
		return nil // the same edge was already live: confirmations unchanged
	}
	return confirmations(ctx, tx, p.Relation, p.To, +1)
}

func linkRetired(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var relation string
	var to entityRef
	err := tx.QueryRow(ctx, `
		UPDATE projection.link SET retired = true WHERE link_id = $1 AND NOT retired
		RETURNING relation, to_type, to_id`, e.EntityID).Scan(&relation, &to.EntityType, &to.EntityID)
	if err != nil {
		return fmt.Errorf("%w: live link %s: %v", ErrMissing, e.EntityID, err)
	}
	return confirmations(ctx, tx, relation, to, -1)
}

// ---------------------------------------------------------------------------
// projects · tasks · facts · consumers
// ---------------------------------------------------------------------------

func projectDeclared(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Name        string  `json:"name"`
		Title       *string `json:"title"` // absent = the slug
		Summary     *string `json:"summary"`
		RepoURL     *string `json:"repo_url"`
		AutoCreated bool    `json:"auto_created"`
		DataClass   *string `json:"data_class"` // 0023; absent = internal
		Scope       *string `json:"scope"`      // 0041
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO projection.project (project_id, tenant_id, name, title, summary, repo_url, auto_created, updated_at, declared_seq, last_seq, owner_id, data_class, scope)
		VALUES ($1,$2,$3,$10,$4,$5,$6,$7,$8,$8,litopys_root($9),COALESCE($11,'internal'),$12)`,
		e.EntityID, e.TenantID, p.Name, p.Summary, p.RepoURL, p.AutoCreated, effective(e), e.Seq, e.ActorID, p.Title, p.DataClass, p.Scope)
	return err
}

func projectAmended(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Name      *string   `json:"name"`
		Title     *string   `json:"title"`
		Summary   *string   `json:"summary"`
		RepoURL   *string   `json:"repo_url"`
		Labels    *[]string `json:"labels"`     // the whole new set (policy, 0010)
		DataClass *string   `json:"data_class"` // 0023
		Scope     *string   `json:"scope"`      // 0041
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	var labels []string
	if p.Labels != nil {
		labels = strs(*p.Labels)
	}
	return exec1(ctx, tx, "project "+e.EntityID.String(), `
		UPDATE projection.project
		   SET name = COALESCE($2, name), summary = COALESCE($3, summary), repo_url = COALESCE($4, repo_url),
		       labels = COALESCE($7, labels), title = COALESCE($8, title), data_class = COALESCE($9, data_class),
		       scope = COALESCE($10, scope), updated_at = GREATEST(updated_at, $5), last_seq = $6
		 WHERE project_id = $1`, e.EntityID, p.Name, p.Summary, p.RepoURL, workClock(e), e.Seq, labels, p.Title, p.DataClass, p.Scope)
}

// policySet projects a rule: the same entity (scope, owner, name) replaces it; active=false keeps
// the row for explain and history but takes it out of decisions.
func policySet(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Name   string          `json:"name"`
		Scope  string          `json:"scope"`
		Rule   json.RawMessage `json:"rule"`
		Active bool            `json:"active"`
		Reason *string         `json:"reason"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO projection.policy (policy_id, tenant_id, owner_id, scope, name, rule, active, reason, actor_id, set_event_id, set_seq)
		VALUES ($1, $2, litopys_root($3), $4, $5, $6, $7, $8, $3, $9, $10)
		ON CONFLICT (policy_id) DO UPDATE
		   SET rule = EXCLUDED.rule, active = EXCLUDED.active, reason = EXCLUDED.reason, actor_id = EXCLUDED.actor_id,
		       owner_id = EXCLUDED.owner_id, set_event_id = EXCLUDED.set_event_id, set_seq = EXCLUDED.set_seq`,
		e.EntityID, e.TenantID, e.ActorID, p.Scope, p.Name, string(p.Rule), p.Active, p.Reason, e.EventID, e.Seq)
	return err
}

func projectTransitioned(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Status string `json:"status"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	// Archiving or restoring is not work on the project: the current server leaves updated_at alone
	// (archive_my_project), and the importer replays it at import time.
	return exec1(ctx, tx, "project "+e.EntityID.String(), `
		UPDATE projection.project SET status = $2, last_seq = $3 WHERE project_id = $1`,
		e.EntityID, p.Status, e.Seq)
}

// Task fields of the todo v2 model and their SQL types. Order is fixed for deterministic SQL.
var taskFields = []struct{ name, typ string }{
	{"title", "text"}, {"body", "text"}, {"kind", "text"}, {"status", "text"}, {"priority", "text"},
	{"rank", "int"}, {"role", "text"}, {"owner", "text"}, {"due", "date"}, {"parent", "int"},
	{"acceptance", "text"}, {"severity", "text"}, {"resolution", "text"}, {"labels", "text[]"}, {"refs", "text[]"},
	{"owner_project", "uuid"}, {"assignee", "jsonb"}, // 0019: the executing project of a handoff, who does it
	{"data_class", "text"}, // 0023: raises the project's class
}

// taskValue decodes one payload field into a SQL parameter; JSON null -> nil (clears the column).
func taskValue(typ string, raw json.RawMessage) (any, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	switch typ {
	case "int":
		var v int
		return v, json.Unmarshal(raw, &v)
	case "text[]":
		var v []string
		err := json.Unmarshal(raw, &v)
		return strs(v), err
	case "jsonb":
		return string(raw), nil
	default: // text, uuid, date (as strings, cast in SQL)
		var v string
		return v, json.Unmarshal(raw, &v)
	}
}

func taskDeclared(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p map[string]json.RawMessage
	if err := decode(e, &p); err != nil {
		return err
	}
	var projectID uuid.UUID
	var num int
	if err := json.Unmarshal(p["project_id"], &projectID); err != nil {
		return fmt.Errorf("payload project_id: %w", err)
	}
	if err := json.Unmarshal(p["num"], &num); err != nil {
		return fmt.Errorf("payload num: %w", err)
	}
	if _, purged := p["purged"]; purged {
		p["title"] = json.RawMessage(`"` + purgedTitle + `"`) // replay of a purged task: the marker keeps structure only (0033)
	}
	cols := []string{"task_id", "tenant_id", "project_id", "num", "declared_seq", "last_seq", "last_event_id", "owner_id"}
	vals := []string{"$1", "$2", "$3", "$4", "$5", "$5", "$6", "(SELECT owner_id FROM projection.project WHERE project_id = $3)"}
	args := []any{e.EntityID, e.TenantID, projectID, num, e.Seq, e.EventID}
	for _, f := range taskFields {
		raw, ok := p[f.name]
		if !ok || string(raw) == "null" {
			continue
		}
		v, err := taskValue(f.typ, raw)
		if err != nil {
			return fmt.Errorf("payload %s: %w", f.name, err)
		}
		args = append(args, v)
		cols = append(cols, f.name)
		vals = append(vals, fmt.Sprintf("$%d::%s", len(args), f.typ))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO projection.task (`+strings.Join(cols, ", ")+`) VALUES (`+strings.Join(vals, ", ")+`)`, args...); err != nil {
		return err
	}
	return touchProject(ctx, tx, projectID, e)
}

// taskUpdate applies a change and bumps revision (the if_revision guard of the verbs).
// Placeholders $1..$3 are task_id, seq, event_id; set uses $4 onwards.
func taskUpdate(ctx context.Context, tx pgx.Tx, e *journal.Event, set string, args ...any) error {
	var projectID uuid.UUID
	all := append([]any{e.EntityID, e.Seq, e.EventID}, args...)
	err := tx.QueryRow(ctx, `
		UPDATE projection.task SET `+set+`, revision = revision + 1, last_seq = $2, last_event_id = $3
		 WHERE task_id = $1
		RETURNING project_id`, all...).Scan(&projectID)
	if err != nil {
		return fmt.Errorf("%w: task %s: %v", ErrMissing, e.EntityID, err)
	}
	return touchProject(ctx, tx, projectID, e)
}

// taskAmended: every field present in the payload is written (null clears; kind/labels/refs fall back
// to their defaults), absent fields are kept.
func taskAmended(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p map[string]json.RawMessage
	if err := decode(e, &p); err != nil {
		return err
	}
	var sets []string
	var args []any
	for _, f := range taskFields {
		raw, ok := p[f.name]
		if !ok {
			continue
		}
		v, err := taskValue(f.typ, raw)
		if err != nil {
			return fmt.Errorf("payload %s: %w", f.name, err)
		}
		args = append(args, v)
		ph := fmt.Sprintf("$%d::%s", len(args)+3, f.typ)
		switch {
		case v == nil && f.name == "kind":
			ph = "'task'"
		case v == nil && (f.name == "labels" || f.name == "refs"):
			ph = "'{}'"
		case v == nil && (f.name == "title" || f.name == "status"):
			args = args[:len(args)-1]
			continue // required columns are never cleared
		}
		if v == nil && ph[0] == '\'' {
			args = args[:len(args)-1]
		}
		sets = append(sets, f.name+" = "+ph)
	}
	if len(sets) == 0 {
		sets = append(sets, "title = title")
	}
	return taskUpdate(ctx, tx, e, strings.Join(sets, ", "), args...)
}

func taskClosed(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Outcome    string  `json:"outcome"`
		Resolution *string `json:"resolution"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	// an empty outcome: a marker of a task purged before 0033 kept no status — the status stays as it was
	return taskUpdate(ctx, tx, e, `status = COALESCE(NULLIF($4, ''), status), resolution = COALESCE($5, resolution), closed_at = $6`,
		p.Outcome, p.Resolution, effective(e))
}

func taskReopened(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Status *string `json:"status"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	status := "open"
	if p.Status != nil {
		status = *p.Status
	}
	return taskUpdate(ctx, tx, e, `status = $4, closed_at = NULL`, status)
}

func taskRetracted(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	return taskUpdate(ctx, tx, e, `retracted = true`)
}

// taskPurged erases the content of a task (0033). E7 contract, as for statements: the row must equal what replay
// builds from the markers, which keep the task's structure (status, kind, priority, …) and drop everything this
// handler clears. The row stays hidden, so its number is never reused.
func taskPurged(ctx context.Context, tx pgx.Tx, e *journal.Event, id uuid.UUID) error {
	return exec1(ctx, tx, "task "+id.String(), `
		UPDATE projection.task
		   SET purged = true, title = $4, body = NULL, owner = NULL, acceptance = NULL, resolution = NULL,
		       labels = '{}', refs = '{}', assignee = NULL, last_seq = $3, last_event_id = $2
		 WHERE task_id = $1`, id, e.EventID, e.Seq, purgedTitle)
}

func factRecorded(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		ProjectID uuid.UUID  `json:"project_id"`
		Kind      string     `json:"kind"`
		Key       string     `json:"key"`
		Value     *string    `json:"value"`
		SecretRef *string    `json:"secret_ref"`
		SourceID  *uuid.UUID `json:"source_id"` // 0047: the record it was taken from
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO projection.fact (tenant_id, project_id, kind, key, value, secret_ref, last_seq, last_event_id, owner_id, source_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,(SELECT owner_id FROM projection.project WHERE project_id = $2),$9)
		ON CONFLICT (project_id, kind, key) DO UPDATE
		   SET value = EXCLUDED.value, secret_ref = EXCLUDED.secret_ref, retired = false, confirmed_at = NULL,
		       last_seq = EXCLUDED.last_seq, last_event_id = EXCLUDED.last_event_id, source_id = EXCLUDED.source_id`, // a new version needs a new confirmation (0039)
		e.TenantID, p.ProjectID, p.Kind, p.Key, p.Value, p.SecretRef, e.Seq, e.EventID, p.SourceID); err != nil {
		return err
	}
	return touchProject(ctx, tx, p.ProjectID, e)
}

// factConfirmed (0039): the person confirmed the version of the fact that record_event_id wrote. The verb refuses
// a confirmation of an older version, so the row still carries that event; last_event_id stays — it names the author
// of the label for the worker.
func factConfirmed(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		RecordEventID uuid.UUID `json:"record_event_id"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	return exec1(ctx, tx, "fact "+e.EntityID.String(), `
		UPDATE projection.fact SET confirmed_at = $3, last_seq = $4
		 WHERE tenant_id = $1 AND last_event_id = $2 AND NOT retired`, e.TenantID, p.RecordEventID, effective(e), e.Seq)
}

// factRetired: projection.fact is keyed by (project, kind, key) and has no fact id, while the event
// addresses the fact by entity_id. The key is taken from the latest fact.recorded of that entity in
// the journal (same transaction, so it is visible during replay as well).
func factRetired(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var key struct {
		ProjectID uuid.UUID `json:"project_id"`
		Kind      string    `json:"kind"`
		Key       string    `json:"key"`
	}
	var raw []byte
	err := tx.QueryRow(ctx, `
		SELECT payload FROM journal
		 WHERE tenant_id = $1 AND entity_type = 'fact' AND entity_id = $2 AND type = 'fact.recorded' AND seq < $3
		 ORDER BY seq DESC LIMIT 1`, e.TenantID, e.EntityID, e.Seq).Scan(&raw)
	if err != nil {
		return fmt.Errorf("%w: fact.recorded for %s: %v", ErrMissing, e.EntityID, err)
	}
	if err := json.Unmarshal(raw, &key); err != nil {
		return fmt.Errorf("fact.recorded payload: %w", err)
	}
	if err := exec1(ctx, tx, "fact "+key.Kind+"/"+key.Key, `
		UPDATE projection.fact SET retired = true, last_seq = $4, last_event_id = $5
		 WHERE project_id = $1 AND kind = $2 AND key = $3`,
		key.ProjectID, key.Kind, key.Key, e.Seq, e.EventID); err != nil {
		return err
	}
	return touchProject(ctx, tx, key.ProjectID, e)
}

func consumerCursorSet(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		Consumer string `json:"consumer"`
		Seq      int64  `json:"seq"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO projection.consumer (tenant_id, consumer, seq, updated_at, owner_id) VALUES ($1,$2,$3,$4,litopys_root($5))
		ON CONFLICT (tenant_id, owner_id, consumer) DO UPDATE SET seq = EXCLUDED.seq, updated_at = EXCLUDED.updated_at`,
		e.TenantID, p.Consumer, p.Seq, effective(e), e.ActorID)
	return err
}

// attachmentAdded (0049): what describes a file of a project; the bytes stay in the app's data directory.
func attachmentAdded(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	var p struct {
		ProjectID uuid.UUID `json:"project_id"`
		SHA256    string    `json:"sha256"`
		Name      string    `json:"name"`
		Mime      string    `json:"mime"`
		Size      int64     `json:"size"`
	}
	if err := decode(e, &p); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO projection.attachment (attachment_id, tenant_id, project_id, owner_id, sha256, name, mime, size, added_by,
		       added_at, last_seq, last_event_id)
		VALUES ($1, $2, $3, (SELECT owner_id FROM projection.project WHERE project_id = $3), $4, $5, $6, $7, $8, $9, $10, $11)`,
		e.EntityID, e.TenantID, p.ProjectID, p.SHA256, p.Name, p.Mime, p.Size, e.ActorID, e.RecordedAt, e.Seq, e.EventID); err != nil {
		return err
	}
	return touchProject(ctx, tx, p.ProjectID, e)
}
