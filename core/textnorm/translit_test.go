package textnorm

import "testing"

// TestLatin: the examples of the official table (resolution No. 55 of 27 January 2010), lower-cased, and the case
// x11 is for.
func TestLatin(t *testing.T) {
	for in, want := range map[string]string{
		"Згорани": "zghorany", "Розгон": "rozghon", "Єнакієве": "yenakiieve", "Їжакевич": "yizhakevych",
		"Йосипівка": "yosypivka", "Юрій": "yurii", "Яготин": "yahotyn", "Знам'янка": "znamianka",
		"Щербухи": "shcherbukhy", "Хмельницький": "khmelnytskyi", "Ґалаґан": "galagan",
		"опенбао": "openbao",
	} {
		if got := Latin(in); got != want {
			t.Errorf("Latin(%q) = %q, want %q", in, got, want)
		}
	}
}
