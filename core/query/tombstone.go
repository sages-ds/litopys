package query

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/privacy"
)

// Tombstones: a reference to an erased entity always resolves — to what is left of it: its kind and
// number, when it was erased, by whom and why. Never its title or content: the title may itself be the secret. The
// reason is shown to everyone who follows the reference (owner's decision 2026-10-24), masked like any free text.

// Tombstone is the erasure or the quarantine of an entity as reads show it.
type Tombstone struct {
	State  string    `json:"state"` // purged | quarantined
	At     time.Time `json:"at"`
	By     ActorRef  `json:"by"`
	Reason string    `json:"reason"`
}

// tombstones: the journal.purged or journal.quarantined event of the given entities of one type, by entity id — the
// latest wins (a purge comes after any quarantine; a released entity is not asked for).
func (s *Service) tombstones(ctx context.Context, tenant uuid.UUID, entityType string, ids []uuid.UUID) (map[uuid.UUID]*Tombstone, error) {
	out := map[uuid.UUID]*Tombstone{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT entity_id, recorded_at, actor_id, coalesce(payload->>'reason', ''),
	               CASE type WHEN 'journal.purged' THEN 'purged' ELSE 'quarantined' END FROM journal
	        WHERE tenant_id = $1 AND type IN ('journal.purged', 'journal.quarantined') AND entity_type = $2 AND entity_id = ANY($3)
	        ORDER BY seq`, tenant, entityType, ids)
	if err != nil {
		return nil, err
	}
	var id uuid.UUID
	var t Tombstone
	_, err = pgx.ForEachRow(rows, []any{&id, &t.At, &t.By.ActorID, &t.Reason, &t.State}, func() error {
		x := t
		x.Reason = privacy.MaskText(x.Reason)
		out[id] = &x
		return nil
	})
	return out, err
}
