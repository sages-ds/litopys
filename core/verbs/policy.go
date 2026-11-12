package verbs

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/policy"
)

// PolicySetIn is policy.set of domain/verbs.yaml.
type PolicySetIn struct {
	Name   string      `json:"name"`
	Scope  string      `json:"scope"` // tenant | owner
	Rule   policy.Rule `json:"rule"`
	Active *bool       `json:"active,omitempty"` // default true
	Reason *string     `json:"reason,omitempty"`
}

// PolicySet puts, replaces (same name) or switches off (active=false) a rule. Scope tenant applies to
// every person of the tenant and needs the admin role.
func (s *Service) PolicySet(ctx context.Context, p Principal, in PolicySetIn) (uuid.UUID, Ack, error) {
	if !policyNameRe.MatchString(in.Name) {
		return uuid.Nil, Ack{}, invalid("name", "%q does not match %s", in.Name, policyNameRe)
	}
	if err := checkEnum("scope", in.Scope, "tenant", "owner"); err != nil {
		return uuid.Nil, Ack{}, err
	}
	if err := in.Rule.Validate(in.Scope); err != nil {
		return uuid.Nil, Ack{}, invalid("rule", "%v", err)
	}
	if in.Scope == "tenant" && p.Role != "admin" {
		return uuid.Nil, Ack{}, fail("forbidden", "a tenant-wide rule needs the admin role")
	}
	if err := optLen("reason", in.Reason, 500); err != nil {
		return uuid.Nil, Ack{}, err
	}
	active := in.Active == nil || *in.Active
	var id uuid.UUID
	var a Ack
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		if err := c.requireLiveActor(); err != nil {
			return err
		}
		id = policy.PolicyID(p.TenantID, in.Scope, c.me, in.Name)
		var raw []byte
		var was bool
		err := c.tx.QueryRow(c.ctx, `SELECT rule, active FROM projection.policy WHERE policy_id = $1`, id).Scan(&raw, &was)
		if err == nil {
			var old policy.Rule
			if json.Unmarshal(raw, &old) == nil && reflect.DeepEqual(old, in.Rule) && was == active {
				a.Unchanged = true
				return nil
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		payload := map[string]any{"name": in.Name, "scope": in.Scope, "rule": in.Rule, "active": active}
		if in.Reason != nil {
			payload["reason"] = *in.Reason
		}
		e, err := c.emit("policy.set", "policy", id, nil, nil, payload)
		if err == nil {
			a = ack(e)
		}
		return err
	})
	return id, a, err
}
