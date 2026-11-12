package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// WorkflowRow is read-models.yaml workflows.row (0020).
type WorkflowRow struct {
	WorkflowID uuid.UUID  `json:"workflow_id"`
	Ref        string     `json:"ref"` // <project>/W-<num>
	Project    string     `json:"project"`
	Key        string     `json:"key"`
	Title      string     `json:"title"`
	Purpose    *string    `json:"purpose,omitempty"`
	Version    int        `json:"version"`
	Steps      int        `json:"steps"` // of the current version
	Retired    bool       `json:"retired"`
	Revision   int        `json:"revision"`
	Provenance Provenance `json:"provenance"`
}

type WorkflowListIn struct {
	Project        string `json:"project"`
	IncludeRetired bool   `json:"include_retired,omitempty"`
}

type WorkflowListOut struct {
	Items []WorkflowRow `json:"items"`
}

func (s *Service) workflowRows(ctx context.Context, where string, a args) ([]WorkflowRow, error) {
	rows, err := s.DB.Query(ctx, `SELECT w.workflow_id, pr.name || '/W-' || w.num, pr.name, w.key, w.title, w.purpose, w.version, v.steps, w.retired, w.revision, `+provCols("j")+`
		  FROM projection.workflow w JOIN projection.project pr ON pr.project_id = w.project_id
		  JOIN projection.workflow_version v ON v.workflow_id = w.workflow_id AND v.version = w.version`+
		provJoin("j", "w.tenant_id", "w.last_seq")+` WHERE `+where+` ORDER BY w.key`, a...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (WorkflowRow, error) {
		var x WorkflowRow
		return x, r.Scan(append([]any{&x.WorkflowID, &x.Ref, &x.Project, &x.Key, &x.Title, &x.Purpose, &x.Version, &x.Steps, &x.Retired, &x.Revision},
			x.Provenance.dest()...)...)
	})
}

// WorkflowList is workflow.list: processes of a project, retired ones on request.
func (s *Service) WorkflowList(ctx context.Context, p Principal, in WorkflowListIn) (WorkflowListOut, error) {
	out := WorkflowListOut{Items: []WorkflowRow{}}
	ctx, _, err := s.enter(ctx, p)
	if err != nil {
		return out, err
	}
	projectID, err := s.projectID(ctx, p, in.Project)
	if err != nil {
		return out, err
	}
	var a args
	where := "w.project_id = " + a.add(projectID)
	if !in.IncludeRetired {
		where += " AND NOT w.retired"
	}
	items, err := s.workflowRows(ctx, where, a)
	if items != nil {
		out.Items = items
	}
	return out, err
}

type WorkflowReadIn struct {
	Project string `json:"project"`
	Key     string `json:"key"`
	Version *int   `json:"version,omitempty"`
	Format  string `json:"format,omitempty"` // json (default) | skill
}

type WorkflowReadOut struct {
	Workflow   WorkflowRow     `json:"workflow"`
	Version    int             `json:"version"`
	Definition json.RawMessage `json:"definition"`
	Reason     *string         `json:"reason,omitempty"`
	SkillMD    *string         `json:"skill_md,omitempty"`
}

// WorkflowRead is workflow.read: the definition of the current or a past version, optionally as SKILL.md.
func (s *Service) WorkflowRead(ctx context.Context, p Principal, in WorkflowReadIn) (WorkflowReadOut, error) {
	var out WorkflowReadOut
	switch in.Format {
	case "", "json", "skill":
	default:
		return out, fail("invalid_input", "format: json | skill")
	}
	ctx, _, err := s.enter(ctx, p)
	if err != nil {
		return out, err
	}
	projectID, err := s.projectID(ctx, p, in.Project)
	if err != nil {
		return out, err
	}
	var a args
	rows, err := s.workflowRows(ctx, "w.project_id = "+a.add(projectID)+" AND w.key = "+a.add(in.Key), a)
	if err != nil {
		return out, err
	}
	if len(rows) == 0 {
		return out, fail("workflow_not_found", "workflow %q in project %q", in.Key, in.Project)
	}
	out.Workflow = rows[0]
	out.Version = out.Workflow.Version
	if in.Version != nil {
		out.Version = *in.Version
	}
	err = s.DB.QueryRow(ctx, `SELECT definition, reason FROM projection.workflow_version WHERE workflow_id = $1 AND version = $2`,
		out.Workflow.WorkflowID, out.Version).Scan(&out.Definition, &out.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, fail("version_not_found", "workflow %q has versions 1..%d", in.Key, out.Workflow.Version)
	}
	if err != nil {
		return out, err
	}
	if in.Format == "skill" {
		md, err := skillMD(out.Workflow, out.Version, out.Definition)
		if err != nil {
			return out, err
		}
		out.SkillMD = &md
	}
	return out, nil
}

