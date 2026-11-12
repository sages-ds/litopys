package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/files"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/projector"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/core/vocab"
)

// Dispatch of the slice-1 verbs (domain/verbs.yaml) to core/verbs and core/query. Inputs are decoded
// strictly; project references go through a.project (slug or title); writes in a project go through
// a.bind (sessions by client). The *_resume family lives in resume.go.

// writeAck is WriteAck of verbs.yaml: the event, and where the write landed.
type writeAck struct {
	verbs.Ack
	Binding
}

func init() {
	// ---------- session ----------
	handle("session.open", sessionOpen)
	handle("session.checkpoint", sessionCheckpoint)
	handle("session.close", sessionClose)
	handle("session.list", sessionList)

	// ---------- statement ----------
	handle("statement.assert", statementAssert)
	handle("statement.supersede", statementSupersede)
	handle("statement.amend", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.StatementAmendIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		b, err := a.bindStatement(ctx, c, in.StatementID)
		if err != nil {
			return nil, err
		}
		if in.Refs != nil {
			var project string
			if err := a.DB.QueryRow(ctx, `SELECT p.name FROM projection.statement s JOIN projection.project p ON p.project_id = s.project_id
			        WHERE s.statement_id = $1`, in.StatementID).Scan(&project); err != nil {
				return nil, errorf("statement_not_found", "statement %s", in.StatementID)
			}
			if in.Mentions, err = a.mentionsOf(ctx, c, *in.Refs, project); err != nil {
				return nil, err
			}
		}
		ack, err := a.V.StatementAmend(ctx, c.verbs(), in)
		b.Warnings = append(b.Warnings, ack.Warnings...)
		return writeAck{ack, b}, err
	})
	handle("statement.retract", statementState(false))
	handle("statement.reinstate", statementState(true))
	handle("statement.retain", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.StatementRetainIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		b, err := a.bindStatement(ctx, c, in.StatementID)
		if err != nil {
			return nil, err
		}
		ack, err := a.V.StatementRetain(ctx, c.verbs(), in)
		return writeAck{ack, b}, err
	})

	// ---------- recall ----------
	handle("recall.search", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			query.SearchIn
			IncludeFolded     *bool `json:"include_folded,omitempty"`
			IncludeSuperseded *bool `json:"include_superseded,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if in.Project != "" {
			slug, _, err := a.project(ctx, c, in.Project)
			if err != nil {
				return nil, err
			}
			in.Project = slug
		}
		in.ExcludeSuperseded = in.IncludeSuperseded != nil && !*in.IncludeSuperseded
		var err error
		if in.Types, err = a.readTypes(ctx, c, in.Project, in.Type, in.Family); err != nil {
			return nil, err
		}
		return a.Q.Search(ctx, c.query(), in.SearchIn)
	})
	handle("recall.read", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			IDs     []uuid.UUID `json:"ids"`
			History bool        `json:"history,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if len(in.IDs) < 1 || len(in.IDs) > 20 {
			return nil, invalid("ids: 1 to 20")
		}
		return a.Q.Read(ctx, c.query(), in.IDs, in.History)
	})
	handle("recall.as_of", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in query.AsOfIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if in.RecordedAt == nil && in.EffectiveAt == nil {
			return nil, invalid("pass recorded_at, effective_at or both")
		}
		if err := a.slugOf(ctx, c, &in.Project); err != nil {
			return nil, err
		}
		var err error
		if in.Types, err = a.readTypes(ctx, c, in.Project, in.Type, in.Family); err != nil {
			return nil, err
		}
		return a.Q.AsOf(ctx, c.query(), in)
	})
	handle("recall.timeline", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in query.TimelineIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if err := a.slugOf(ctx, c, &in.Project); err != nil {
			return nil, err
		}
		var err error
		if in.Types, err = a.readTypes(ctx, c, in.Project, in.Type, in.Family); err != nil {
			return nil, err
		}
		return a.Q.Timeline(ctx, c.query(), in)
	})

	// ---------- journal ----------
	handle("journal.changes", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in query.ChangesIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if in.Project != "" {
			if err := a.slugOf(ctx, c, &in.Project); err != nil {
				return nil, err
			}
		}
		return a.Q.Changes(ctx, c.query(), in)
	})
	handle("journal.purge", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Target     verbs.EntityRef `json:"target"`
			Reason     string          `json:"reason"`
			LegalBasis *string         `json:"legal_basis,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.V.JournalPurge(ctx, c.verbs(), in.Target.EntityType, in.Target.EntityID, in.Reason, in.LegalBasis)
	})

	handle("journal.quarantine", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Ref     string `json:"ref"`
			Project string `json:"project,omitempty"`
			Reason  string `json:"reason"`
			Secret  bool   `json:"secret,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		target, err := a.quarantineTarget(ctx, c, in.Ref, in.Project)
		if err != nil {
			return nil, err
		}
		return a.V.JournalQuarantine(ctx, c.verbs(), *target, in.Reason, in.Secret)
	})
	handle("journal.release", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Ref     string `json:"ref"`
			Project string `json:"project,omitempty"`
			Reason  string `json:"reason"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		target, err := a.quarantineTarget(ctx, c, in.Ref, in.Project)
		if err != nil {
			return nil, err
		}
		return a.V.JournalRelease(ctx, c.verbs(), *target, in.Reason)
	})

	// ---------- deletion ----------
	handle("deletion.propose", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Ref     string `json:"ref"`
			Project string `json:"project,omitempty"`
			Action  string `json:"action"`
			Reason  string `json:"reason"`
			Secret  bool   `json:"secret,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		target, err := a.quarantineTarget(ctx, c, in.Ref, in.Project)
		if err != nil {
			return nil, err
		}
		out, err := a.V.DeletionPropose(ctx, c.verbs(), *target, in.Action, in.Reason, in.Secret)
		if err != nil {
			return nil, err
		}
		impact, err := a.Q.Impact(ctx, c.query(), target.EntityType, target.EntityID)
		if err != nil {
			return nil, err
		}
		res := map[string]any{"deletion_id": out.DeletionID, "ref": out.Ref, "state": out.State, "impact": impact}
		if out.Already {
			res["already"] = true
		} else {
			res["event_id"], res["seq"] = out.EventID, out.Seq
		}
		return res, nil
	})
	handle("deletion.approve", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Deletion string         `json:"deletion"`
			Project  string         `json:"project,omitempty"`
			Impact   map[string]int `json:"impact,omitempty"`
			Note     *string        `json:"note,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		id, err := a.deletionID(ctx, c, in.Deletion, in.Project)
		if err != nil {
			return nil, err
		}
		return a.V.DeletionApprove(ctx, c.verbs(), id, in.Impact, in.Note)
	})
	handle("deletion.reject", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Deletion string `json:"deletion"`
			Project  string `json:"project,omitempty"`
			Reason   string `json:"reason"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		id, err := a.deletionID(ctx, c, in.Deletion, in.Project)
		if err != nil {
			return nil, err
		}
		return a.V.DeletionReject(ctx, c.verbs(), id, in.Reason)
	})
	handle("deletion.list", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in query.DeletionListIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if in.Project != "" {
			if err := a.slugOf(ctx, c, &in.Project); err != nil {
				return nil, err
			}
		}
		out, err := a.Q.DeletionList(ctx, c.query(), in)
		if err != nil {
			return nil, err
		}
		return map[string]any{"items": out.Items}, nil
	})
	handle("deletion.impact", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Ref     string `json:"ref"`
			Project string `json:"project,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if in.Project != "" {
			if err := a.slugOf(ctx, c, &in.Project); err != nil {
				return nil, err
			}
		}
		target, ok, err := a.lookupRef(ctx, c, in.Ref, in.Project, true)
		if err != nil {
			return nil, err
		}
		if !ok { // not a number or an id: a project by name or title
			_, id, err := a.project(ctx, c, in.Ref)
			if err != nil {
				return nil, err
			}
			target = &verbs.EntityRef{EntityType: "project", EntityID: id}
		}
		return a.Q.Impact(ctx, c.query(), target.EntityType, target.EntityID)
	})

	// ---------- project ----------
	handle("project.declare", projectDeclare)
	handle("project.amend", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.ProjectAmendIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if err := a.slugOf(ctx, c, &in.Project); err != nil {
			return nil, err
		}
		return a.V.ProjectAmend(ctx, c.verbs(), in)
	})
	handle("project.transition", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Project string  `json:"project"`
			Status  string  `json:"status"`
			Reason  *string `json:"reason,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if err := a.slugOf(ctx, c, &in.Project); err != nil {
			return nil, err
		}
		return a.V.ProjectTransition(ctx, c.verbs(), in.Project, in.Status, in.Reason)
	})
	handle("layer.list", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Project string `json:"project"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if err := a.slugOf(ctx, c, &in.Project); err != nil {
			return nil, err
		}
		return a.Q.Layers(ctx, c.query(), in.Project)
	})
	handle("vocabulary.list", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Project string `json:"project,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		var set vocab.Set
		if in.Project != "" {
			_, id, err := a.project(ctx, c, in.Project)
			if err != nil {
				return nil, err
			}
			if set, err = vocab.ProjectSet(ctx, a.DB, id); err != nil {
				return nil, err
			}
		}
		return map[string]any{"vocabularies": vocab.Catalog(set)}, nil
	})
	handle("vocabulary.read", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Key     string `json:"key"`
			Version *int   `json:"version,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		d, ok := vocab.Read(in.Key)
		if !ok || (in.Version != nil && *in.Version != d.Version) {
			return nil, &Error{Code: "vocabulary_not_found", Message: "no built-in vocabulary " + in.Key + " at that version",
				Hint: "vocabulary_list lists the vocabularies and their current versions"}
		}
		return d, nil
	})
	handle("attachment.record", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Project       string `json:"project"`
			Name          string `json:"name"`
			Mime          string `json:"mime,omitempty"`
			ContentBase64 string `json:"content_base64"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if a.Files == nil {
			return nil, &Error{Code: "attachments_off", Message: "this server keeps no files (LITOPYS_FILES_DIR is not set)", status: http.StatusNotImplemented}
		}
		b, err := base64.StdEncoding.DecodeString(in.ContentBase64)
		if err != nil {
			return nil, invalid("content_base64: %v", err)
		}
		if len(b) > files.MaxSize {
			return nil, invalid("the file is %d bytes, at most %d", len(b), files.MaxSize)
		}
		if _, _, err := a.project(ctx, c, in.Project); err != nil { // the project first: no file without a place for it
			return nil, err
		}
		sha, err := a.Files.Put(b)
		if err != nil {
			return nil, err
		}
		return a.V.AttachmentAdd(ctx, c.verbs(), in.Project, sha, in.Name, in.Mime, int64(len(b)))
	})
	handle("attachment.list", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Project string `json:"project,omitempty"`
			Ref     string `json:"ref,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if in.Project != "" {
			if err := a.slugOf(ctx, c, &in.Project); err != nil {
				return nil, err
			}
		}
		items, err := a.Q.Attachments(ctx, c.query(), in.Project, strings.TrimPrefix(in.Ref, "att:"))
		return map[string]any{"items": nonNil(items)}, err
	})
	handle("ledger.read", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Project  string `json:"project"`
			Type     string `json:"type,omitempty"`
			GroupBy  string `json:"group_by,omitempty"`
			DateFrom string `json:"date_from,omitempty"`
			DateTo   string `json:"date_to,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		slug, id, err := a.project(ctx, c, in.Project)
		if err != nil {
			return nil, err
		}
		q := query.LedgerIn{Project: slug, GroupBy: in.GroupBy}
		for _, d := range []struct {
			s   string
			dst **time.Time
		}{{in.DateFrom, &q.From}, {in.DateTo, &q.To}} {
			if d.s == "" {
				continue
			}
			t, err := time.Parse("2006-01-02", d.s)
			if err != nil {
				return nil, invalid("date: YYYY-MM-DD")
			}
			*d.dst = &t
		}
		if in.Type != "" {
			set, err := vocab.ProjectSet(ctx, a.DB, id)
			if err != nil {
				return nil, err
			}
			r, err := set.Resolve(in.Type)
			if err != nil {
				return nil, invalid("type: %v", err)
			}
			q.Type = r.Type
		}
		return a.Q.Ledger(ctx, c.query(), q)
	})
	handle("project.pack", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Project string `json:"project"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		slug, _, err := a.project(ctx, c, in.Project)
		if err != nil {
			return nil, err
		}
		return map[string]any{"project": slug, "path": "/viewer/export/" + slug + ".zip",
			"hint": "the person opens it in the viewer of this server (the same address as the connector, /viewer/), signed in"}, nil
	})
	handle("project.list", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Status string `json:"status,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		items, err := a.Q.ProjectList(ctx, c.query(), in.Status)
		return map[string]any{"items": nonNil(items)}, err
	})

	// ---------- task ----------
	handle("task.declare", taskDeclare)
	handle("task.amend", taskAmend)
	handle("task.close", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			taskRef
			Outcome    string  `json:"outcome"`
			Resolution *string `json:"resolution,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		id, b, err := a.bindTask(ctx, c, in.taskRef)
		if err != nil {
			return nil, err
		}
		ack, err := a.V.TaskClose(ctx, c.verbs(), id, in.Outcome, in.Resolution)
		return writeAck{ack, b}, err
	})
	handle("task.reopen", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			taskRef
			Status *string `json:"status,omitempty"`
			Reason *string `json:"reason,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		id, b, err := a.bindTask(ctx, c, in.taskRef)
		if err != nil {
			return nil, err
		}
		ack, err := a.V.TaskReopen(ctx, c.verbs(), id, in.Status, in.Reason)
		return writeAck{ack, b}, err
	})
	handle("task.retract", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			taskRef
			Reason string `json:"reason"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		id, b, err := a.bindTask(ctx, c, in.taskRef)
		if err != nil {
			return nil, err
		}
		ack, err := a.V.TaskRetract(ctx, c.verbs(), id, in.Reason)
		return writeAck{ack, b}, err
	})
	handle("task.list", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in query.TaskListIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if err := a.slugOf(ctx, c, &in.Project); err != nil {
			return nil, err
		}
		return a.Q.TaskList(ctx, c.query(), in)
	})

	// ---------- workflow (0020) ----------
	handle("workflow.declare", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.WorkflowDeclareIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		b, err := a.bindProject(ctx, c, &in.Project, nil)
		if err != nil {
			return nil, err
		}
		out, err := a.V.WorkflowDeclare(ctx, c.verbs(), in)
		return struct {
			verbs.WorkflowDeclareOut
			Binding
		}{out, b}, err
	})
	handle("workflow.amend", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		rest, fields, err := splitFields(args, map[string]bool{"project": true, "key": true}, map[string]bool{"title": true, "purpose": true})
		if err != nil {
			return nil, err
		}
		var project, key string
		_ = json.Unmarshal(rest["project"], &project)
		_ = json.Unmarshal(rest["key"], &key)
		b, err := a.bindProject(ctx, c, &project, nil)
		if err != nil {
			return nil, err
		}
		ack, err := a.V.WorkflowAmend(ctx, c.verbs(), verbs.WorkflowAmendIn{Project: project, Key: key, Fields: fields})
		return writeAck{ack, b}, err
	})
	handle("workflow.supersede", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.WorkflowSupersedeIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		b, err := a.bindProject(ctx, c, &in.Project, nil)
		if err != nil {
			return nil, err
		}
		out, err := a.V.WorkflowSupersede(ctx, c.verbs(), in)
		return struct {
			verbs.WorkflowSupersedeOut
			Binding
		}{out, b}, err
	})
	handle("workflow.retire", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Project string  `json:"project"`
			Key     string  `json:"key"`
			Reason  *string `json:"reason,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		b, err := a.bindProject(ctx, c, &in.Project, nil)
		if err != nil {
			return nil, err
		}
		ack, err := a.V.WorkflowRetire(ctx, c.verbs(), in.Project, in.Key, in.Reason)
		return writeAck{ack, b}, err
	})
	handle("workflow.list", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in query.WorkflowListIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if err := a.slugOf(ctx, c, &in.Project); err != nil {
			return nil, err
		}
		return a.Q.WorkflowList(ctx, c.query(), in)
	})
	handle("workflow.read", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in query.WorkflowReadIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if err := a.slugOf(ctx, c, &in.Project); err != nil {
			return nil, err
		}
		return a.Q.WorkflowRead(ctx, c.query(), in)
	})

	// ---------- execution · step · approval (0021) ----------
	handle("execution.start", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.ExecutionStartIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		b, err := a.bindProject(ctx, c, &in.Project, nil)
		if err != nil {
			return nil, err
		}
		out, err := a.V.ExecutionStart(ctx, c.verbs(), in)
		return struct {
			verbs.ExecutionStartOut
			Binding
		}{out, b}, err
	})
	handle("execution.close", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.ExecutionCloseIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.V.ExecutionClose(ctx, c.verbs(), in)
	})
	handle("execution.list", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in query.ExecutionListIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if in.Project != "" {
			if err := a.slugOf(ctx, c, &in.Project); err != nil {
				return nil, err
			}
		}
		return a.Q.ExecutionList(ctx, c.query(), in)
	})
	handle("execution.read", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			ExecutionID uuid.UUID `json:"execution_id"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.Q.ExecutionRead(ctx, c.query(), in.ExecutionID)
	})
	handle("step.start", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.StepStartIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.V.StepStart(ctx, c.verbs(), in)
	})
	handle("step.checkpoint", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.StepCheckpointIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.V.StepCheckpoint(ctx, c.verbs(), in)
	})
	handle("step.finish", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.StepFinishIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.V.StepFinish(ctx, c.verbs(), in)
	})
	handle("step.open_items", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in query.StepOpenItemsIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if in.Project != "" {
			if err := a.slugOf(ctx, c, &in.Project); err != nil {
				return nil, err
			}
		}
		return a.Q.StepOpenItems(ctx, c.query(), in)
	})
	handle("step.pack", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in query.StepPackIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.Q.StepPack(ctx, c.query(), in)
	})
	handle("step.delegate", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.StepDelegateIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if in.Agent != nil {
			if err := checkAgent(*in.Agent); err != nil {
				return nil, err
			}
		}
		parent := auth.Principal{TenantID: c.TenantID, ActorID: c.ActorID, Role: c.Role, TokenID: c.Grant.TokenID, Grant: c.Grant,
			OAuth: c.OAuth, Human: c.Owner}
		return a.V.StepDelegate(ctx, c.verbs(), parent, in)
	})
	handle("approval.grant", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.ApprovalGrantIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.V.ApprovalGrant(ctx, c.verbs(), in)
	})
	handle("approval.revoke", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			ApprovalID uuid.UUID `json:"approval_id"`
			Reason     *string   `json:"reason,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.V.ApprovalRevoke(ctx, c.verbs(), in.ApprovalID, in.Reason)
	})

	// ---------- fact ----------
	handle("fact.record", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.FactRecordIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		b, err := a.bindProject(ctx, c, &in.Project, nil)
		if err != nil {
			return nil, err
		}
		if in.Source != nil { // the record the fact comes from: it must exist, live or not
			ref, ok, err := a.lookupRef(ctx, c, *in.Source, in.Project, true)
			if !ok {
				return nil, invalid("source: R-N or the id of the record the fact comes from; a free-text source goes into a note record, then pass its R-N")
			}
			if err != nil {
				return nil, err
			}
			if ref.EntityType != "statement" {
				return nil, invalid("source: a record, not a %s", ref.EntityType)
			}
			in.SourceID = &ref.EntityID
		}
		ack, err := a.V.FactRecord(ctx, c.verbs(), in)
		return writeAck{ack, b}, err
	})
	handle("fact.retire", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Project string  `json:"project"`
			Kind    string  `json:"kind"`
			Key     string  `json:"key"`
			Reason  *string `json:"reason,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		b, err := a.bindProject(ctx, c, &in.Project, nil)
		if err != nil {
			return nil, err
		}
		ack, err := a.V.FactRetire(ctx, c.verbs(), in.Project, in.Kind, in.Key, in.Reason)
		return writeAck{ack, b}, err
	})
	handle("fact.list", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Project string `json:"project,omitempty"`
			Kind    string `json:"kind,omitempty"`
			Query   string `json:"query,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if in.Project != "" {
			if err := a.slugOf(ctx, c, &in.Project); err != nil {
				return nil, err
			}
		}
		items, err := a.Q.FactList(ctx, c.query(), in.Project, in.Kind, in.Query)
		return map[string]any{"items": nonNil(items)}, err
	})
	handle("work.open_items", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Project string `json:"project"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if err := a.slugOf(ctx, c, &in.Project); err != nil {
			return nil, err
		}
		return a.Q.WorkOpenItems(ctx, c.query(), in.Project)
	})

	// ---------- link ----------
	handle("link.assert", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			From     verbs.EntityRef `json:"from"`
			To       verbs.EntityRef `json:"to"`
			Relation string          `json:"relation"`
			Note     *string         `json:"note,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		id, ack, err := a.V.LinkAssert(ctx, c.verbs(), in.From, in.To, in.Relation, in.Note)
		return map[string]any{"link_id": id, "event_id": ack.EventID, "seq": ack.Seq, "already": ack.Already}, err
	})
	handle("link.retire", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			LinkID uuid.UUID `json:"link_id"`
			Reason *string   `json:"reason,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.V.LinkRetire(ctx, c.verbs(), in.LinkID, in.Reason)
	})

	// ---------- consumer ----------
	handle("consumer.checkpoint", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Consumer string `json:"consumer"`
			Seq      int64  `json:"seq"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.V.ConsumerCheckpoint(ctx, c.verbs(), in.Consumer, in.Seq)
	})
	handle("consumer.list", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Consumer string `json:"consumer,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.Q.ConsumerList(ctx, c.query(), in.Consumer)
	})

	// ---------- actor / run ----------
	handle("actor.register", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.ActorRegisterIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		id, ack, err := a.V.ActorRegister(ctx, c.verbs(), in)
		return map[string]any{"actor_id": id, "event_id": ack.EventID, "seq": ack.Seq, "already": ack.Already}, err
	})
	handle("actor.amend", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.ActorAmendIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.V.ActorAmend(ctx, c.verbs(), in)
	})
	handle("actor.revoke", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			ActorID uuid.UUID `json:"actor_id"`
			Reason  string    `json:"reason"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.V.ActorRevoke(ctx, c.verbs(), in.ActorID, in.Reason)
	})
	handle("run.start", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			ParentRunID *uuid.UUID      `json:"parent_run_id,omitempty"`
			Client      json.RawMessage `json:"client"`
			Purpose     *string         `json:"purpose,omitempty"`
			Agent       string          `json:"agent,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if err := checkAgent(in.Agent); err != nil {
			return nil, err
		}
		id, err := a.V.RunStart(ctx, c.verbs(), verbs.RunStartIn{ParentRunID: in.ParentRunID, Client: c.clientInfo(in.Client, in.Agent), Purpose: in.Purpose})
		return map[string]any{"run_id": id}, err
	})
	handle("run.finish", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			RunID   uuid.UUID `json:"run_id"`
			Outcome string    `json:"outcome"`
			Note    *string   `json:"note,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.V.RunFinish(ctx, c.verbs(), in.RunID, in.Outcome, in.Note)
	})

	// ---------- system ----------
	handle("system.whoami", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		if err := decode(args, &struct{}{}); err != nil {
			return nil, err
		}
		w, err := a.Q.Whoami(ctx, c.query())
		if err != nil {
			return nil, err
		}
		return map[string]any{"actor": w.Actor, "tenant_id": w.TenantID, "role": w.Role, "email": w.Email,
			"display_name": w.DisplayName, "service": "Litopys", "profile": c.profile(), "vendor": c.Grant.Vendor,
			"oauth": c.OAuth, "user_agent": c.UserAgent}, nil
	})
	handle("system.capabilities", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		if err := decode(args, &struct{}{}); err != nil {
			return nil, err
		}
		return a.capabilities(c)
	})
	handle("system.guide", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Topic string `json:"topic,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return systemGuide(in.Topic)
	})
	handle("actor.list", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in query.ActorListIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.Q.ActorList(ctx, c.query(), in)
	})
	handle("system.health", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		if err := decode(args, &struct{}{}); err != nil {
			return nil, err
		}
		return a.Q.Health(ctx, c.query(), projector.Version)
	})

	// ---------- policy ----------
	handle("policy.set", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in verbs.PolicySetIn
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		id, ack, err := a.V.PolicySet(ctx, c.verbs(), in)
		return map[string]any{"policy_id": id, "event_id": ack.EventID, "seq": ack.Seq, "unchanged": ack.Unchanged}, err
	})
	handle("policy.explain", func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			Project string     `json:"project,omitempty"`
			TokenID *uuid.UUID `json:"token_id,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if in.Project != "" {
			if err := a.slugOf(ctx, c, &in.Project); err != nil {
				return nil, err
			}
		}
		return a.Q.PolicyExplain(ctx, c.query(), in.Project, in.TokenID)
	})
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// slugOf replaces a project reference (slug or title) with the slug.
func (a *API) slugOf(ctx context.Context, c *Caller, ref *string) error {
	slug, _, err := a.project(ctx, c, *ref)
	if err == nil {
		*ref = slug
	}
	return err
}

