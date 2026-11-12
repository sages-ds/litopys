package verbs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/privacy"
)

type Part struct {
	Index int `json:"index"`
	Total int `json:"total"`
}

type StatementAssertIn struct {
	Project     string         `json:"project"`
	Type        string         `json:"type"`
	Title       string         `json:"title"`
	Body        *string        `json:"body,omitempty"`
	Concepts    []string       `json:"concepts,omitempty"`
	Files       []string       `json:"files,omitempty"`
	ExternalRef *string        `json:"external_ref,omitempty"`
	Part        *Part          `json:"part,omitempty"` // set by the facade when it splits a long body
	ContextID   *uuid.UUID     `json:"context_id,omitempty"`
	EffectiveAt *time.Time     `json:"effective_at,omitempty"`
	Supersedes  *uuid.UUID     `json:"supersedes,omitempty"`
	DataClass   *string        `json:"data_class,omitempty"` // raises the project's class for this statement (0023)
	Refs        []string       `json:"refs,omitempty"`       // as given; internal ones arrive resolved in Mentions
	Fields      map[string]any `json:"fields,omitempty"`     // structured fields of the type
	Mentions    []EntityRef    `json:"-"`
	// RetainUntil and OnExpiry: the retention of this record (0043) — both or neither
	RetainUntil *string `json:"retain_until,omitempty"`
	OnExpiry    *string `json:"on_expiry,omitempty"`
	// ExternalRefIsLabel: external_ref is a shared correlation label, not an idempotency key.
	// The memory_* facade sets it — in the current contract several observations share one ref,
	// and dedup there is title + body head within 24h, done by the facade.
	ExternalRefIsLabel bool `json:"-"`
	// Outcome: the structure of a type=outcome statement; only session_close sets it (writeOutcome).
	Outcome map[string]any `json:"-"`
	// LegacyTypes: the memory_* facade 1.19 keeps the old closed list of types, stored by the fixed legacy map
	// (bugfix → dev.bugfix) outside the project's vocabularies.
	LegacyTypes bool `json:"-"`
}

type StatementAssertOut struct {
	StatementID uuid.UUID `json:"statement_id"`
	Num         int64     `json:"num"` // R-<num> within the owner
	Ref         string    `json:"ref"`
	Type        string    `json:"type"` // as stored: a short name of the project's vocabularies resolves to the full one
	Ack
	Masked bool `json:"masked,omitempty"`
}

// clean strips private blocks and masks secrets; masked reports whether masking changed anything.
func (in *StatementAssertIn) clean() (masked bool) {
	title := privacy.NormalizeOneLine(privacy.StripPrivate(in.Title))
	mt := privacy.MaskTitle(title)
	masked = mt != title
	in.Title = mt
	if in.Body != nil {
		body := privacy.StripPrivate(*in.Body)
		mb := privacy.MaskText(body)
		masked = masked || mb != body
		in.Body = &mb
	}
	return masked
}

func (in *StatementAssertIn) validate() error {
	if err := checkProjectName("project", in.Project); err != nil {
		return err
	}
	if err := checkType("type", in.Type, in.LegacyTypes); err != nil {
		return err
	}
	if err := checkLen("title", in.Title, 1, 500); err != nil {
		return err
	}
	if err := optLen("body", in.Body, 32768); err != nil {
		return err
	}
	if err := checkList("concepts", in.Concepts, 20, 100); err != nil {
		return err
	}
	if err := checkList("files", in.Files, 50, 512); err != nil {
		return err
	}
	if err := optLen("external_ref", in.ExternalRef, 512); err != nil {
		return err
	}
	if err := checkList("refs", in.Refs, maxRefs, 300); err != nil {
		return err
	}
	if in.Part != nil && (in.Part.Index < 1 || in.Part.Total < in.Part.Index) {
		return invalid("part", "index %d / total %d", in.Part.Index, in.Part.Total)
	}
	if in.RetainUntil != nil || in.OnExpiry != nil {
		if err := checkRecordRetention(in.RetainUntil, in.OnExpiry, time.Now()); err != nil {
			return err
		}
	}
	return checkDataClass("data_class", in.DataClass)
}