// skillMD renders a version as an Agent Skills SKILL.md: metadata in the front matter, the steps
// as instructions. It is an export for agent clients; the definition in Litopys stays the source.
func skillMD(w WorkflowRow, version int, def json.RawMessage) (string, error) {
	var d struct {
		Steps []struct {
			Key              string   `json:"key"`
			Title            string   `json:"title"`
			Kind             string   `json:"kind"`
			ExecutorClass    *string  `json:"executor_class"`
			Delegable        *bool    `json:"delegable"`
			WhoMayRun        *string  `json:"who_may_run"`
			Verbs            []string `json:"verbs"`
			WriteMode        *string  `json:"write_mode"`
			MaxDataClass     *string  `json:"max_data_class"`
			RequiresApproval *bool    `json:"requires_approval"`
			Condition        *string  `json:"condition"`
			Acceptance       *string  `json:"acceptance"`
			External         *struct {
				System *string `json:"system"`
				Ref    *string `json:"ref"`
			} `json:"external"`
			Subworkflow *string `json:"subworkflow"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(def, &d); err != nil {
		return "", err
	}
	desc := w.Title
	if w.Purpose != nil && *w.Purpose != "" {
		desc = *w.Purpose
	}
	var b strings.Builder
	fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\nmetadata:\n  litopys-project: %s\n  litopys-version: %d\n---\n\n",
		w.Key, oneLine(desc), w.Project, version)
	fmt.Fprintf(&b, "# %s\n\nVersion %d of the process `%s` (Litopys). Do the steps in order; a step with a condition runs only when it holds.\n\n",
		w.Title, version, w.Key)
	for i, s := range d.Steps {
		fmt.Fprintf(&b, "## %d. %s (`%s`, %s)\n\n", i+1, s.Title, s.Key, s.Kind)
		var notes []string
		if s.Condition != nil {
			notes = append(notes, "only when `"+*s.Condition+"`")
		}
		if s.RequiresApproval != nil && *s.RequiresApproval {
			notes = append(notes, "wait for the user's explicit GO before this step")
		}
		if s.WhoMayRun != nil && *s.WhoMayRun == "human_only" || s.Kind == "human" {
			notes = append(notes, "the user does this step; do not do it yourself")
		}
		if s.ExecutorClass != nil {
			notes = append(notes, "model class: "+*s.ExecutorClass)
		}
		if s.Delegable != nil && !*s.Delegable {
			notes = append(notes, "do not delegate to a sub-agent")
		}
		if len(s.Verbs) > 0 {
			notes = append(notes, "allowed tools: "+strings.Join(s.Verbs, ", "))
		}
		if s.WriteMode != nil && *s.WriteMode == "candidate" {
			notes = append(notes, "results are candidates for review, not final records")
		}
		if s.MaxDataClass != nil {
			notes = append(notes, "data class at most "+*s.MaxDataClass)
		}
		if s.External != nil && s.External.System != nil {
			ref := ""
			if s.External.Ref != nil {
				ref = " " + *s.External.Ref
			}
			notes = append(notes, "external job: "+*s.External.System+ref)
		}
		if s.Subworkflow != nil {
			notes = append(notes, "runs the process `"+*s.Subworkflow+"`")
		}
		for _, n := range notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
		if s.Acceptance != nil && *s.Acceptance != "" {
			fmt.Fprintf(&b, "\nDone when: %s\n", *s.Acceptance)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 1024 {
		s = string(r[:1023]) + "…"
	}
	return s
}