// bindProject resolves the project reference in place and binds the write to a session.
func (a *API) bindProject(ctx context.Context, c *Caller, ref *string, explicit *uuid.UUID) (Binding, error) {
	slug, id, err := a.project(ctx, c, *ref)
	if err != nil {
		return Binding{}, err
	}
	*ref = slug
	return a.bind(ctx, c, id, explicit)
}

func (a *API) bindStatement(ctx context.Context, c *Caller, id uuid.UUID) (Binding, error) {
	var project uuid.UUID
	err := a.DB.QueryRow(ctx, `SELECT project_id FROM projection.statement WHERE statement_id = $1`, id).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		return Binding{}, errorf("statement_not_found", "statement %s", id)
	}
	if err != nil {
		return Binding{}, err
	}
	return a.bind(ctx, c, project, nil)
}

// taskRef addresses a task by id or by project and number (models think in numbers).
type taskRef struct {
	TaskID  *uuid.UUID `json:"task_id,omitempty"`
	Project string     `json:"project,omitempty"`
	Num     int        `json:"num,omitempty"`
}

func (a *API) bindTask(ctx context.Context, c *Caller, r taskRef) (uuid.UUID, Binding, error) {
	var id, project uuid.UUID
	switch {
	case r.TaskID != nil && r.Project == "" && r.Num == 0:
		err := a.DB.QueryRow(ctx, `SELECT project_id FROM projection.task WHERE task_id = $1 AND NOT hidden`, *r.TaskID).Scan(&project)
		if errors.Is(err, pgx.ErrNoRows) {
			return id, Binding{}, errorf("task_not_found", "task %s", *r.TaskID)
		}
		if err != nil {
			return id, Binding{}, err
		}
		id = *r.TaskID
	case r.TaskID == nil && r.Project != "" && r.Num > 0:
		slug, pid, err := a.project(ctx, c, r.Project)
		if err != nil {
			return id, Binding{}, err
		}
		if id, err = a.V.TaskByNum(ctx, c.verbs(), slug, r.Num); err != nil {
			return id, Binding{}, err
		}
		project = pid
	default:
		return id, Binding{}, invalid("address the task by task_id, or by project and num")
	}
	b, err := a.bind(ctx, c, project, nil)
	return id, b, err
}