// DedupWindow: a repeat of the same record within it is not written again.
var DedupWindow = 10 * time.Minute

func (c *cmd) assert(in StatementAssertIn, out *StatementAssertOut) error {
	projectID, err := c.projectID(in.Project)
	if err != nil {
		return err
	}
	// session_close writes the reserved outcome itself (Outcome set); the facade keeps its old list
	family, warning, err := c.resolveType(&in.Type, projectID, in.LegacyTypes || in.Outcome != nil)
	if err != nil {
		return err
	}
	out.Type = in.Type
	if in.ExternalRef != nil && !in.ExternalRefIsLabel { // idempotency: external_ref (+ part) within the project
		var id uuid.UUID
		var partIndex *int
		if in.Part != nil {
			partIndex = &in.Part.Index
		}
		var num int64
		err := c.tx.QueryRow(c.ctx, `SELECT statement_id, num FROM projection.statement
		        WHERE project_id = $1 AND external_ref = $2 AND part_index IS NOT DISTINCT FROM $3`,
			projectID, *in.ExternalRef, partIndex).Scan(&id, &num)
		if err == nil {
			out.StatementID, out.Deduped, out.Num, out.Ref = id, true, num, RecordRef(num)
			if in.RetainUntil != nil {
				out.Warnings = append(out.Warnings, "retention_not_set: the record exists already (external_ref); set its retention with statement_retain")
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	// a repeat without external_ref — a retry after a lost answer (a 502): the same type, title and
	// body head in the project within DedupWindow is answered with the record already there. Outcomes (session.close)
	// and the facade (its own 24 h rule) are out.
	if in.ExternalRef == nil && in.Outcome == nil && !in.LegacyTypes {
		body := ""
		if in.Body != nil {
			body = *in.Body
		}
		var id uuid.UUID
		var num int64
		err := c.tx.QueryRow(c.ctx, `SELECT statement_id, num FROM projection.statement
		        WHERE project_id = $1 AND type = $2 AND title = $3 AND left(COALESCE(body, ''), 500) = left($4, 500)
		          AND recorded_at > now() - $5::interval AND status = 'current' AND NOT hidden
		        ORDER BY recorded_at DESC LIMIT 1`,
			projectID, in.Type, in.Title, body, fmt.Sprintf("%d seconds", int(DedupWindow.Seconds()))).Scan(&id, &num)
		if err == nil {
			out.StatementID, out.Deduped, out.Num, out.Ref = id, true, num, RecordRef(num)
			out.Warnings = append(out.Warnings, "deduped: the same record was written moments ago; it is returned, nothing new is written")
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	if in.ContextID != nil {
		// A context belongs to the terminal, not to a project (events.yaml context.opened): writing into
		// another project from the same session is normal — 16% of the current corpus does it.
		// A closed context still takes writes, as today.
		_, run, _, err := c.context(*in.ContextID)
		if err != nil {
			return err
		}
		c.run = run
	}
	payload := map[string]any{"project_id": projectID, "type": in.Type, "family": family, "title": in.Title}
	if in.Body != nil {
		payload["body"] = *in.Body
	}
	if len(in.Concepts) > 0 {
		payload["concepts"] = in.Concepts
	}
	if len(in.Files) > 0 {
		payload["files"] = in.Files
	}
	if in.ExternalRef != nil {
		payload["external_ref"] = *in.ExternalRef
	}
	if in.Part != nil {
		payload["part"] = in.Part
	}
	if in.DataClass != nil {
		payload["data_class"] = *in.DataClass
	}
	if in.Outcome != nil {
		payload["outcome"] = in.Outcome
	}
	if len(in.Refs) > 0 {
		payload["refs"] = in.Refs
	}
	fields, fw, err := checkFields(in.Type, in.Fields)
	if err != nil {
		return err
	}
	if len(fields) > 0 {
		payload["fields"] = fields
	}
	if in.RetainUntil != nil {
		payload["retain_until"], payload["on_expiry"] = *in.RetainUntil, *in.OnExpiry
	}
	if out.Num, err = c.nextRecordNum(projectID); err != nil {
		return err
	}
	payload["num"], out.Ref = out.Num, RecordRef(out.Num)
	out.StatementID = uuid.New()
	e, err := c.emit("statement.asserted", "statement", out.StatementID, in.ContextID, in.EffectiveAt, payload)
	if err != nil {
		return err
	}
	out.Ack = ack(e)
	out.Warnings = append(warningList(warning), fw...)
	if len(in.Mentions) > 0 {
		if err := c.setMentions(EntityRef{EntityType: "statement", EntityID: out.StatementID}, in.Mentions); err != nil {
			return err
		}
	}
	if in.Supersedes != nil {
		if err := c.supersede(*in.Supersedes, out.StatementID, projectID, nil); err != nil {
			return err
		}
	}
	return nil
}

// supersede marks old as replaced by new (old must be a current statement of the same project).
func (c *cmd) supersede(old, replacement, projectID uuid.UUID, reason *string) error {
	var oldProject, author uuid.UUID
	var status string
	err := c.tx.QueryRow(c.ctx, `SELECT project_id, actor_id, status FROM projection.statement WHERE statement_id = $1`,
		old).Scan(&oldProject, &author, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return fail("statement_not_found", "statement %s", old)
	}
	if err != nil {
		return err
	}
	if err := c.mine(author); err != nil {
		return err
	}
	if oldProject != projectID {
		return fail("statement_not_found", "statement %s is in another project", old)
	}
	if status != "current" {
		return fail("statement_not_current", "statement %s is %s", old, status)
	}
	if err := c.retentionGuard(old); err != nil { // the replacement would carry the content without the retention
		return err
	}
	payload := map[string]any{"superseded_by": replacement}
	if reason != nil {
		payload["reason"] = *reason
	}
	_, err = c.emit("statement.superseded", "statement", old, nil, nil, payload)
	return err
}

// StatementAssert writes a statement. Secrets are masked before the write (masked: true);
// a repeated external_ref (+ part) in the project returns the same statement (deduped).
func (s *Service) StatementAssert(ctx context.Context, p Principal, in StatementAssertIn) (StatementAssertOut, error) {
	var out StatementAssertOut
	out.Masked = in.clean()
	if err := in.validate(); err != nil {
		return out, err
	}
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		return c.assert(in, &out)
	})
	return out, err
}

// StatementSupersede asserts the replacement and supersedes the old one in one transaction.
func (s *Service) StatementSupersede(ctx context.Context, p Principal, old uuid.UUID, in StatementAssertIn, reason *string) (StatementAssertOut, error) {
	var out StatementAssertOut
	out.Masked = in.clean()
	if err := in.validate(); err != nil {
		return out, err
	}
	in.Supersedes = nil
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		if err := c.assert(in, &out); err != nil {
			return err
		}
		if out.Deduped {
			return fail("statement_exists", "external_ref already used by %s", out.StatementID)
		}
		projectID, err := c.projectID(in.Project)
		if err != nil {
			return err
		}
		return c.supersede(old, out.StatementID, projectID, reason)
	})
	return out, err
}

// statement loads a statement of the principal's human.
func (c *cmd) statement(id uuid.UUID) (status string, revision int, err error) {
	var author uuid.UUID
	err = c.tx.QueryRow(c.ctx, `SELECT actor_id, status, revision FROM projection.statement WHERE statement_id = $1 FOR UPDATE`,
		id).Scan(&author, &status, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, fail("statement_not_found", "statement %s", id)
	}
	if err != nil {
		return
	}
	if c.mine(author) != nil {
		return "", 0, fail("statement_not_found", "statement %s", id)
	}
	return
}

type StatementAmendIn struct {
	StatementID uuid.UUID `json:"statement_id"`
	IfRevision  *int      `json:"if_revision,omitempty"`
	Title       *string   `json:"title,omitempty"`
	Body        *string   `json:"body,omitempty"`
	Type        *string   `json:"type,omitempty"`
	Concepts    []string  `json:"concepts,omitempty"`
	Files       []string  `json:"files,omitempty"`
	Reason      *string   `json:"reason,omitempty"`
	DataClass   *string   `json:"data_class,omitempty"` // raise only (0023)
	// Refs replaces the list; nil = unchanged. Internal ones arrive resolved in Mentions.
	Refs *[]string `json:"refs,omitempty"`
	// Fields replaces the structured fields; nil = unchanged, {} = none.
	Fields   map[string]any `json:"fields,omitempty"`
	Mentions []EntityRef    `json:"-"`
	// LegacyTypes: as in StatementAssertIn — the facade's old list, stored by the legacy map.
	LegacyTypes bool `json:"-"`
}

// StatementAmend writes new content of the same statement; the previous one stays in history.
func (s *Service) StatementAmend(ctx context.Context, p Principal, in StatementAmendIn) (Ack, error) {
	payload := map[string]any{}
	if in.Title != nil {
		t := privacy.MaskTitle(privacy.NormalizeOneLine(privacy.StripPrivate(*in.Title)))
		if err := checkLen("title", t, 1, 500); err != nil {
			return Ack{}, err
		}
		payload["title"] = t
	}
	if in.Body != nil {
		b := privacy.MaskText(privacy.StripPrivate(*in.Body))
		if err := checkLen("body", b, 0, 32768); err != nil {
			return Ack{}, err
		}
		payload["body"] = b
	}
	if in.Type != nil {
		if err := checkType("type", *in.Type, in.LegacyTypes); err != nil {
			return Ack{}, err
		}
	}
	if in.Concepts != nil {
		if err := checkList("concepts", in.Concepts, 20, 100); err != nil {
			return Ack{}, err
		}
		payload["concepts"] = in.Concepts
	}
	if in.Files != nil {
		if err := checkList("files", in.Files, 50, 512); err != nil {
			return Ack{}, err
		}
		payload["files"] = in.Files
	}
	if in.DataClass != nil {
		if err := checkDataClass("data_class", in.DataClass); err != nil {
			return Ack{}, err
		}
		payload["data_class"] = *in.DataClass
	}
	if in.Refs != nil {
		if err := checkList("refs", *in.Refs, maxRefs, 300); err != nil {
			return Ack{}, err
		}
		payload["refs"] = *in.Refs
	}
	if len(payload) == 0 && in.Type == nil && in.Fields == nil { // type and fields are checked in the transaction
		return Ack{}, invalid("statement.amend", "nothing to change")
	}
	if in.Reason != nil {
		if err := checkLen("reason", *in.Reason, 0, 500); err != nil {
			return Ack{}, err
		}
		payload["reason"] = *in.Reason
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		status, revision, err := c.statement(in.StatementID)
		if err != nil {
			return err
		}
		if status == "retracted" {
			return fail("statement_retracted", "statement %s", in.StatementID)
		}
		if in.IfRevision != nil && *in.IfRevision != revision {
			return fail("revision_conflict", "revision is %d, not %d", revision, *in.IfRevision)
		}
		if in.DataClass != nil {
			if err := c.raiseOnly("statement", "statement_id", in.StatementID, *in.DataClass); err != nil {
				return err
			}
		}
		if in.Type != nil {
			var projectID uuid.UUID
			if err := c.tx.QueryRow(c.ctx, `SELECT project_id FROM projection.statement WHERE statement_id = $1`, in.StatementID).Scan(&projectID); err != nil {
				return err
			}
			typ := *in.Type
			family, warning, err := c.resolveType(&typ, projectID, in.LegacyTypes)
			if err != nil {
				return err
			}
			payload["type"], payload["family"] = typ, family
			a.Warnings = warningList(warning)
		}
		if in.Fields != nil {
			typ, _ := payload["type"].(string)
			if typ == "" {
				if err := c.tx.QueryRow(c.ctx, `SELECT type FROM projection.statement WHERE statement_id = $1`, in.StatementID).Scan(&typ); err != nil {
					return err
				}
			}
			fields, fw, err := checkFields(typ, in.Fields)
			if err != nil {
				return err
			}
			if fields == nil {
				fields = map[string]any{}
			}
			payload["fields"] = fields
			a.Warnings = append(a.Warnings, fw...)
		}
		// a change of metadata only (files, refs, concepts, class) does not move when the thing happened: an expense
		// that got its receipt attached kept jumping to today (roast r2, Р2-6)
		var effective *time.Time
		if metadataOnly(payload) {
			var at time.Time
			if err := c.tx.QueryRow(c.ctx, `SELECT effective_at FROM projection.statement WHERE statement_id = $1`, in.StatementID).Scan(&at); err != nil {
				return err
			}
			effective = &at
		}
		e, err := c.emit("statement.amended", "statement", in.StatementID, nil, effective, payload)
		if err != nil {
			return err
		}
		warnings := a.Warnings
		a = ack(e)
		a.Warnings = warnings
		if in.Refs != nil {
			return c.setMentions(EntityRef{EntityType: "statement", EntityID: in.StatementID}, in.Mentions)
		}
		return nil
	})
	return a, err
}

