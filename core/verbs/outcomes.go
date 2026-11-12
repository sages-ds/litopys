package verbs

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/privacy"
)

// Outcomes of a session per subject: session_close writes one statement of type outcome per subject
// the session moved — what came of it, its state, the next step — with an about link to the subject. The caller
// resolves the textual ref (R-498, T-12, <project>/T-12, topic:<name>) to Subject or Topic; ownership is checked here.

// MaxOutcomes caps the outcomes of one close.
const MaxOutcomes = 10

var outcomeStates = []string{"done", "partial", "blocked", "failed"}

// OutcomeIn is one subject's outcome; exactly one of Subject and Topic is set.
type OutcomeIn struct {
	Ref      string
	Subject  *EntityRef
	Topic    string
	Result   string
	State    string
	Next     *string
	Evidence []string
}

// OutcomeOut names the statement written for a ref.
type OutcomeOut struct {
	Ref         string    `json:"ref"`
	StatementID uuid.UUID `json:"statement_id"`
}

// CloseIn is session_close with its outcomes and next steps.
type CloseIn struct {
	Summary   string
	FinishRun *bool
	Outcomes  []OutcomeIn
	NextSteps []string
}

func oneLine(s string) string {
	return privacy.MaskTitle(privacy.NormalizeOneLine(privacy.StripPrivate(s)))
}

// clean validates and masks the outcomes and next steps before the transaction.
func (in *CloseIn) clean() error {
	if len(in.Outcomes) > MaxOutcomes {
		return invalid("outcomes", "at most %d, one per subject", MaxOutcomes)
	}
	seen := map[string]bool{}
	for i := range in.Outcomes {
		o := &in.Outcomes[i]
		field := fmt.Sprintf("outcomes[%d]", i)
		if (o.Subject == nil) == (o.Topic == "") {
			return invalid(field+".ref", "a record, a task or topic:<name>")
		}
		key := o.Topic
		if o.Subject != nil {
			key = o.Subject.EntityID.String()
		}
		if seen[key] {
			return invalid(field+".ref", "%s: one outcome per subject in a session", o.Ref)
		}
		seen[key] = true
		if err := checkEnum(field+".state", o.State, outcomeStates...); err != nil {
			return err
		}
		o.Result = oneLine(o.Result)
		if err := checkLen(field+".result", o.Result, 1, 300); err != nil {
			return err
		}
		if o.Next != nil {
			n := oneLine(*o.Next)
			o.Next = &n
			if n == "" {
				o.Next = nil
			}
		}
		if o.State != "done" && o.Next == nil {
			return invalid(field+".next", "%s is %s: say what comes next", o.Ref, o.State)
		}
		if err := optLen(field+".next", o.Next, 300); err != nil {
			return err
		}
		if len(o.Evidence) > 5 {
			return invalid(field+".evidence", "at most 5")
		}
		for j := range o.Evidence {
			o.Evidence[j] = oneLine(o.Evidence[j])
			if err := checkLen(fmt.Sprintf("%s.evidence[%d]", field, j), o.Evidence[j], 1, 300); err != nil {
				return err
			}
		}
		if err := checkLen(field+".topic", o.Topic, 0, 100); err != nil {
			return err
		}
	}
	if len(in.NextSteps) > 5 {
		return invalid("next_steps", "at most 5")
	}
	for i := range in.NextSteps {
		in.NextSteps[i] = oneLine(in.NextSteps[i])
		if err := checkLen(fmt.Sprintf("next_steps[%d]", i), in.NextSteps[i], 1, 300); err != nil {
			return err
		}
	}
	return nil
}

// subjectProject: the project of an own subject — a live statement or task of the principal's human.
func (c *cmd) subjectProject(r EntityRef) (string, error) {
	var name string
	switch r.EntityType {
	case "statement":
		if _, _, err := c.statement(r.EntityID); err != nil {
			return "", err
		}
		err := c.tx.QueryRow(c.ctx, `SELECT p.name FROM projection.statement s JOIN projection.project p ON p.project_id = s.project_id
		        WHERE s.statement_id = $1`, r.EntityID).Scan(&name)
		return name, err
	case "task":
		err := c.tx.QueryRow(c.ctx, `SELECT pr.name FROM projection.task t JOIN projection.project pr ON pr.project_id = t.project_id
		        WHERE t.task_id = $1 AND pr.owner_id = $2 AND NOT t.hidden`, r.EntityID, c.me).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fail("task_not_found", "task %s", r.EntityID)
		}
		return name, err
	}
	return "", invalid("ref", "an outcome is about a record or a task")
}