func statementAssert(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
	var in struct {
		Project     string         `json:"project"`
		Type        string         `json:"type"`
		Title       string         `json:"title"`
		Body        *string        `json:"body,omitempty"`
		Concepts    []string       `json:"concepts,omitempty"`
		Files       []string       `json:"files,omitempty"`
		ExternalRef *string        `json:"external_ref,omitempty"`
		SessionID   *uuid.UUID     `json:"session_id,omitempty"`
		EffectiveAt *time.Time     `json:"effective_at,omitempty"`
		Supersedes  *uuid.UUID     `json:"supersedes,omitempty"`
		DataClass   *string        `json:"data_class,omitempty"`
		Refs        []string       `json:"refs,omitempty"`
		RetainUntil *string        `json:"retain_until,omitempty"`
		OnExpiry    *string        `json:"on_expiry,omitempty"`
		Fields      map[string]any `json:"fields,omitempty"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	b, err := a.bindProject(ctx, c, &in.Project, in.SessionID)
	if err != nil {
		return nil, err
	}
	mentions, err := a.mentionsOf(ctx, c, in.Refs, in.Project)
	if err != nil {
		return nil, err
	}
	out, err := a.V.StatementAssert(ctx, c.verbs(), verbs.StatementAssertIn{Project: in.Project, Type: in.Type, Title: in.Title,
		Body: in.Body, Concepts: in.Concepts, Files: in.Files, ExternalRef: in.ExternalRef, ContextID: b.SessionID,
		EffectiveAt: in.EffectiveAt, Supersedes: in.Supersedes, DataClass: in.DataClass, Refs: in.Refs, Mentions: mentions,
		RetainUntil: in.RetainUntil, OnExpiry: in.OnExpiry, Fields: in.Fields})
	if err != nil {
		return nil, err
	}
	b.Warnings = append(b.Warnings, out.Warnings...)
	return struct {
		verbs.StatementAssertOut
		Binding
	}{out, b}, nil
}

func statementSupersede(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
	var in struct {
		Supersedes uuid.UUID       `json:"supersedes"`
		Statement  json.RawMessage `json:"statement"`
		Reason     *string         `json:"reason,omitempty"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	var st struct {
		Project     string     `json:"project"`
		Type        string     `json:"type"`
		Title       string     `json:"title"`
		Body        *string    `json:"body,omitempty"`
		Concepts    []string   `json:"concepts,omitempty"`
		Files       []string   `json:"files,omitempty"`
		ExternalRef *string    `json:"external_ref,omitempty"`
		SessionID   *uuid.UUID `json:"session_id,omitempty"`
		EffectiveAt *time.Time `json:"effective_at,omitempty"`
		DataClass   *string    `json:"data_class,omitempty"`
		Refs        []string   `json:"refs,omitempty"`
	}
	if err := decode(in.Statement, &st); err != nil {
		return nil, err
	}
	b, err := a.bindProject(ctx, c, &st.Project, st.SessionID)
	if err != nil {
		return nil, err
	}
	mentions, err := a.mentionsOf(ctx, c, st.Refs, st.Project)
	if err != nil {
		return nil, err
	}
	out, err := a.V.StatementSupersede(ctx, c.verbs(), in.Supersedes, verbs.StatementAssertIn{Project: st.Project, Type: st.Type,
		Title: st.Title, Body: st.Body, Concepts: st.Concepts, Files: st.Files, ExternalRef: st.ExternalRef, ContextID: b.SessionID,
		EffectiveAt: st.EffectiveAt, DataClass: st.DataClass, Refs: st.Refs, Mentions: mentions}, in.Reason)
	if err != nil {
		return nil, err
	}
	b.Warnings = append(b.Warnings, out.Warnings...)
	return struct {
		verbs.StatementAssertOut
		Binding
	}{out, b}, nil
}

func statementState(reinstate bool) handler {
	return func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
		var in struct {
			StatementID uuid.UUID `json:"statement_id"`
			Reason      *string   `json:"reason,omitempty"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		b, err := a.bindStatement(ctx, c, in.StatementID)
		if err != nil {
			return nil, err
		}
		var ack verbs.Ack
		if reinstate {
			ack, err = a.V.StatementReinstate(ctx, c.verbs(), in.StatementID, in.Reason)
		} else {
			ack, err = a.V.StatementRetract(ctx, c.verbs(), in.StatementID, in.Reason)
		}
		return writeAck{ack, b}, err
	}
}

func projectDeclare(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
	var in struct {
		Name      string  `json:"name,omitempty"`
		Title     *string `json:"title,omitempty"`
		Summary   *string `json:"summary,omitempty"`
		RepoURL   *string `json:"repo_url,omitempty"`
		DataClass *string `json:"data_class,omitempty"`
		Scope     *string `json:"scope,omitempty"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	var slug string
	var created bool
	switch {
	case in.Name != "":
		var ack verbs.Ack
		err := a.inClientRun(ctx, c, func() (err error) {
			_, ack, err = a.V.ProjectDeclare(ctx, c.verbs(), verbs.ProjectDeclareIn{Name: in.Name, Title: in.Title, Summary: in.Summary,
				RepoURL: in.RepoURL, DataClass: in.DataClass, Scope: in.Scope})
			return err
		})
		if err != nil {
			return nil, err
		}
		slug, created = in.Name, !ack.Already
	case in.Title != nil:
		var err error
		if slug, created, err = a.declareFromTitle(ctx, c, *in.Title, in.Summary); err != nil {
			return nil, err
		}
		if created && (in.DataClass != nil || in.Scope != nil) { // the slug is derived there; class and scope follow as an amend
			if _, err := a.V.ProjectAmend(ctx, c.verbs(), verbs.ProjectAmendIn{Project: slug, DataClass: in.DataClass, Scope: in.Scope}); err != nil {
				return nil, err
			}
		}
	default:
		return nil, invalid("pass a title, a name (slug) or both")
	}
	_, id, err := a.project(ctx, c, slug)
	return map[string]any{"project_id": id, "name": slug, "created": created}, err
}

// taskFields: the todo v2 fields task.declare / task.amend pass through to core/verbs (it validates them).
var taskFields = map[string]bool{"status": true, "title": true, "body": true, "kind": true, "priority": true, "rank": true,
	"role": true, "owner": true, "due": true, "parent": true, "acceptance": true, "severity": true, "labels": true, "refs": true,
	"owner_project": true, "assignee": true, "data_class": true}

// taskRefs resolves the references among task fields before core/verbs sees them: owner_project by slug or title
// to the slug, an assignee agent through checkAgent (a role, never a model or vendor name).
func (a *API) taskRefs(ctx context.Context, c *Caller, fields map[string]any) error {
	if name, ok := fields["owner_project"].(string); ok {
		slug, _, err := a.project(ctx, c, name)
		if err != nil {
			return err
		}
		fields["owner_project"] = slug
	}
	if m, ok := fields["assignee"].(map[string]any); ok {
		if agent, ok := m["agent"].(string); ok {
			if err := checkAgent(agent); err != nil {
				return err
			}
		}
	}
	return nil
}

func splitFields(args json.RawMessage, keep map[string]bool, allowed map[string]bool) (map[string]json.RawMessage, map[string]any, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(args, &all); err != nil {
		return nil, nil, invalid("%v", err)
	}
	fields := map[string]any{}
	for k, v := range all {
		switch {
		case keep[k]:
		case allowed[k]:
			var x any
			if err := json.Unmarshal(v, &x); err != nil {
				return nil, nil, invalid("%s: %v", k, err)
			}
			fields[k] = x
			delete(all, k)
		default:
			return nil, nil, invalid("unknown field %q", k)
		}
	}
	return all, fields, nil
}

func taskDeclare(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
	rest, fields, err := splitFields(args, map[string]bool{"project": true}, taskFields)
	if err != nil {
		return nil, err
	}
	var project string
	if err := json.Unmarshal(rest["project"], &project); err != nil || project == "" {
		return nil, invalid("project is required")
	}
	b, err := a.bindProject(ctx, c, &project, nil)
	if err != nil {
		return nil, err
	}
	if err := a.taskRefs(ctx, c, fields); err != nil {
		return nil, err
	}
	mentions, err := a.mentionsOf(ctx, c, refsOf(fields["refs"]), project)
	if err != nil {
		return nil, err
	}
	out, err := a.V.TaskDeclare(ctx, c.verbs(), verbs.TaskDeclareIn{Project: project, Fields: fields, Mentions: mentions})
	if err != nil {
		return nil, err
	}
	b.Warnings = append(b.Warnings, out.Warnings...)
	return struct {
		verbs.TaskDeclareOut
		Binding
	}{out, b}, nil
}

func taskAmend(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
	amendable := map[string]bool{"resolution": true}
	for k := range taskFields {
		amendable[k] = true
	}
	rest, fields, err := splitFields(args, map[string]bool{"task_id": true, "project": true, "num": true, "if_revision": true}, amendable)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(rest)
	var in struct {
		taskRef
		IfRevision *int `json:"if_revision,omitempty"`
	}
	if err := decode(b, &in); err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, invalid("nothing to change: pass at least one field")
	}
	id, bind, err := a.bindTask(ctx, c, in.taskRef)
	if err != nil {
		return nil, err
	}
	if err := a.taskRefs(ctx, c, fields); err != nil {
		return nil, err
	}
	var mentions []verbs.EntityRef
	if v, ok := fields["refs"]; ok {
		var project string
		if err := a.DB.QueryRow(ctx, `SELECT p.name FROM projection.task t JOIN projection.project p ON p.project_id = t.project_id
		        WHERE t.task_id = $1`, id).Scan(&project); err != nil {
			return nil, err
		}
		if mentions, err = a.mentionsOf(ctx, c, refsOf(v), project); err != nil {
			return nil, err
		}
	}
	ack, err := a.V.TaskAmend(ctx, c.verbs(), verbs.TaskAmendIn{TaskID: id, IfRevision: in.IfRevision, Fields: fields, Mentions: mentions})
	bind.Warnings = append(bind.Warnings, ack.Warnings...)
	return writeAck{ack, bind}, err
}

// ---------- session verbs ----------

func sessionOpen(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
	var in struct {
		Project    string          `json:"project"`
		Attach     string          `json:"attach"`
		AutoCreate bool            `json:"auto_create,omitempty"`
		Agent      string          `json:"agent,omitempty"`
		RunID      *uuid.UUID      `json:"run_id,omitempty"`
		Client     json.RawMessage `json:"client,omitempty"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	if in.Attach == "" {
		return nil, invalid(`attach is required: the id of a session you opened before, or "none"`)
	}
	if err := checkAgent(in.Agent); err != nil {
		return nil, err
	}
	slug, id, err := a.project(ctx, c, in.Project)
	created := false
	if errors.As(err, new(*Error)) && in.AutoCreate && apiCode(err) == "project_not_found" {
		if slug, created, err = a.declareFromTitle(ctx, c, in.Project, nil); err == nil {
			_, id, err = a.project(ctx, c, slug)
		}
	}
	if err != nil {
		return nil, err
	}
	out, err := a.V.ContextOpen(ctx, c.verbs(), verbs.ContextOpenIn{Project: slug, Attach: in.Attach, RunID: in.RunID,
		Client: c.clientInfo(in.Client, in.Agent)})
	if err != nil {
		return nil, err
	}
	set, err := vocab.ProjectSet(ctx, a.DB, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"session": SessionRef{SessionID: out.ContextID, Project: slug, Reused: out.Attached},
		"attached": out.Attached, "attach_miss": out.AttachMiss, "project_created": created, "vocabulary": set.Block(),
		"rules": rulesBlock()}, nil
}

// readTypes: the type filter of a recall read. family must be a family. A type is resolved
// against the project's set the way a write would store it; a short or old name then matches both the stored name and
// the name as given (records written before, or through the facade 1.19, keep it). Without a project, or for a type
// the set does not resolve (unknown, outcome), the type is matched as given; an ambiguous short name is refused.
func (a *API) readTypes(ctx context.Context, c *Caller, project, typ, family string) ([]string, error) {
	if family != "" && !vocab.IsFamily(family) {
		return nil, invalid("family is one of %s", strings.Join(vocab.Families(), ", "))
	}
	if typ == "" || project == "" {
		return nil, nil
	}
	_, id, err := a.project(ctx, c, project)
	if err != nil {
		return nil, err
	}
	set, err := vocab.ProjectSet(ctx, a.DB, id)
	if err != nil {
		return nil, err
	}
	r, err := set.Resolve(typ)
	var ve *vocab.Error
	switch {
	case errors.As(err, &ve) && ve.Code == "type_ambiguous":
		return nil, errorf(ve.Code, "%s", ve.Error())
	case err != nil || r.Type == typ:
		return nil, nil
	case r.Legacy && !strings.Contains(r.Type, "."):
		// an old name that landed on a general type (discovery → finding; bugfix → event without dev) would
		// widen the filter to the whole family; the name as given is what such records keep
		return nil, nil
	}
	return []string{r.Type, typ}, nil
}

func apiCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// mySession: the session a checkpoint or close is about — the given one, else the only open session of
// this client (in the project, when one is named).
func (a *API) mySession(ctx context.Context, c *Caller, explicit *uuid.UUID, project string) (*openSession, error) {
	if explicit != nil {
		s, err := a.sessionByID(ctx, c, *explicit)
		if err != nil {
			return nil, err
		}
		if s == nil {
			return nil, errorf("session_not_found", "session %s", *explicit)
		}
		return s, nil
	}
	g := c.grant()
	if g == "" {
		return nil, invalid("session_id is required for this token")
	}
	var a1 args
	where := "x.owner_id = " + a1.add(c.Owner) + " AND x.closed_at IS NULL AND x.client->>'grant' = " + a1.add(g)
	if project != "" {
		_, pid, err := a.project(ctx, c, project)
		if err != nil {
			return nil, err
		}
		where += " AND x.project_id = " + a1.add(pid)
	}
	rows, err := a.DB.Query(ctx, `SELECT x.context_id, x.run_id, x.project_id, x.opened_at, p.name
	   FROM projection.context x JOIN projection.project p ON p.project_id = x.project_id WHERE `+where+` ORDER BY x.opened_at DESC LIMIT 5`, a1...)
	if err != nil {
		return nil, err
	}
	open, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (openSession, error) {
		var s openSession
		return s, r.Scan(&s.id, &s.run, &s.project, &s.opened, &s.projName)
	})
	if err != nil {
		return nil, err
	}
	switch len(open) {
	case 0:
		return nil, &Error{Code: "session_not_found", Message: "no open session of this client", Hint: "call project_resume first, or pass session_id"}
	case 1:
		return &open[0], nil
	}
	return nil, &Error{Code: "invalid_input", Message: "several open sessions of this client", Hint: "pass session_id or project"}
}

func sessionCheckpoint(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
	var in struct {
		SessionID *uuid.UUID `json:"session_id,omitempty"`
		Project   string     `json:"project,omitempty"`
		Summary   string     `json:"summary"`
		Current   *string    `json:"current,omitempty"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	s, err := a.mySession(ctx, c, in.SessionID, in.Project)
	if err != nil {
		return nil, err
	}
	ack, err := a.V.ContextCheckpoint(ctx, c.verbs(), s.id, in.Summary, in.Current)
	return writeAck{ack, Binding{SessionID: &s.id, Inherited: in.SessionID == nil}}, err
}

func sessionClose(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
	var in struct {
		SessionID *uuid.UUID   `json:"session_id,omitempty"`
		Project   string       `json:"project,omitempty"`
		Summary   string       `json:"summary"`
		FinishRun *bool        `json:"finish_run,omitempty"`
		Outcomes  []outcomeArg `json:"outcomes,omitempty"`
		NextSteps []string     `json:"next_steps,omitempty"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	s, err := a.mySession(ctx, c, in.SessionID, in.Project)
	if err != nil {
		return nil, err
	}
	outcomes, warnings, err := a.resolveOutcomes(ctx, c, in.Outcomes, s.projName)
	if err != nil {
		return nil, err
	}
	out, err := a.V.ContextCloseWith(ctx, c.verbs(), s.id, verbs.CloseIn{Summary: in.Summary, FinishRun: in.FinishRun,
		Outcomes: outcomes, NextSteps: in.NextSteps})
	if err != nil {
		return nil, err
	}
	return struct {
		verbs.ContextCloseOut
		Session  SessionRef `json:"session"`
		Warnings []string   `json:"warnings,omitempty"`
	}{out, SessionRef{SessionID: s.id, Project: s.projName}, warnings}, nil
}

func sessionList(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error) {
	var in struct {
		Project string `json:"project,omitempty"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	if in.Project != "" {
		if err := a.slugOf(ctx, c, &in.Project); err != nil {
			return nil, err
		}
	}
	rows, err := a.Q.ContextList(ctx, c.query(), in.Project)
	if err != nil {
		return nil, err
	}
	type item struct {
		SessionID   uuid.UUID        `json:"session_id"`
		Project     string           `json:"project"`
		OpenedAt    time.Time        `json:"opened_at"`
		LastEventAt time.Time        `json:"last_event_at"`
		Client      json.RawMessage  `json:"client,omitempty"`
		Mine        bool             `json:"mine"`
		Provenance  query.Provenance `json:"provenance"`
	}
	items := []item{}
	g := c.grant()
	for _, r := range rows {
		var cl struct {
			Grant string `json:"grant"`
		}
		_ = json.Unmarshal(r.Client, &cl)
		items = append(items, item{r.ContextID, r.Project, r.OpenedAt, r.LastEventAt, r.Client, g != "" && cl.Grant == g, r.Provenance})
	}
	return map[string]any{"items": items}, nil
}

// capabilities: system.capabilities plus what this connection may do.
func (a *API) capabilities(c *Caller) (any, error) {
	doc, err := Surface()
	if err != nil {
		return nil, err
	}
	base := a.Q.Capabilities()
	tools := []string{}
	for _, t := range doc.Tools {
		if t.Visible(c.Role, c.profile()) {
			tools = append(tools, t.Name)
		}
	}
	return map[string]any{
		"service": "Litopys", "api_version": doc.APIVersion, "surface_hash": doc.Hash,
		"spec_version": base.SpecVersion, "envelope_version": base.EnvelopeVersion, "slices": base.Slices,
		"modules": base.Modules, "facades": base.Facades,
		"guide": map[string]any{"tool": "system_guide", "version": Guide.Version, "topics": Guide.Order},
		"connection": map[string]any{"role": c.Role, "profile": c.profile(), "vendor": c.Grant.Vendor, "oauth": c.OAuth,
			"read_only": c.profile() == policy.ProfileReadonly, "tools": tools,
			"destructive_tools": destructive(doc, c)},
	}, nil
}

func destructive(doc SurfaceDoc, c *Caller) []string {
	out := []string{}
	for _, t := range doc.Tools {
		if t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint && t.Visible(c.Role, c.profile()) {
			out = append(out, t.Name)
		}
	}
	return out
}

// args builds positional parameters: add returns "$n".
type args []any

func (a *args) add(v any) string { *a = append(*a, v); return "$" + strconv.Itoa(len(*a)) }