// metadataOnly: an amend that changes none of title, body, type or fields.
func metadataOnly(payload map[string]any) bool {
	for k := range payload {
		switch k {
		case "files", "refs", "concepts", "data_class", "reason":
		default:
			return false
		}
	}
	return true
}

func (s *Service) statusChange(ctx context.Context, p Principal, id uuid.UUID, reason *string, typ, from string) (Ack, error) {
	if err := optLen("reason", reason, 500); err != nil {
		return Ack{}, err
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		status, _, err := c.statement(id)
		if err != nil {
			return err
		}
		if typ == "statement.retracted" {
			if err := c.retractOwn(id); err != nil {
				return err
			}
		}
		if status != from {
			a.Already = true
			return nil
		}
		payload := map[string]any{}
		if reason != nil {
			payload["reason"] = *reason
		}
		e, err := c.emit(typ, "statement", id, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}

// StatementRetract is the author's soft delete; a repeated retract gives already.
func (s *Service) StatementRetract(ctx context.Context, p Principal, id uuid.UUID, reason *string) (Ack, error) {
	return s.statusChange(ctx, p, id, reason, "statement.retracted", "current")
}

// StatementReinstate undoes a retract; a statement that is not retracted gives already.
func (s *Service) StatementReinstate(ctx context.Context, p Principal, id uuid.UUID, reason *string) (Ack, error) {
	return s.statusChange(ctx, p, id, reason, "statement.reinstated", "retracted")
}

// purgeable: the entity types whose content the projector erases (journalPurged) and whose purged markers replay
// (0004, 0033). Any other type would erase the journal but leave its projection row — refused until it is added.
var purgeable = []string{"statement", "task"}

// JournalPurge physically erases the content of an entity (compliance). Runs as litopys_admin:
// journal_purge() appends journal.purged itself, the event is then projected in the same tx.
func (s *Service) JournalPurge(ctx context.Context, p Principal, entityType string, entityID uuid.UUID, reason string, legalBasis *string) (Ack, error) {
	if s.Admin == nil {
		return Ack{}, fail("forbidden", "journal.purge needs the admin role")
	}
	if err := checkEnum("target.entity_type", entityType, purgeable...); err != nil {
		return Ack{}, err
	}
	if err := checkLen("reason", reason, 1, 1000); err != nil {
		return Ack{}, err
	}
	if err := optLen("legal_basis", legalBasis, 200); err != nil {
		return Ack{}, err
	}
	var a Ack
	err := s.exec(ctx, s.Admin, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		run, cause := c.run, c.cause
		if run == uuid.Nil {
			run = p.ActorID
		}
		if cause == uuid.Nil {
			cause = run
		}
		live := false
		if entityType == "task" {
			var err error
			if live, err = c.taskLive(entityID); err != nil {
				return err
			}
		}
		var seq int64
		if err := c.tx.QueryRow(ctx, `SELECT journal_purge($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			p.TenantID, entityType, entityID, reason, legalBasis, p.ActorID, run, cause, run).Scan(&seq); err != nil {
			return err
		}
		if err := c.projectPurge(seq, &a); err != nil || !live {
			return err
		}
		return c.askDependents(entityID, "purged")
	})
	return a, err
}
