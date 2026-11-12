package verbs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/policy"
)

// Data classes (0023): a project has one (internal by default); a statement or a task may raise it
// for itself and never lower it — the decision of the owner 2026-10-16. Lowering a project's class is the owner's
// explicit project.amend.

// checkDataClass validates an optional class.
func checkDataClass(field string, v *string) error {
	if v == nil {
		return nil
	}
	return checkEnum(field, *v, dataClasses...)
}

// ExecutorProfile is an entry of the registry of executors (0024): what an agent or service runs
// on and the highest data class it may receive. The key is only a reference into the secret store (BYOK).
type ExecutorProfile struct {
	Provider       string              `json:"provider"`
	Model          string              `json:"model"`
	KeyKind        string              `json:"key_kind"`
	SecretRef      *string             `json:"secret_ref,omitempty"`
	Limits         *ExecutorLimits     `json:"limits,omitempty"`
	DataPolicy     *ExecutorDataPolicy `json:"data_policy,omitempty"`
	MaxDataClass   string              `json:"max_data_class"`
	TermsCheckedAt *string             `json:"terms_checked_at,omitempty"`
	TermsURL       *string             `json:"terms_url,omitempty"`
	// The preset it was made from, how to reach the model, the key the app stores, the access a
	// person widened beyond the class
	Preset   *string           `json:"preset,omitempty"`
	Adapter  *string           `json:"adapter,omitempty"`
	BaseURL  *string           `json:"base_url,omitempty"`
	Fields   map[string]string `json:"fields,omitempty"`
	Disabled *bool             `json:"disabled,omitempty"`
	Key      *ExecutorKeyRef   `json:"key,omitempty"`
	Access   *ExecutorAccess   `json:"access,omitempty"`
}

// ExecutorKeyRef names the key the app stores for the executor (fingerprint, not the key).
type ExecutorKeyRef struct {
	Fingerprint string `json:"fingerprint"`
	SetAt       string `json:"set_at"`
}

// ExecutorAccess: a person's conscious widening of what the executor may see beyond its class (owner's decision
// 2026-11-01): projects named here regardless of their class, or all.
type ExecutorAccess struct {
	Mode      string   `json:"mode"`
	Projects  []string `json:"projects,omitempty"`
	Consent   *string  `json:"consent,omitempty"`
	GrantedAt *string  `json:"granted_at,omitempty"`
}

// widens: the access goes beyond the class of the profile.
func (a *ExecutorAccess) widens() bool { return a != nil && a.Mode != "" && a.Mode != "preset" }

type ExecutorLimits struct {
	RPM *int `json:"rpm,omitempty"`
	RPD *int `json:"rpd,omitempty"`
	TPM *int `json:"tpm,omitempty"`
	TPD *int `json:"tpd,omitempty"`
}

type ExecutorDataPolicy struct {
	Training    *bool `json:"training,omitempty"`
	Retention   *bool `json:"retention,omitempty"`
	HumanReview *bool `json:"human_review,omitempty"`
}

var (
	keyKinds    = []string{"local", "free", "paid", "plan_credits"}
	secretRefRe = regexp.MustCompile(`^[A-Za-z0-9_./:-]{1,512}$`)
)

// check validates the profile and the two rules of data classes: local data goes only to local models, and a provider
// that trains on the data gets public data only.
func (x *ExecutorProfile) check() error {
	if err := checkLen("executor.provider", x.Provider, 1, 60); err != nil {
		return err
	}
	if err := checkLen("executor.model", x.Model, 1, 120); err != nil {
		return err
	}
	if err := checkEnum("executor.key_kind", x.KeyKind, keyKinds...); err != nil {
		return err
	}
	if err := checkEnum("executor.max_data_class", x.MaxDataClass, dataClasses...); err != nil {
		return err
	}
	if x.SecretRef != nil {
		if x.KeyKind == "local" {
			return invalid("executor.secret_ref", "a local model has no provider key")
		}
		if !secretRefRe.MatchString(*x.SecretRef) {
			return invalid("executor.secret_ref", "a path in the secret store (letters, digits, _ . / : -), never the key itself")
		}
	}
	if x.Limits != nil {
		for name, v := range map[string]*int{"rpm": x.Limits.RPM, "rpd": x.Limits.RPD, "tpm": x.Limits.TPM, "tpd": x.Limits.TPD} {
			if v != nil && *v < 0 {
				return invalid("executor.limits."+name, "must be >= 0")
			}
		}
	}
	if x.TermsCheckedAt != nil {
		if _, err := time.Parse("2006-01-02", *x.TermsCheckedAt); err != nil {
			return invalid("executor.terms_checked_at", "must be YYYY-MM-DD")
		}
	}
	if err := optLen("executor.terms_url", x.TermsURL, 512); err != nil {
		return err
	}
	if x.MaxDataClass == "local" && x.KeyKind != "local" {
		return invalid("executor.max_data_class", "local data goes only to a local model (key_kind local)")
	}
	if err := x.checkReach(); err != nil {
		return err
	}
	// a provider that trains on the data gets public data — unless a person widened the access knowingly (consent)
	if x.DataPolicy != nil && x.DataPolicy.Training != nil && *x.DataPolicy.Training && x.MaxDataClass != "public" &&
		!(x.Access.widens() && x.Access.Consent != nil) {
		return invalid("executor.max_data_class", "a provider that trains on the data gets public data only, unless a person widens the access (access with consent)")
	}
	return nil
}

