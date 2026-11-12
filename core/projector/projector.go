// Package projector is the ONLY code that writes projection.* (invariant 2).
//
// Apply is one function, the same for the live write path and for replay, so a rebuild
// reproduces the live tables by construction (gate E7). Handlers live in handlers.go and
// follow the table "Що робить apply у зрізі 1" of the journal design.
package projector

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/journal"
)

// Version of the apply code, stored in projector_state.version. A binary starting with a
// different version treats the projections as stale and rebuilds them.
// Bump it whenever any handler changes what it writes.
const Version = "32" // 32: statement_search by textnorm v3, no legacy reference forms; 31: attachments (0049); 30: the source of a fact (0047); 29: structured fields of records (0046); 28: the work clock never runs ahead of the recording time; 27: the retention of one record, statement.retention_set (0043); 26: raw-event drafts leave the work clock; 25: legacy_type of imported records (0040); 24: confirmed_at of facts, fact.confirmed (0039); 23: record families, family from the event or the legacy map (0038); 22: number floors, numbering.reserved (0037); 21: deletion proposals D-N (0035); 20: quarantine, statement/task hidden (0034); 19: purge of a task, task.purged (0033); 18: refs in statement_version (0032); 17: statement refs (0031); 16: entity numbers R/W/X (0030); 2: search_vocab projected; 3: timestamps from effective_at; 4: tasks = todo v2 (0005); 5: vocab from title+body; 6: owner_id (0008); 7: policy + project labels (0010); 8: session opens do not touch project.updated_at; 9: work clock — amends move updated_at, retract/reinstate/supersede and project transitions do not; 10: project title (0014); 11: statement_search via core/textnorm v1 (0025); 12: textnorm v2; 13: title idents and stems in statement_search (0026); 14: identifier counts ident_tf (0027); 15: stem_vocab for the typo channel (0028)

var (
	// ErrUnknownType: the event type has no handler — a new type reached the journal before the projector.
	ErrUnknownType = errors.New("projector: unknown event type")
	// ErrOutOfOrder: the event is not the next seq after projector_state.last_seq.
	ErrOutOfOrder = errors.New("projector: event out of order")
	// ErrMissing: the event refers to a row the projections do not have (wrong order or a bug).
	ErrMissing = errors.New("projector: referenced row missing")
)

type handler func(ctx context.Context, tx pgx.Tx, e *journal.Event) error

// handlers. Keep in sync with domain/events.yaml
// (TestHandlersMatchDomainSliceOne fails when they diverge).
var handlers = map[string]handler{
	"actor.registered":        actorRegistered,
	"actor.revoked":           actorRevoked,
	"actor.amended":           actorAmended,
	"run.started":             runStarted,
	"run.finished":            runFinished,
	"context.opened":          contextOpened,
	"context.closed":          contextClosed,
	"context.checkpointed":    contextCheckpointed,
	"statement.asserted":      statementAsserted,
	"statement.amended":       statementAmended,
	"statement.retracted":     statementRetracted,
	"statement.reinstated":    statementReinstated,
	"statement.retention_set": statementRetentionSet,
	"statement.superseded":    statementSuperseded,
	"link.asserted":           linkAsserted,
	"link.retired":            linkRetired,
	"project.declared":        projectDeclared,
	"project.amended":         projectAmended,
	"project.transitioned":    projectTransitioned,
	"task.declared":           taskDeclared,
	"task.amended":            taskAmended,
	"task.closed":             taskClosed,
	"task.reopened":           taskReopened,
	"task.retracted":          taskRetracted,
	"fact.recorded":           factRecorded,
	"attachment.added":        attachmentAdded,
	"fact.retired":            factRetired,
	"fact.confirmed":          factConfirmed,
	"consumer.cursor_set":     consumerCursorSet,
	"journal.purged":          journalPurged,
	"journal.quarantined":     journalQuarantined, // 0034
	"journal.released":        journalReleased,
	"deletion.proposed":       deletionProposed, // 0035
	"deletion.approved":       deletionApproved,
	"deletion.rejected":       deletionRejected,
	"deletion.executed":       deletionExecuted,
	"numbering.reserved":      numberingReserved, // 0037
	"policy.set":              policySet,
	"workflow.declared":       workflowDeclared, // 0020
	"workflow.amended":        workflowAmended,
	"workflow.superseded":     workflowSuperseded,
	"workflow.retired":        workflowRetired,
	"execution.started":       executionStarted, // 0021
	"execution.closed":        executionClosed,
	"step.started":            stepStarted,
	"step.checkpointed":       stepCheckpointed,
	"step.finished":           stepFinished,
	"approval.granted":        approvalGranted,
	"approval.revoked":        approvalRevoked,
	"step.delegated":          stepDelegated, // 0022
}

// Known reports whether the projector has a handler for the type.
func Known(eventType string) bool { _, ok := handlers[eventType]; return ok }

// Apply projects one event inside the caller's transaction and advances projector_state.
//
// Exactly-once: an event with seq <= projector_state.last_seq is skipped (already applied);
// any other seq than last_seq+1 is ErrOutOfOrder. The live path calls Apply right after
// journal.Append in the same tx; Rebuild calls it for every event in seq order.
func Apply(ctx context.Context, tx pgx.Tx, e *journal.Event) error {
	h, ok := handlers[e.Type]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownType, e.Type)
	}
	var last int64
	err := tx.QueryRow(ctx,
		`SELECT last_seq FROM projection.projector_state WHERE tenant_id = $1 FOR UPDATE`, e.TenantID).Scan(&last)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx,
			`INSERT INTO projection.projector_state (tenant_id, last_seq, version) VALUES ($1, 0, $2)`,
			e.TenantID, Version); err != nil {
			return fmt.Errorf("projector_state init: %w", err)
		}
	case err != nil:
		return fmt.Errorf("projector_state: %w", err)
	}
	if e.Seq <= last {
		return nil
	}
	if e.Seq != last+1 {
		return fmt.Errorf("%w: seq %d after %d", ErrOutOfOrder, e.Seq, last)
	}
	// Row-level security of 0008: projections accept writes only in the projector scope, and the
	// projector reads rows of every owner (links, counters). The caller's scope comes back after.
	var prev string
	if err := tx.QueryRow(ctx, `SELECT litopys_scope()`).Scan(&prev); err != nil {
		return fmt.Errorf("projector scope: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('litopys.scope', 'projector', true)`); err != nil {
		return fmt.Errorf("projector scope: %w", err)
	}
	if err := h(ctx, tx, e); err != nil {
		return fmt.Errorf("apply %s (seq %d): %w", e.Type, e.Seq, err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('litopys.scope', $1, true)`, prev); err != nil {
		return fmt.Errorf("projector scope: %w", err)
	}
	// The version is not touched here: the projections were built by whatever code built them, and only a
	// fresh state (above) or a finished Rebuild may say they match this code. Otherwise the first write of a new
	// binary would mark old projections current, and an interrupted rebuild would look finished.
	_, err = tx.Exec(ctx,
		`UPDATE projection.projector_state SET last_seq = $2 WHERE tenant_id = $1`, e.TenantID, e.Seq)
	return err
}
