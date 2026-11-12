package verbs

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"litopys.smart-gw.co.ua/core/privacy"
	"litopys.smart-gw.co.ua/core/vocab"
)

// Structured fields of a record: the type of the vocabulary names them and their kind; a
// sum is a number, not prose, so ledger.summary can add it up. A type without fields takes none.

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	phoneRe    = regexp.MustCompile(`^\+?[0-9 ()-]{5,30}$`)
	emailRe    = regexp.MustCompile(`^[^@\s]{1,64}@[^@\s]{1,255}$`)
)

// checkFields validates fields against the stored type and returns them normalised (currency upper-cased, strings
// trimmed) with warnings for missing required ones; nil in — nil out.
func checkFields(typ string, in map[string]any) (map[string]any, []string, error) {
	if in == nil {
		return nil, nil, nil
	}
	t, _ := vocab.Lookup(typ)
	if len(t.Fields) == 0 {
		if len(in) == 0 {
			return nil, nil, nil
		}
		return nil, nil, invalid("fields", "type %s has no structured fields", typ)
	}
	allowed := slices.Sorted(maps.Keys(t.Fields))
	out := map[string]any{}
	for k, v := range in {
		f, ok := t.Fields[k]
		if !ok {
			return nil, nil, invalid("fields."+k, "not a field of %s (fields: %s)", typ, strings.Join(allowed, ", "))
		}
		if v == nil {
			continue
		}
		switch f.Type {
		case "number":
			n, ok := v.(float64)
			if !ok {
				return nil, nil, invalid("fields."+k, "a number (1250.50), not text")
			}
			out[k] = n
		default:
			s, ok := v.(string)
			if !ok {
				return nil, nil, invalid("fields."+k, "text")
			}
			s = strings.TrimSpace(s)
			if err := checkLen("fields."+k, s, 1, 500); err != nil {
				return nil, nil, err
			}
			if kind := privacy.DetectSecret(s); kind != "" {
				return nil, nil, fail("secret_rejected", "fields.%s contains a secret (%s); write ${VAR} instead", k, kind)
			}
			switch f.Type {
			case "date":
				if _, err := time.Parse("2006-01-02", s); err != nil {
					return nil, nil, invalid("fields."+k, "a date YYYY-MM-DD")
				}
			case "currency":
				if s = strings.ToUpper(s); !currencyRe.MatchString(s) {
					return nil, nil, invalid("fields."+k, "an ISO 4217 code: UAH, EUR, USD")
				}
			case "email":
				if !emailRe.MatchString(s) {
					return nil, nil, invalid("fields."+k, "an e-mail address")
				}
			case "phone":
				if !phoneRe.MatchString(s) {
					return nil, nil, invalid("fields."+k, "a phone number: digits, spaces, + ( ) -")
				}
			}
			out[k] = s
		}
	}
	var warnings []string
	for _, k := range allowed {
		if _, ok := out[k]; !ok && t.Fields[k].Required {
			warnings = append(warnings, fmt.Sprintf("field_missing: %s of %s", k, typ))
		}
	}
	return out, warnings, nil
}
