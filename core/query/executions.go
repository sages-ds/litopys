package query

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/flow"
	"litopys.smart-gw.co.ua/core/policy"
)

// Executions of workflows (0021): step states come from core/flow, as in the commands.

type ExecutionRow struct {
	ExecutionID   uuid.UUID  `json:"execution_id"`
	Ref           string     `json:"ref"` // <project>/X-<num>
	Project       string     `json:"project"`
	Workflow      string     `json:"workflow"`
	Version       int        `json:"version"`
	State         string     `json:"state"` // working, input_required (a step waits for a person), completed, failed, canceled
	StepsTotal    int        `json:"steps_total"`
	StepsDone     int        `json:"steps_done"`
	WaitingPerson int        `json:"waiting_person"`
	TaskNum       *int       `json:"task_num,omitempty"`
	Summary       *string    `json:"summary,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	ClosedAt      *time.Time `json:"closed_at,omitempty"`
	Provenance    Provenance `json:"provenance"`

	projectClass string // data class of the run's project (0023)
	params       map[string]any
	def          flow.Definition
	attempts     []attemptRow
	approval     []approvalRow
	state        flow.State
}

type attemptRow struct {
	AttemptID  uuid.UUID       `json:"attempt_id"`
	Key        string          `json:"-"`
	N          int             `json:"attempt"`
	State      string          `json:"state"`
	ActorID    uuid.UUID       `json:"actor_id"`
	LeaseUntil time.Time       `json:"lease_until"`
	Output     json.RawMessage `json:"output,omitempty"`
	Withheld   bool            `json:"output_withheld,omitempty"` // the step's class is above the reader's grant
	Error      *string         `json:"error,omitempty"`
}

type approvalRow struct {
	ApprovalID uuid.UUID       `json:"approval_id"`
	Key        string          `json:"-"`
	ActorID    uuid.UUID       `json:"actor_id"`
	Items      json.RawMessage `json:"items,omitempty"`
	Note       *string         `json:"note,omitempty"`
	Used       bool            `json:"used"`
	Revoked    bool            `json:"revoked"`
}

func (s *Service) executions(ctx context.Context, where string, limit string, a args) ([]*ExecutionRow, error) {
	rows, err := s.DB.Query(ctx, `SELECT e.execution_id, pr.name || '/X-' || e.num, pr.name, pr.data_class, w.key, e.version, e.state, t.num, e.summary, e.started_at, e.closed_at,
		       e.params, v.definition, `+provCols("j")+`
		  FROM projection.execution e JOIN projection.project pr ON pr.project_id = e.project_id
		  JOIN projection.workflow w ON w.workflow_id = e.workflow_id
		  JOIN projection.workflow_version v ON v.workflow_id = e.workflow_id AND v.version = e.version
		  LEFT JOIN projection.task t ON t.task_id = e.task_id`+provJoin("j", "e.tenant_id", "e.last_seq")+`
		 WHERE `+where+` ORDER BY e.started_at DESC LIMIT `+limit, a...)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*ExecutionRow, error) {
		x := &ExecutionRow{}
		var params, def []byte
		if err := r.Scan(append([]any{&x.ExecutionID, &x.Ref, &x.Project, &x.projectClass, &x.Workflow, &x.Version, &x.State, &x.TaskNum, &x.Summary,
			&x.StartedAt, &x.ClosedAt, &params, &def}, x.Provenance.dest()...)...); err != nil {
			return nil, err
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &x.params); err != nil {
				return nil, err
			}
		}
		var err error
		x.def, err = flow.ParseDefinition(def)
		return x, err
	})
	if err != nil || len(list) == 0 {
		return list, err
	}
	ids := make([]uuid.UUID, len(list))
	byID := map[uuid.UUID]*ExecutionRow{}
	for i, x := range list {
		ids[i], byID[x.ExecutionID] = x.ExecutionID, x
	}
	rows, err = s.DB.Query(ctx, `SELECT execution_id, attempt_id, step_key, n, state, actor_id, lease_until, output, error
		  FROM projection.step_attempt WHERE execution_id = ANY($1) ORDER BY step_key, n`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var eid uuid.UUID
		var a attemptRow
		var out []byte
		if err := rows.Scan(&eid, &a.AttemptID, &a.Key, &a.N, &a.State, &a.ActorID, &a.LeaseUntil, &out, &a.Error); err != nil {
			rows.Close()
			return nil, err
		}
		a.Output = out
		byID[eid].attempts = append(byID[eid].attempts, a)
	}
	rows.Close()
	rows, err = s.DB.Query(ctx, `SELECT execution_id, approval_id, step_key, actor_id, items, note, used_by IS NOT NULL, revoked
		  FROM projection.approval WHERE execution_id = ANY($1) ORDER BY granted_at`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var eid uuid.UUID
		var a approvalRow
		var items []byte
		if err := rows.Scan(&eid, &a.ApprovalID, &a.Key, &a.ActorID, &items, &a.Note, &a.Used, &a.Revoked); err != nil {
			rows.Close()
			return nil, err
		}
		a.Items = items
		byID[eid].approval = append(byID[eid].approval, a)
	}
	rows.Close()
	now := time.Now()
	for _, x := range list {
		att := make([]flow.Attempt, len(x.attempts))
		for i, a := range x.attempts {
			att[i] = flow.Attempt{Key: a.Key, N: a.N, State: a.State, LeaseUntil: a.LeaseUntil, Output: a.Output}
		}
		var appr []flow.Approval
		for _, a := range x.approval {
			if !a.Revoked {
				appr = append(appr, flow.Approval{Key: a.Key, Used: a.Used})
			}
		}
		if x.state, err = flow.Derive(x.def, x.params, att, appr, now); err != nil {
			return nil, err
		}
		x.StepsTotal = len(x.def.Steps)
		for _, ss := range x.state.Steps {
			switch ss.State {
			case flow.Completed, flow.Skipped:
				x.StepsDone++
			case flow.InputRequired:
				x.WaitingPerson++
			}
		}
		if x.State == "working" && x.WaitingPerson > 0 && len(x.state.ReadyFor) == 0 && !hasWorking(x.state) {
			x.State = "input_required"
		}
	}
	return list, nil
}

func hasWorking(st flow.State) bool {
	for _, ss := range st.Steps {
		if ss.State == flow.Working {
			return true
		}
	}
	return false
}

type ExecutionListIn struct {
	Project string `json:"project,omitempty"`
	State   string `json:"state,omitempty"` // open (default) | closed | all
	Limit   int    `json:"limit,omitempty"`
}

type ExecutionListOut struct {
	Items []*ExecutionRow `json:"items"`
}

// ExecutionList is execution.list.
func (s *Service) ExecutionList(ctx context.Context, p Principal, in ExecutionListIn) (ExecutionListOut, error) {
	out := ExecutionListOut{Items: []*ExecutionRow{}}
	ctx, owner, err := s.enter(ctx, p)
	if err != nil {
		return out, err
	}
	var a args
	where := []string{"e.owner_id = " + a.add(owner)}
	if in.Project != "" {
		pid, err := s.projectID(ctx, p, in.Project)
		if err != nil {
			return out, err
		}
		where = append(where, "e.project_id = "+a.add(pid))
	}
	switch in.State {
	case "", "open":
		where = append(where, "e.state = 'working'")
	case "closed":
		where = append(where, "e.state <> 'working'")
	case "all":
	default:
		return out, fail("invalid_input", "state: open | closed | all")
	}
	items, err := s.executions(ctx, strings.Join(where, " AND "), a.add(clamp(in.Limit, 50, 1, 200)), a)
	if items != nil {
		out.Items = items
	}
	return out, err
}

type StepView struct {
	Key       string        `json:"key"`
	Title     string        `json:"title"`
	Kind      string        `json:"kind"`
	State     string        `json:"state"`
	Waiting   string        `json:"waiting,omitempty"`
	Expired   bool          `json:"lease_expired,omitempty"`
	Attempts  []attemptRow  `json:"attempts"`
	Approvals []approvalRow `json:"approvals,omitempty"`
}

type ExecutionReadOut struct {
	Execution *ExecutionRow  `json:"execution"`
	Steps     []StepView     `json:"steps"`
	Params    map[string]any `json:"params,omitempty"`
}

func (s *Service) execution(ctx context.Context, p Principal, id uuid.UUID) (*ExecutionRow, error) {
	ctx, owner, err := s.enter(ctx, p)
	if err != nil {
		return nil, err
	}
	var a args
	list, err := s.executions(ctx, "e.owner_id = "+a.add(owner)+" AND e.execution_id = "+a.add(id), "1", a)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fail("execution_not_found", "execution %s", id)
	}
	list[0].withhold(p.Grant.MaxDataClass)
	return list[0], nil
}

// stepClass is the data class of a step's output: the step's own max_data_class when the definition
// sets it, else the project's. A step that handles records raised above its project must declare its class.
func (x *ExecutionRow) stepClass(key string) string {
	for _, st := range x.def.Steps {
		if st.Key == key && st.MaxDataClass != nil {
			return *st.MaxDataClass
		}
	}
	return x.projectClass
}

// withhold blanks the outputs of steps whose class is above limit (a derived grant's max_data_class); the step
// states stay, so the run reads the same apart from the hidden values. Conditions were evaluated before (Derive).
func (x *ExecutionRow) withhold(limit string) {
	if limit == "" {
		return
	}
	for i, a := range x.attempts {
		if len(a.Output) > 0 && policy.ClassRank(x.stepClass(a.Key)) > policy.ClassRank(limit) {
			x.attempts[i].Output, x.attempts[i].Withheld = nil, true
		}
	}
}

// takeable: a run's step can go to an executor whose class is limit — the grant it gets (the narrower of limit and
// the step's own class) still sees the run's project. An empty limit takes anything.
func (x *ExecutionRow) takeable(st flow.Step, limit string) bool {
	if limit == "" {
		return true
	}
	g := limit
	if st.MaxDataClass != nil && policy.ClassRank(*st.MaxDataClass) < policy.ClassRank(g) {
		g = *st.MaxDataClass
	}
	return policy.ClassRank(g) >= policy.ClassRank(x.projectClass)
}

// ExecutionRead is execution.read: every step with its derived state, attempts and approvals.
func (s *Service) ExecutionRead(ctx context.Context, p Principal, id uuid.UUID) (ExecutionReadOut, error) {
	var out ExecutionReadOut
	x, err := s.execution(ctx, p, id)
	if err != nil {
		return out, err
	}
	out.Execution, out.Params = x, x.params
	for i, st := range x.def.Steps {
		ss := x.state.Steps[i]
		v := StepView{Key: st.Key, Title: st.Title, Kind: st.Kind, State: ss.State, Waiting: ss.Waiting, Expired: ss.Expired, Attempts: []attemptRow{}}
		for _, a := range x.attempts {
			if a.Key == st.Key {
				v.Attempts = append(v.Attempts, a)
			}
		}
		for _, a := range x.approval {
			if a.Key == st.Key {
				v.Approvals = append(v.Approvals, a)
			}
		}
		out.Steps = append(out.Steps, v)
	}
	return out, nil
}

type StepOpenItemsIn struct {
	Project string `json:"project,omitempty"`
	// ExecutorActorID: only steps this executor may take by its profile's data class.
	ExecutorActorID *uuid.UUID `json:"executor_actor_id,omitempty"`
	Limit           int        `json:"limit,omitempty"`
}

type StepOpenItem struct {
	ExecutionID   uuid.UUID `json:"execution_id"`
	Project       string    `json:"project"`
	Workflow      string    `json:"workflow"`
	Version       int       `json:"version"`
	StepKey       string    `json:"step_key"`
	Title         string    `json:"title"`
	Kind          string    `json:"kind"`
	ExecutorClass *string   `json:"executor_class,omitempty"`
	LeaseExpired  bool      `json:"lease_expired,omitempty"`
}

// StepOpenItems is step.open_items: ready steps of open runs — the queue an executor pulls.
func (s *Service) StepOpenItems(ctx context.Context, p Principal, in StepOpenItemsIn) (struct {
	Items []StepOpenItem `json:"items"`
}, error) {
	var out struct {
		Items []StepOpenItem `json:"items"`
	}
	out.Items = []StepOpenItem{}
	// the class limit: the caller's own grant, narrowed by the named executor's profile — per project, since a person
	// may have widened the executor's access to some projects
	var profile []byte
	named := in.ExecutorActorID != nil
	if named {
		ctx2, _, err := s.enter(ctx, p)
		if err != nil {
			return out, err
		}
		err = s.DB.QueryRow(ctx2, `SELECT executor FROM projection.actor
		        WHERE actor_id = $1 AND kind IN ('agent', 'service') AND revoked_at IS NULL`, *in.ExecutorActorID).Scan(&profile)
		if errors.Is(err, pgx.ErrNoRows) {
			return out, fail("invalid_input", "executor_actor_id: no live agent or service %s of yours", *in.ExecutorActorID)
		}
		if err != nil {
			return out, err
		}
	}
	classFor := func(project string) string {
		class := p.Grant.MaxDataClass
		if ec := policy.ExecutorClass(profile, project); named && ec != "" && (class == "" || policy.ClassRank(ec) < policy.ClassRank(class)) {
			class = ec
		}
		return class
	}
	runs, err := s.ExecutionList(ctx, p, ExecutionListIn{Project: in.Project, State: "open", Limit: 200})
	if err != nil {
		return out, err
	}
	limit := clamp(in.Limit, 50, 1, 200)
	for _, x := range runs.Items {
		class := classFor(x.Project)
		for i, st := range x.def.Steps {
			ss := x.state.Steps[i]
			if ss.State != flow.Ready || (st.WhoMayRun != nil && *st.WhoMayRun == "human_only") || !x.takeable(st, class) {
				continue
			}
			out.Items = append(out.Items, StepOpenItem{ExecutionID: x.ExecutionID, Project: x.Project, Workflow: x.Workflow, Version: x.Version,
				StepKey: st.Key, Title: st.Title, Kind: st.Kind, ExecutorClass: st.ExecutorClass, LeaseExpired: ss.Expired})
			if len(out.Items) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

type StepPackIn struct {
	ExecutionID uuid.UUID `json:"execution_id"`
	StepKey     string    `json:"step_key"`
}

type StepPackOut struct {
	Step         json.RawMessage `json:"step"`
	State        string          `json:"state"`
	Params       map[string]any  `json:"params"`
	Inputs       map[string]any  `json:"inputs"`
	Context      []PackItem      `json:"context"`
	BudgetTokens *int            `json:"budget_tokens,omitempty"`
}

type PackItem struct {
	Selector   string `json:"selector"`
	Value      any    `json:"value,omitempty"`
	Unresolved string `json:"unresolved,omitempty"`
	Truncated  int    `json:"truncated,omitempty"` // recall: statements dropped to fit budget_tokens
}

// PackStatement is one statement a recall: selector brings: enough to cite and judge it, not its body.
type PackStatement struct {
	StatementID uuid.UUID `json:"statement_id"`
	Type        string    `json:"type"`
	Title       string    `json:"title"`
	EffectiveAt time.Time `json:"effective_at"`
	Snippet     *string   `json:"snippet,omitempty"`
}

var taskSelector = regexp.MustCompile(`^task:([a-z0-9][a-z0-9._-]*)#([0-9]+)$`)

// RecallSelectorLimit caps the statements one recall: selector brings before the budget cut.
const RecallSelectorLimit = 5

// StepPack is step.pack: the definition of one step, the run parameters, the outputs of finished steps and the
// context its selectors name (steps.<key>.output, task:<project>#<num>, recall:<query> in the run's project; other
// kinds are reported unresolved). Over budget_tokens, recall: statements are dropped from the last selector back.
func (s *Service) StepPack(ctx context.Context, p Principal, in StepPackIn) (StepPackOut, error) {
	out := StepPackOut{Params: map[string]any{}, Inputs: map[string]any{}, Context: []PackItem{}}
	x, err := s.execution(ctx, p, in.ExecutionID)
	if err != nil {
		return out, err
	}
	var rawDef struct {
		Steps []json.RawMessage `json:"steps"`
	}
	var def []byte
	ctx2, _, err := s.enter(ctx, p)
	if err != nil {
		return out, err
	}
	if err := s.DB.QueryRow(ctx2, `SELECT v.definition FROM projection.execution e JOIN projection.workflow_version v
	        ON v.workflow_id = e.workflow_id AND v.version = e.version WHERE e.execution_id = $1`, in.ExecutionID).Scan(&def); err != nil {
		return out, err
	}
	if err := json.Unmarshal(def, &rawDef); err != nil {
		return out, err
	}
	idx := -1
	for i, st := range x.def.Steps {
		if st.Key == in.StepKey {
			idx = i
		}
	}
	if idx < 0 {
		return out, fail("step_not_found", "no step %q in this version", in.StepKey)
	}
	out.Step, out.State = rawDef.Steps[idx], x.state.Steps[idx].State
	if x.params != nil {
		out.Params = x.params
	}
	latest := map[string]attemptRow{}
	for _, a := range x.attempts {
		if a.State == "completed" {
			if cur, ok := latest[a.Key]; !ok || a.N > cur.N {
				latest[a.Key] = a
			}
		}
	}
	withheld := map[string]bool{}
	for k, a := range latest {
		if a.Withheld {
			withheld[k] = true // above this grant's data class: neither an input nor a selector value
			continue
		}
		var v any
		if len(a.Output) > 0 {
			_ = json.Unmarshal(a.Output, &v)
		}
		out.Inputs[k] = v
	}
	var cp struct {
		ContextPack *struct {
			Selectors    []string `json:"selectors"`
			BudgetTokens *int     `json:"budget_tokens"`
		} `json:"context_pack"`
	}
	_ = json.Unmarshal(rawDef.Steps[idx], &cp)
	if cp.ContextPack == nil {
		return out, nil
	}
	out.BudgetTokens = cp.ContextPack.BudgetTokens
	for _, sel := range cp.ContextPack.Selectors {
		item := PackItem{Selector: sel}
		switch {
		case strings.HasPrefix(sel, "steps.") && strings.HasSuffix(sel, ".output"):
			key := strings.TrimSuffix(strings.TrimPrefix(sel, "steps."), ".output")
			if v, ok := out.Inputs[key]; ok {
				item.Value = v
			} else if withheld[key] {
				item.Unresolved = "step " + key + " output is above this grant's data class"
			} else {
				item.Unresolved = "step " + key + " has no output yet"
			}
		case taskSelector.MatchString(sel):
			m := taskSelector.FindStringSubmatch(sel)
			num, _ := strconv.Atoi(m[2])
			list, err := s.TaskList(ctx, p, TaskListIn{Project: m[1], Nums: []int{num}, IncludeBody: true})
			switch {
			case err != nil:
				item.Unresolved = err.Error()
			case len(list.Items) == 0:
				item.Unresolved = "no such task"
			default:
				item.Value = list.Items[0]
			}
		case strings.HasPrefix(sel, "recall:"):
			q := strings.TrimSpace(strings.TrimPrefix(sel, "recall:"))
			if q == "" {
				item.Unresolved = "empty recall query"
				break
			}
			// the reader's own principal: a derived token sees only statements within its data class (0023)
			res, err := s.Search(ctx, p, SearchIn{Query: q, Project: x.Project, IncludeSnippet: true, Limit: RecallSelectorLimit})
			switch {
			case err != nil:
				item.Unresolved = err.Error()
			case len(res.Items) == 0:
				item.Unresolved = "no statements match"
			default:
				rows := make([]PackStatement, 0, len(res.Items))
				for _, r := range res.Items {
					rows = append(rows, PackStatement{StatementID: r.StatementID, Type: r.Type, Title: r.Title, EffectiveAt: r.EffectiveAt, Snippet: r.Snippet})
				}
				item.Value = rows
			}
		default:
			item.Unresolved = "selector kind not resolved by the server yet"
		}
		out.Context = append(out.Context, item)
	}
	if out.BudgetTokens != nil {
		fitRecall(&out, *out.BudgetTokens)
	}
	return out, nil
}

// fitRecall drops recall: statements from the last selector back until the package fits the budget; the
// definition, inputs and other selectors are what the step was written against and are never cut.
func fitRecall(out *StepPackOut, budget int) {
	for i := len(out.Context) - 1; i >= 0 && estimateTokens(out) > budget; i-- {
		item := &out.Context[i]
		rows, ok := item.Value.([]PackStatement)
		if !ok {
			continue
		}
		for len(rows) > 0 && estimateTokens(out) > budget {
			rows = rows[:len(rows)-1]
			item.Truncated++
			item.Value = rows
		}
		if len(rows) == 0 {
			item.Value = nil
			item.Unresolved = "over budget_tokens"
		}
	}
}