// checkReach validates the reach fields: adapter, base URL, preset fields, key reference and access.
func (x *ExecutorProfile) checkReach() error {
	if x.Adapter != nil {
		if err := checkEnum("executor.adapter", *x.Adapter, "openai", "anthropic"); err != nil {
			return err
		}
	}
	if x.BaseURL != nil && !strings.HasPrefix(*x.BaseURL, "https://") && !strings.HasPrefix(*x.BaseURL, "http://") {
		return invalid("executor.base_url", "an http(s) URL")
	}
	if err := optLen("executor.base_url", x.BaseURL, 512); err != nil {
		return err
	}
	if err := optLen("executor.preset", x.Preset, 60); err != nil {
		return err
	}
	for k, v := range x.Fields {
		if err := checkLen("executor.fields."+k, v, 1, 120); err != nil {
			return err
		}
	}
	if x.Key != nil && !keyFingerprintRe.MatchString(x.Key.Fingerprint) {
		return invalid("executor.key.fingerprint", "8 hex digits")
	}
	if a := x.Access; a != nil {
		if err := checkEnum("executor.access.mode", a.Mode, "preset", "projects", "all"); err != nil {
			return err
		}
		if a.Mode == "projects" && len(a.Projects) == 0 {
			return invalid("executor.access.projects", "name the projects for mode projects")
		}
		if a.widens() && (a.Consent == nil || strings.TrimSpace(*a.Consent) == "") {
			return invalid("executor.access.consent", "widening the access needs the terms the person accepted")
		}
		if err := optLen("executor.access.consent", a.Consent, 2000); err != nil {
			return err
		}
	}
	return nil
}

var keyFingerprintRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// requireHumanForAccess: only a person widens an executor's access — not an agent, not a delegated grant (owner's
// decision 2026-11-01).
func (c *cmd) requireHumanForAccess(x *ExecutorProfile) error {
	if x == nil || !x.Access.widens() {
		return nil
	}
	var kind string
	if err := c.tx.QueryRow(c.ctx, `SELECT kind FROM projection.actor WHERE actor_id = $1`, c.p.ActorID).Scan(&kind); err != nil {
		return err
	}
	if kind != "human" || c.p.Grant.Derived {
		return fail("person_required", "only a person widens what an executor may see")
	}
	return nil
}

// narrowestClass is the most restrictive of the given limits for what a grant may see — the lowest class; empty
// arguments set no limit, all empty gives "".
func narrowestClass(classes ...string) string {
	out := ""
	for _, c := range classes {
		if c != "" && (out == "" || policy.ClassRank(c) < policy.ClassRank(out)) {
			out = c
		}
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// raiseOnly refuses a record class below the one the record already has (an absent one counts as the lowest).
// table is a projection with data_class and the id column idCol.
func (c *cmd) raiseOnly(table, idCol string, id uuid.UUID, next string) error {
	var cur *string
	err := c.tx.QueryRow(c.ctx, fmt.Sprintf(`SELECT data_class FROM projection.%s WHERE %s = $1`, table, idCol), id).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // the caller reports the missing record
	}
	if err != nil {
		return err
	}
	if cur != nil && policy.ClassRank(next) < policy.ClassRank(*cur) {
		return fail("data_class_lowered", "data class of this record is %s; it can only be raised, not lowered to %s", *cur, next)
	}
	return nil
}
