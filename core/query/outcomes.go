package query

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Outcomes of subjects: statements of type outcome with their structure (statement.outcome, 0029).
// Only current, unpurged outcomes of the principal's own records are read.

// OutcomeRow is read-models.yaml outcomes/row.
type OutcomeRow struct {
	StatementID uuid.UUID  `json:"statement_id"`
	Project     string     `json:"project"`
	Ref         string     `json:"ref,omitempty"`
	SubjectType string     `json:"subject_type,omitempty"`
	SubjectID   *uuid.UUID `json:"subject_id,omitempty"`
	Topic       string     `json:"topic,omitempty"`
	Result      string     `json:"result"`
	State       string     `json:"state"`
	Next        string     `json:"next,omitempty"`
	Evidence    []string   `json:"evidence,omitempty"`
	EffectiveAt time.Time  `json:"effective_at"`
	SessionID   *uuid.UUID `json:"session_id,omitempty"`
}

const outcomeCols = `s.statement_id, p.name, s.title, s.outcome, s.effective_at, s.context_id`

// outcomeWhere: the live outcomes of the principal ($1 root actor).
var outcomeWhere = `s.type = 'outcome' AND s.outcome IS NOT NULL AND s.status = 'current' AND NOT s.hidden AND ` + mineSQL("s.actor_id", "$1")

func (s *Service) outcomes(ctx context.Context, p Principal, where string, limit int, params ...any) ([]OutcomeRow, error) {
	ctx, root, err := s.enter(ctx, p)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT `+outcomeCols+`
	  FROM projection.statement s JOIN projection.project p ON p.project_id = s.project_id
	 WHERE `+outcomeWhere+` AND `+where+`
	 ORDER BY s.effective_at DESC, s.asserted_seq DESC LIMIT `+strconv.Itoa(clamp(limit, 1, 1, 50)), append([]any{root}, params...)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (OutcomeRow, error) {
		var x OutcomeRow
		var raw []byte
		if err := r.Scan(&x.StatementID, &x.Project, &x.Result, &raw, &x.EffectiveAt, &x.SessionID); err != nil {
			return x, err
		}
		var o struct {
			Ref         string     `json:"ref"`
			SubjectType string     `json:"subject_type"`
			SubjectID   *uuid.UUID `json:"subject_id"`
			Topic       string     `json:"topic"`
			State       string     `json:"state"`
			Next        string     `json:"next"`
			Evidence    []string   `json:"evidence"`
		}
		if err := json.Unmarshal(raw, &o); err != nil {
			return x, err
		}
		x.Ref, x.SubjectType, x.SubjectID, x.Topic, x.State, x.Next, x.Evidence = o.Ref, o.SubjectType, o.SubjectID, o.Topic, o.State, o.Next, o.Evidence
		return x, nil
	})
}

func firstOutcome(rows []OutcomeRow, err error) (*OutcomeRow, error) {
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// LatestOutcome: the latest outcome about a subject (a statement or a task), nil when none.
func (s *Service) LatestOutcome(ctx context.Context, p Principal, subject uuid.UUID) (*OutcomeRow, error) {
	return firstOutcome(s.outcomes(ctx, p, `s.outcome->>'subject_id' = $2`, 1, subject.String()))
}

// TopicOutcome: the latest outcome of a topic label (topic:<name>), compared case-insensitively.
func (s *Service) TopicOutcome(ctx context.Context, p Principal, topic string) (*OutcomeRow, error) {
	return firstOutcome(s.outcomes(ctx, p, `lower(s.outcome->>'topic') = $2`, 1, strings.ToLower(strings.TrimSpace(topic))))
}

// OpenOutcomes: per subject of the project, its latest outcome when that is not done — what is left hanging.
func (s *Service) OpenOutcomes(ctx context.Context, p Principal, projectID uuid.UUID, limit int) ([]OutcomeRow, error) {
	rows, err := s.outcomes(ctx, p, `s.project_id = $2`, 50, projectID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []OutcomeRow{}
	for _, r := range rows { // newest first: the first row of a subject is its latest outcome
		key := r.Topic
		if r.SubjectID != nil {
			key = r.SubjectID.String()
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if r.State != "done" && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

// SessionOutcomes: the outcomes a session's close wrote.
func (s *Service) SessionOutcomes(ctx context.Context, p Principal, contextID uuid.UUID) ([]OutcomeRow, error) {
	return s.outcomes(ctx, p, `s.context_id = $2`, 10, contextID)
}
