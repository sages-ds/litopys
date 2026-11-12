package textnorm

import "unicode"

// SwapLayout retypes a word typed in the wrong keyboard layout: a word all of Latin letters becomes what the
// same keys give in the Ukrainian layout («ghjtrn» → «проект»), a word all of Cyrillic ones what they give in the
// English layout («щзутифщ» → «openbao»). The keys are those of the standard Windows Ukrainian (Enhanced) layout over
// US QWERTY. A word mixing scripts or holding anything else is returned as it is. The text is lower-case.
//
// Ukrainian letters on punctuation keys (х ї ж є б ю) cannot be met from a word: the tokenizer has already split it
// there, so «;jdnf» reaches here as «jdnf».
func SwapLayout(w string) string {
	cyr, lat := false, false
	for _, r := range w {
		switch {
		case r >= 'a' && r <= 'z':
			lat = true
		case unicode.Is(unicode.Cyrillic, r):
			cyr = true
		default:
			return w
		}
	}
	if cyr == lat {
		return w
	}
	m := toUkrainian
	if cyr {
		m = toEnglish
	}
	rs := []rune(w)
	for i, r := range rs {
		v, ok := m[r]
		if !ok {
			return w // a Cyrillic letter of no Ukrainian key: not a layout slip
		}
		rs[i] = v
	}
	return string(rs)
}

var toUkrainian = map[rune]rune{
	'q': 'й', 'w': 'ц', 'e': 'у', 'r': 'к', 't': 'е', 'y': 'н', 'u': 'г', 'i': 'ш', 'o': 'щ', 'p': 'з',
	'a': 'ф', 's': 'і', 'd': 'в', 'f': 'а', 'g': 'п', 'h': 'р', 'j': 'о', 'k': 'л', 'l': 'д',
	'z': 'я', 'x': 'ч', 'c': 'с', 'v': 'м', 'b': 'и', 'n': 'т', 'm': 'ь',
}

var toEnglish = func() map[rune]rune {
	m := map[rune]rune{'х': '[', 'ї': ']', 'ж': ';', 'є': '\'', 'б': ',', 'ю': '.', 'ґ': '\\'}
	for l, c := range toUkrainian {
		m[c] = l
	}
	return m
}()
