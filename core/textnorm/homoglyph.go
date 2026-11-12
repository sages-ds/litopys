package textnorm

import (
	"strings"
	"unicode"
)

// Cyrillic letters that look like Latin ones. A token that mixes scripts (НР_TOOLS typed with Cyrillic Н and Р,
// a code pasted from a Ukrainian document) gets them folded to Latin; a token in one script is left alone, so
// Ukrainian words never change.
var lookAlike = map[rune]rune{
	'а': 'a', 'в': 'b', 'е': 'e', 'к': 'k', 'м': 'm', 'н': 'h', 'о': 'o', 'р': 'p', 'с': 'c', 'т': 't', 'у': 'y',
	'х': 'x', 'і': 'i', 'ї': 'i', 'ј': 'j', 'ѕ': 's',
	'А': 'A', 'В': 'B', 'Е': 'E', 'К': 'K', 'М': 'M', 'Н': 'H', 'О': 'O', 'Р': 'P', 'С': 'C', 'Т': 'T', 'У': 'Y',
	'Х': 'X', 'І': 'I', 'Ї': 'I', 'Ј': 'J', 'Ѕ': 'S',
}

func foldMixed(t string) string {
	cyr, lat := false, false
	for _, r := range t {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			cyr = true
		case unicode.Is(unicode.Latin, r):
			lat = true
		}
	}
	if !cyr || !lat {
		return t
	}
	return strings.Map(func(r rune) rune {
		if l, ok := lookAlike[r]; ok {
			return l
		}
		return r
	}, t)
}
