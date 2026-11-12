package api

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/verbs"
)

// Internal refs: a record R-<num> within the owner; a task, workflow or execution T-, W-, X-<num>
// within a project — <project>/T-12, or T-12 in the project of the call; or the uuid of an entity of the owner.
// Anything else is not an internal ref (jira:ABC-1, pr:<url>): the caller decides what it is.

var (
	recordRefRe  = regexp.MustCompile(`^(?i:r)-(\d+)$`)
	projectRefRe = regexp.MustCompile(`^(?:([^/\s]+)/)?([TtWwXx])-(\d+)$`)
)

// projectKinds: the entity of a project ref by its letter, with its table, id column and liveness condition.
var projectKinds = map[byte]struct{ entity, table, id, live string }{
	'T': {"task", "task", "task_id", "NOT hidden"},
	'W': {"workflow", "workflow", "workflow_id", "true"},
	'X': {"execution", "execution", "execution_id", "true"},
}

// lookupRef resolves an internal ref. ok = false: ref is not of an internal form. A ref of an internal form whose
// entity does not exist (or is not the caller's) answers <entity>_not_found. gone = true also resolves an entity that
// is no longer live — a purged record, a retracted task: a mention speaks of the past; a subject of
// work (an outcome) must be live.
func (a *API) lookupRef(ctx context.Context, c *Caller, ref, project string, gone bool) (_ *verbs.EntityRef, ok bool, _ error) {
	ref = strings.TrimSpace(ref)
	if id, err := uuid.Parse(ref); err == nil {
		var typ string
		err := a.DB.QueryRow(ctx, `SELECT 'statement' FROM projection.statement WHERE statement_id = $1 AND owner_id = $2 AND ($3 OR NOT hidden)
		        UNION ALL SELECT 'task' FROM projection.task t JOIN projection.project p ON p.project_id = t.project_id
		         WHERE t.task_id = $1 AND p.owner_id = $2 AND ($3 OR NOT t.hidden)
		        UNION ALL SELECT 'workflow' FROM projection.workflow WHERE workflow_id = $1 AND owner_id = $2
		        UNION ALL SELECT 'execution' FROM projection.execution WHERE execution_id = $1 AND owner_id = $2`, id, c.Owner, gone).Scan(&typ)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, true, errorf("entity_not_found", "no record, task, workflow or execution of yours has id %s", id)
		}
		if err != nil {
			return nil, true, err
		}
		return &verbs.EntityRef{EntityType: typ, EntityID: id}, true, nil
	}
	if m := recordRefRe.FindStringSubmatch(ref); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		var id uuid.UUID
		err := a.DB.QueryRow(ctx, `SELECT statement_id FROM projection.statement WHERE owner_id = $1 AND num = $2 AND ($3 OR NOT hidden)`,
			c.Owner, n, gone).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, true, errorf("statement_not_found", "record R-%d", n)
		}
		if err != nil {
			return nil, true, err
		}
		return &verbs.EntityRef{EntityType: "statement", EntityID: id}, true, nil
	}
	if m := projectRefRe.FindStringSubmatch(ref); m != nil {
		if m[1] != "" {
			project = m[1]
		}
		if project == "" {
			return nil, true, invalid("ref %q: name the project (<project>/%s-%s) — this call has no project", ref, strings.ToUpper(m[2]), m[3])
		}
		slug, pid, err := a.project(ctx, c, project)
		if err != nil {
			return nil, true, err
		}
		letter := strings.ToUpper(m[2])[0]
		k := projectKinds[letter]
		n, _ := strconv.Atoi(m[3])
		var id uuid.UUID
		err = a.DB.QueryRow(ctx, `SELECT `+k.id+` FROM projection.`+k.table+` WHERE project_id = $1 AND num = $2 AND ($3 OR `+k.live+`)`,
			pid, n, gone).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, true, errorf(k.entity+"_not_found", "%s %s/%c-%d", k.entity, slug, letter, n)
		}
		if err != nil {
			return nil, true, err
		}
		return &verbs.EntityRef{EntityType: k.entity, EntityID: id}, true, nil
	}
	return nil, false, nil
}

// mentionsOf resolves the refs of a write in the project of the written entity: internal ones become
// mention targets, a missing entity refuses the whole write (ref_not_found), anything else
// stays a string of the list. A purged or retracted entity is a target too.
func (a *API) mentionsOf(ctx context.Context, c *Caller, refs []string, project string) ([]verbs.EntityRef, error) {
	var out []verbs.EntityRef
	for _, ref := range refs {
		er, ok, err := a.lookupRef(ctx, c, ref, project, true)
		if !ok {
			continue
		}
		var e *Error
		if errors.As(err, &e) && strings.HasSuffix(e.Code, "_not_found") {
			return nil, &Error{Code: "ref_not_found", Message: "refs: " + strconv.Quote(ref) + " — " + e.Message,
				Hint: "an internal ref names an existing entity: R-498, T-12 or <project>/T-12, W-, X-, or an id; " +
					"other refs (jira:ABC-1, pr:<url>) are kept as they are"}
		}
		if err != nil {
			return nil, err
		}
		out = append(out, *er)
	}
	return out, nil
}

// refsOf reads a refs value of task fields (a list of strings; null clears).
func refsOf(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// quarantineTarget resolves the ref of journal.quarantine / journal.release: a record or a task, live or not (the
// verb answers already_purged or not_quarantined itself).
func (a *API) quarantineTarget(ctx context.Context, c *Caller, ref, project string) (*verbs.EntityRef, error) {
	if project != "" {
		if err := a.slugOf(ctx, c, &project); err != nil {
			return nil, err
		}
	}
	target, ok, err := a.lookupRef(ctx, c, ref, project, true)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errorf("invalid_input", "ref: R-N, [<project>/]T-N or the id of a record or task, got %q", ref)
	}
	return target, nil
}

var deletionRefRe = regexp.MustCompile(`^(?:([^/\s]+)/)?[Dd]-(\d+)$`)

// deletionID resolves [<project>/]D-N or the id of a deletion proposal of the person.
func (a *API) deletionID(ctx context.Context, c *Caller, ref, project string) (uuid.UUID, error) {
	ref = strings.TrimSpace(ref)
	if id, err := uuid.Parse(ref); err == nil {
		return id, nil // the verb answers deletion_not_found for a foreign or unknown one
	}
	m := deletionRefRe.FindStringSubmatch(ref)
	if m == nil {
		return uuid.Nil, errorf("invalid_input", "deletion: [<project>/]D-N or the proposal's id, got %q", ref)
	}
	if m[1] != "" {
		project = m[1]
	}
	if project == "" {
		return uuid.Nil, errorf("invalid_input", "deletion %s: name the project (<project>/D-N or project)", ref)
	}
	_, projectID, err := a.project(ctx, c, project)
	if err != nil {
		return uuid.Nil, err
	}
	n, _ := strconv.Atoi(m[2])
	var id uuid.UUID
	err = a.DB.QueryRow(ctx, `SELECT deletion_id FROM projection.deletion WHERE project_id = $1 AND num = $2 AND owner_id = $3`,
		projectID, n, c.Owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errorf("deletion_not_found", "%s/D-%d", project, n)
	}
	return id, err
}