// writeOutcome asserts the outcome statement in the session's context and links it to its subject.
func (c *cmd) writeOutcome(contextID uuid.UUID, contextProject string, o OutcomeIn) (OutcomeOut, error) {
	project := contextProject
	if o.Subject != nil {
		var err error
		if project, err = c.subjectProject(*o.Subject); err != nil {
			return OutcomeOut{}, err
		}
	}
	structure := map[string]any{"ref": o.Ref, "state": o.State}
	lines := []string{"state: " + o.State}
	if o.Next != nil {
		structure["next"] = *o.Next
		lines = append(lines, "next: "+*o.Next)
	}
	if len(o.Evidence) > 0 {
		structure["evidence"] = o.Evidence
		lines = append(lines, "evidence: "+strings.Join(o.Evidence, "; "))
	}
	var concepts []string
	if o.Subject != nil {
		structure["subject_type"], structure["subject_id"] = o.Subject.EntityType, o.Subject.EntityID
	} else {
		structure["topic"] = o.Topic
		concepts = []string{o.Topic}
	}
	lines = append(lines, "subject: "+o.Ref)
	body := strings.Join(lines, "\n")
	var out StatementAssertOut
	if err := c.assert(StatementAssertIn{Project: project, Type: "outcome", Title: o.Result, Body: &body, Concepts: concepts,
		ContextID: &contextID, Outcome: structure}, &out); err != nil {
		return OutcomeOut{}, err
	}
	if o.Subject != nil {
		from := EntityRef{EntityType: "statement", EntityID: out.StatementID}
		if _, err := c.emit("link.asserted", "link", uuid.New(), nil, nil,
			map[string]any{"from": from, "to": *o.Subject, "relation": "about"}); err != nil {
			return OutcomeOut{}, err
		}
	}
	return OutcomeOut{Ref: o.Ref, StatementID: out.StatementID}, nil
}

// closeHints: tasks the run changed, left open and gave no outcome, and open tasks an outcome
// calls done (п.3) — hints, never a refusal.
func (c *cmd) closeHints(run uuid.UUID, contextProject string, outs []OutcomeIn) (missing, hints []string, err error) {
	about := map[uuid.UUID]OutcomeIn{}
	for _, o := range outs {
		if o.Subject != nil && o.Subject.EntityType == "task" {
			about[o.Subject.EntityID] = o
		}
	}
	rows, err := c.tx.Query(c.ctx, `SELECT t.task_id, pr.name, t.num, t.status FROM projection.task t
	        JOIN projection.project pr ON pr.project_id = t.project_id
	       WHERE pr.owner_id = $1 AND NOT t.hidden AND t.task_id IN
	             (SELECT DISTINCT j.entity_id FROM journal j WHERE j.tenant_id = $2 AND j.run_id = $3 AND j.entity_type = 'task')
	       ORDER BY pr.name, t.num`, c.me, c.p.TenantID, run)
	if err != nil {
		return nil, nil, err
	}
	type task struct {
		id      uuid.UUID
		project string
		num     int
		status  string
	}
	tasks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (task, error) {
		var t task
		return t, r.Scan(&t.id, &t.project, &t.num, &t.status)
	})
	if err != nil {
		return nil, nil, err
	}
	for _, t := range tasks {
		ref := fmt.Sprintf("T-%d", t.num)
		if t.project != contextProject {
			ref = t.project + "/" + ref
		}
		closed := t.status == "done" || t.status == "dropped"
		if _, ok := about[t.id]; !ok && !closed {
			missing = append(missing, ref)
		}
	}
	// a done outcome of a task still open: say so, change nothing
	for _, o := range outs {
		if o.State != "done" || o.Subject == nil || o.Subject.EntityType != "task" {
			continue
		}
		var status string
		if err := c.tx.QueryRow(c.ctx, `SELECT status FROM projection.task WHERE task_id = $1`, o.Subject.EntityID).Scan(&status); err != nil {
			return nil, nil, err
		}
		if status != "done" && status != "dropped" {
			hints = append(hints, o.Ref+" is still "+status+": call task_close if it is finished")
		}
	}
	return missing, hints, nil
}
