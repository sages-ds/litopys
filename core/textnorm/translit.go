package textnorm

import "strings"

// Latin transliterates Ukrainian Cyrillic into Latin letters by the official table of the Cabinet of Ministers of
// Ukraine (resolution No. 55 of 27 January 2010): є, ї, й, ю, я take their word-initial form (ye, yi, y, yu, ya) at
// the start of a word, «зг» gives «zgh», the soft sign and the apostrophe are dropped. Search v2 uses it for a query
// word no record holds — a Latin name typed in Cyrillic («опенбао» → openbao, x11). The text is lower-case; a rune
// outside the table is kept as it is.
func Latin(s string) string {
	rs := []rune(strings.ToLower(s))
	var b strings.Builder
	for i, r := range rs {
		start := i == 0 || !isLetter(rs[i-1])
		if r == 'г' && i > 0 && rs[i-1] == 'з' {
			b.WriteString("gh")
			continue
		}
		if start {
			if v, ok := latinInitial[r]; ok {
				b.WriteString(v)
				continue
			}
		}
		if v, ok := latinTable[r]; ok {
			b.WriteString(v)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isLetter(r rune) bool { _, ok := latinTable[r]; return ok || r >= 'a' && r <= 'z' }

var latinInitial = map[rune]string{'є': "ye", 'ї': "yi", 'й': "y", 'ю': "yu", 'я': "ya"}

var latinTable = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "h", 'ґ': "g", 'д': "d", 'е': "e", 'є': "ie", 'ж': "zh", 'з': "z",
	'и': "y", 'і': "i", 'ї': "i", 'й': "i", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p",
	'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch",
	'ь': "", 'ю': "iu", 'я': "ia", '\'': "",
}
