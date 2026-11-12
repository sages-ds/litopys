package verbs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/cron"
	"litopys.smart-gw.co.ua/internal/pg"
)

// StartScheduled starts the workflows whose schedule trigger names this minute: a daily digest,
// a weekly review. Once per workflow and minute — a second worker or a repeated pass in the same minute starts
// nothing; the run belongs to the workflow's owner, as if they started it. It returns the runs started.
func (s *Service) StartScheduled(ctx context.Context, tenant uuid.UUID, now time.Time) ([]string, error) {
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	rows, err := s.DB.Query(sys, `
		SELECT w.workflow_id, w.owner_id, p.name, w.key, v.definition->'triggers'
		  FROM projection.workflow w
		  JOIN projection.workflow_version v ON v.workflow_id = w.workflow_id AND v.version = w.version
		  JOIN projection.project p ON p.project_id = w.project_id
		 WHERE w.tenant_id = $1 AND NOT w.retired AND p.status = 'active'
		   AND v.definition->'triggers' @> '[{"kind": "schedule"}]'`, tenant)
	if err != nil {
		return nil, err
	}
	type due struct {
		id           uuid.UUID
		owner        uuid.UUID
		project, key string
	}
	var list []due
	for rows.Next() {
		var d due
		var raw []byte
		if err := rows.Scan(&d.id, &d.owner, &d.project, &d.key, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var triggers []WorkflowTrigger
		_ = json.Unmarshal(raw, &triggers)
		for _, t := range triggers {
			if t.Kind != "schedule" || t.Spec == nil {
				continue
			}
			if sp, err := cron.Parse(*t.Spec); err == nil && sp.Due(now) {
				list = append(list, d)
				break
			}
		}
	}
	rows.Close()
	var started []string
	for _, d := range list {
		// once per minute by the database clock: another worker, or this one again, within the minute starts nothing
		var already bool
		if err := s.DB.QueryRow(sys, `SELECT EXISTS (SELECT 1 FROM projection.execution WHERE workflow_id = $1
		        AND started_at > now() - interval '50 seconds')`, d.id).Scan(&already); err != nil {
			return started, err
		}
		if already {
			continue
		}
		out, err := s.ExecutionStart(ctx, Principal{TenantID: tenant, ActorID: d.owner}, ExecutionStartIn{Project: d.project, Workflow: d.key})
		if err != nil {
			return started, fmt.Errorf("%s/%s: %w", d.project, d.key, err)
		}
		started = append(started, out.Ref)
	}
	return started, nil
}
