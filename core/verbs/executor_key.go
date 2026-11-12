package verbs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ExecutorKeySet stores the provider key of an executor the person entered on the settings page:
// sealed by the caller (core/executors.Sealer, bound to the executor), kept in auth.executor_key outside the journal,
// and the profile gets only its fingerprint (actor.amended) — the journal never holds the key. Only a person sets a
// key: an agent never sees one.
func (s *Service) ExecutorKeySet(ctx context.Context, p Principal, executor uuid.UUID, fingerprint string, nonce, sealed []byte) (Ack, error) {
	if !keyFingerprintRe.MatchString(fingerprint) || len(nonce) == 0 || len(sealed) == 0 {
		return Ack{}, invalid("key", "a sealed key with its fingerprint")
	}
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		var kind string
		if err := c.tx.QueryRow(ctx, `SELECT kind FROM projection.actor WHERE actor_id = $1`, p.ActorID).Scan(&kind); err != nil {
			return err
		}
		if kind != "human" || p.Grant.Derived {
			return fail("person_required", "only a person sets an executor's key")
		}
		if err := c.mine(executor); err != nil {
			return err
		}
		var raw []byte
		err := c.tx.QueryRow(ctx, `SELECT executor FROM projection.actor WHERE actor_id = $1 AND revoked_at IS NULL FOR UPDATE`, executor).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && len(raw) == 0) {
			return invalid("executor", "no live executor %s with a profile", executor)
		}
		if err != nil {
			return err
		}
		var x ExecutorProfile
		if err := json.Unmarshal(raw, &x); err != nil {
			return err
		}
		if x.KeyKind == "local" {
			return invalid("key", "a local model has no provider key")
		}
		owner, err := c.owner(p.ActorID)
		if err != nil {
			return err
		}
		if _, err := c.tx.Exec(ctx, `
			INSERT INTO auth.executor_key (actor_id, tenant_id, owner_id, nonce, sealed, fingerprint, set_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (actor_id) DO UPDATE SET nonce = $4, sealed = $5, fingerprint = $6, set_at = now(), set_by = $7,
			       checked_at = NULL, check_ok = NULL, check_note = NULL`,
			executor, p.TenantID, owner, nonce, sealed, fingerprint, p.ActorID); err != nil {
			return err
		}
		x.Key = &ExecutorKeyRef{Fingerprint: fingerprint, SetAt: time.Now().UTC().Format(time.RFC3339)}
		e, err := c.emit("actor.amended", "actor", executor, nil, nil, map[string]any{"executor": x})
		if err != nil {
			return err
		}
		a = ack(e)
		return nil
	})
	return a, err
}
