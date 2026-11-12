package verbs

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Human confirmation of retention labels outside the chat (owner decision
// 2026-10-30). A label that erases (purge) or covers every record of a project (applies_to: record) is only proposed by
// fact.record — whoever writes it, the main assistant of a person included; the worker applies it once the person
// confirmed that very version in the viewer. The viewer is the only caller of FactConfirm.

// NeedsConfirmation is the warning code of fact.record for a label the worker will not apply before the confirmation.
const NeedsConfirmation = "retention_needs_confirmation"

// HumanOnly: the label is one only a person decides (purge, or applies_to: record).
func (r Retention) HumanOnly() bool { return r.humanOnly() }

// ConfirmURL is the viewer page where the person confirms the label key of a project.
func ConfirmURL(project, key string) string { return "/viewer/confirm/" + project + "/" + key }

// needsConfirmation is the warning fact.record gives for a human-only label: who confirms it and where.
func needsConfirmation(project, key string) string {
	return NeedsConfirmation + ": the label is proposed; ask the user to confirm it in the Litopys viewer at " +
		ConfirmURL(project, key) + " — the worker does not apply it before"
}

// RetentionImpact is what a label covers in its first pass (the person sees it before the GO).
type RetentionImpact struct {
	Records int `json:"records"`
	Tasks   int `json:"tasks"`
}

// RetentionLabel is a retention fact as the confirmation page shows it.
type RetentionLabel struct {
	ProjectID     uuid.UUID       `json:"-"`
	Project       string          `json:"project"`
	Key           string          `json:"key"`
	Value         string          `json:"value"`
	RecordEventID uuid.UUID       `json:"record_event_id"`
	Confirmed     *time.Time      `json:"confirmed_at,omitempty"`
	HumanOnly     bool            `json:"human_only"`
	Impact        RetentionImpact `json:"impact"`
}

// RetentionLabelOf reads a live retention label of the principal's project with its first-pass impact (at most 500 a
// kind, as the worker's pass). Read-only.
func (s *Service) RetentionLabelOf(ctx context.Context, p Principal, project, key string, now time.Time) (RetentionLabel, error) {
	l := RetentionLabel{Project: project, Key: key}
	var value *string
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		id, err := c.projectID(project)
		if err != nil {
			return err
		}
		l.ProjectID = id
		err = c.tx.QueryRow(c.ctx, `SELECT value, last_event_id, confirmed_at FROM projection.fact
		        WHERE project_id = $1 AND kind = 'retention' AND key = $2 AND NOT retired`, id, key).Scan(&value, &l.RecordEventID, &l.Confirmed)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail("fact_not_found", "no live retention label %q in project %s", key, project)
		}
		return err
	})
	if err != nil {
		return l, err
	}
	r, err := ParseRetention(value)
	if err != nil {
		return l, err
	}
	l.Value, l.HumanOnly = *value, r.humanOnly()
	zone, err := time.LoadLocation(RetentionZone)
	if err != nil {
		return l, err
	}
	if cut, ok := r.cutoff(now, zone); ok {
		targets, err := s.retentionTargets(ctx, l.ProjectID, r, cut)
		if err != nil {
			return l, err
		}
		for _, t := range targets {
			if t.EntityType == "task" {
				l.Impact.Tasks++
			} else {
				l.Impact.Records++
			}
		}
	}
	return l, nil
}

// FactConfirm records the person's confirmation of the version recordEventID of a human-only retention label.
// Refused: from anyone but a person (retention_confirm_requires_human), for a label that needs none, for another
// version than the one shown (label_changed), for an impact that changed since it was shown (impact_changed: the
// page shows the new one). A label already confirmed in this version answers already.
func (s *Service) FactConfirm(ctx context.Context, p Principal, project, key string, recordEventID uuid.UUID, shown RetentionImpact, now time.Time) (Ack, error) {
	l, err := s.RetentionLabelOf(ctx, p, project, key, now)
	if err != nil {
		return Ack{}, err
	}
	if !l.HumanOnly {
		return Ack{}, invalid("key", "label %q needs no confirmation: the worker applies it as it is", key)
	}
	if l.RecordEventID != recordEventID {
		return Ack{}, fail("label_changed", "label %q was written again after the page was shown: review the new version", key)
	}
	if l.Impact != shown {
		return Ack{}, fail("impact_changed", "label %q now covers %d records and %d tasks: review the new impact", key, l.Impact.Records, l.Impact.Tasks)
	}
	if l.Confirmed != nil {
		return Ack{Already: true}, nil
	}
	var a Ack
	err = s.exec(ctx, s.DB, p, func(c *cmd) error {
		kind, err := c.actorKind()
		if err != nil {
			return err
		}
		if kind != "human" {
			return fail("retention_confirm_requires_human", "only the person confirms a retention label, not an %s", kind)
		}
		var current uuid.UUID
		if err := c.tx.QueryRow(c.ctx, `SELECT last_event_id FROM projection.fact WHERE project_id = $1 AND kind = 'retention'
		        AND key = $2 AND NOT retired FOR UPDATE`, l.ProjectID, key).Scan(&current); err != nil {
			return err
		}
		if current != recordEventID { // written again between the read above and this lock
			return fail("label_changed", "label %q was written again after the page was shown: review the new version", key)
		}
		e, err := c.emit("fact.confirmed", "fact", FactID(l.ProjectID, "retention", key), nil, nil, map[string]any{
			"record_event_id": recordEventID, "channel": "viewer", "impact": map[string]int{"records": shown.Records, "tasks": shown.Tasks}})
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return a, err
}
