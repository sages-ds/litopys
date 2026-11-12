package verbs

import (
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/vocab"
)

// Record types: a type is a name of the project's vocabularies — "decision" (general), "migration"
// or "dba.migration" — checked by the command against the project's set, not by a closed enum.

var typeNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)?$`)

// checkType: the shape of a type name; the facade's old list (legacy) stays a closed enum.
func checkType(field, typ string, legacy bool) error {
	if legacy {
		return checkEnum(field, typ, statementTypes...)
	}
	if len(typ) > 63 || !typeNameRe.MatchString(typ) {
		return invalid(field, "a type of the project's vocabularies, like decision, migration or dba.migration")
	}
	return nil
}

// resolveType turns the given type into the stored one and its family; legacy (the facade, session_close) maps an
// old type by the fixed legacy map, without the project's set (the old contract takes its types in any
// project), and keeps any other as given. warning is type_legacy_alias when an old type name was mapped by the set.
func (c *cmd) resolveType(typ *string, projectID uuid.UUID, legacy bool) (family, warning string, err error) {
	if legacy {
		if to, ok := vocab.Legacy(*typ); ok {
			*typ = to
		}
		family, ok := vocab.FamilyOf(*typ)
		if !ok {
			return "", "", invalid("type", "unknown type %q", *typ)
		}
		return family, "", nil
	}
	set, err := vocab.ProjectSet(c.ctx, c.tx, projectID)
	if err != nil {
		return "", "", err
	}
	r, err := set.Resolve(*typ)
	var ve *vocab.Error
	if errors.As(err, &ve) {
		return "", "", typeError(ve, set)
	}
	if err != nil {
		return "", "", err
	}
	if r.Legacy {
		warning = "type_legacy_alias: " + *typ + " is stored as " + r.Type
		if r.Outside != "" {
			// Outside always has a vocabulary prefix: general is in every set, so only another vocabulary's type
			// can be outside it
			v, _, _ := strings.Cut(r.Outside, ".")
			warning += " (" + r.Outside + " needs the vocabulary " + v + ", not enabled for the project)"
		}
	}
	*typ = r.Type
	return r.Family, warning, nil
}

// typeError: the refusal with what the project accepts, at most 40 types.
func typeError(e *vocab.Error, set vocab.Set) error {
	msg := e.Error()
	if e.Code == "type_unknown" || e.Code == "type_reserved" {
		var allowed []string
		for _, x := range set.Entries() {
			if len(allowed) == 40 {
				allowed = append(allowed, "…")
				break
			}
			allowed = append(allowed, x.Type+" ("+x.Family+")")
		}
		msg += ". Allowed: " + strings.Join(allowed, ", ")
	}
	return fail(e.Code, "%s", msg)
}

func warningList(w string) []string {
	if w == "" {
		return nil
	}
	return []string{w}
}
