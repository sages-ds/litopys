package verbs

import (
	"github.com/google/uuid"
)

// Explicit refs: the edge resolves the internal refs of a record or a task (R-N, <project>/T-, W-, X-N,
// an id) to entities and passes them as Mentions; the command keeps the refs as given and makes one link "mentions"
// per target, in the same transaction. A new list replaces the old one: links to targets no longer named are
// retired, new ones asserted. Nothing is parsed out of text.

const maxRefs = 20

// setMentions makes the live mentions links of from exactly targets (a self-mention is skipped).
func (c *cmd) setMentions(from EntityRef, targets []EntityRef) error {
	rows, err := c.tx.Query(c.ctx, `SELECT link_id, to_type, to_id FROM projection.link
	        WHERE from_type = $1 AND from_id = $2 AND relation = 'mentions' AND NOT retired`, from.EntityType, from.EntityID)
	if err != nil {
		return err
	}
	live := map[EntityRef]uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		var to EntityRef
		if err := rows.Scan(&id, &to.EntityType, &to.EntityID); err != nil {
			rows.Close()
			return err
		}
		live[to] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	want := map[EntityRef]bool{}
	for _, to := range targets {
		if to == from || want[to] {
			continue
		}
		want[to] = true
		if _, ok := live[to]; ok {
			continue
		}
		if _, err := c.emit("link.asserted", "link", uuid.New(), nil, nil,
			map[string]any{"from": from, "to": to, "relation": "mentions"}); err != nil {
			return err
		}
	}
	for to, id := range live {
		if !want[to] {
			if _, err := c.emit("link.retired", "link", id, nil, nil, map[string]any{"reason": "no longer in refs"}); err != nil {
				return err
			}
		}
	}
	return nil
}
