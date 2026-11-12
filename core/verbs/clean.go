package verbs

import "litopys.smart-gw.co.ua/core/privacy"

// Point-wise privacy for the verbs without a pipeline yet: the same calls as
// statements.go until one pipeline replaces them with one pipeline for every free-text field.

// cleanLine is a one-line field: strip <private>, collapse whitespace, mask only unambiguous secrets.
func cleanLine(s string) string {
	return privacy.MaskTitle(privacy.NormalizeOneLine(privacy.StripPrivate(s)))
}

// cleanText is free text (body, summary, note, reason, acceptance, resolution).
func cleanText(s string) string { return privacy.MaskText(privacy.StripPrivate(s)) }

// cleanOpt cleans an optional free-text field in place.
func cleanOpt(s *string) {
	if s != nil {
		*s = cleanText(*s)
	}
}
