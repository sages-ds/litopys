package verbs

import (
	"strconv"

	"github.com/google/uuid"
)

// Human numbers of entities (0030): R-<num> within the owner, <project>/T-, W-, X-<num> within the
// project. The command issues the number under a lock and puts it into the creation event; numbers are never reused
// (rows of retracted and purged entities stay, and a migration reserves what the source gave out — 0037).

// RecordRef is the canonical ref of a record.
func RecordRef(num int64) string { return "R-" + strconv.FormatInt(num, 10) }

// ProjectRef is the canonical ref of a task (T), workflow (W) or execution (X): always with the project, a reply
// may mix projects.
func ProjectRef(project, kind string, num int) string {
	return project + "/" + kind + "-" + strconv.Itoa(num)
}

// nextRecordNum issues the next record number of the project's owner, serialised per owner for the transaction.
func (c *cmd) nextRecordNum(projectID uuid.UUID) (int64, error) {
	var owner uuid.UUID
	if err := c.tx.QueryRow(c.ctx, `SELECT owner_id FROM projection.project WHERE project_id = $1`, projectID).Scan(&owner); err != nil {
		return 0, err
	}
	if _, err := c.tx.Exec(c.ctx, `SELECT pg_advisory_xact_lock(hashtext('litopys.statement.num:' || $1::text))`, owner); err != nil {
		return 0, err
	}
	var n int64
	err := c.tx.QueryRow(c.ctx, `SELECT GREATEST(COALESCE(max(num), 0), `+floorSQL("R")+`) + 1
	          FROM projection.statement WHERE owner_id = $1`, owner).Scan(&n)
	return n, err
}

// nextTaskNum issues the next task number of a project; the caller holds the project row lock.
func (c *cmd) nextTaskNum(projectID uuid.UUID) (int, error) {
	var n int
	err := c.tx.QueryRow(c.ctx, `SELECT GREATEST(COALESCE(max(num), 0), `+floorSQL("T")+`) + 1
	          FROM projection.task WHERE project_id = $1`, projectID).Scan(&n)
	return n, err
}

// floorSQL: the number floor a migration reserved for scope $1 (0037) — the source's numbers stay unused even
// where the source erased its newest entities.
func floorSQL(kind string) string {
	return `COALESCE((SELECT floor FROM projection.num_floor WHERE scope_id = $1 AND kind = '` + kind + `'), 0)`
}

// nextProjectNum issues the next workflow or execution number of a project (table: workflow | execution) under the
// project row lock, as task numbers are.
func (c *cmd) nextProjectNum(table string, projectID uuid.UUID) (int, error) {
	if _, err := c.tx.Exec(c.ctx, `SELECT 1 FROM projection.project WHERE project_id = $1 FOR UPDATE`, projectID); err != nil {
		return 0, err
	}
	var n int
	err := c.tx.QueryRow(c.ctx, `SELECT COALESCE(max(num), 0) + 1 FROM projection.`+table+` WHERE project_id = $1`, projectID).Scan(&n)
	return n, err
}

// slug is the name of a project, for refs.
func (c *cmd) slug(projectID uuid.UUID) (string, error) {
	var s string
	err := c.tx.QueryRow(c.ctx, `SELECT name FROM projection.project WHERE project_id = $1`, projectID).Scan(&s)
	return s, err
}
