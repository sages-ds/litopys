// Package stem gives the stem of a Ukrainian or English word.
//
// The stemmers are the official Snowball algorithms (snowballstem.org, BSD-3-Clause, COPYING.snowball), compiled
// to Go at the commit in SNOWBALL_SHA by .gitea/workflows/stemmers.yml: ukrainian/ and english/ are generated code,
// snowballrt/ is the Snowball Go runtime. Do not edit them by hand — regenerate; CI checks the drift and that the
// stems equal the reference stems of snowball-data.
//
// Texts are Ukrainian and English only. THERE IS NO RUSSIAN SUPPORT (owner 2026-10-19).
package stem

import (
	"litopys.smart-gw.co.ua/core/textnorm/stem/english"
	"litopys.smart-gw.co.ua/core/textnorm/stem/snowballrt"
	"litopys.smart-gw.co.ua/core/textnorm/stem/ukrainian"
)

// Ukrainian returns the stem of a lower-case Ukrainian word.
func Ukrainian(word string) string {
	env := snowballrt.NewEnv(word)
	ukrainian.Stem(env)
	return env.Current()
}

// English returns the stem of a lower-case English word.
func English(word string) string {
	env := snowballrt.NewEnv(word)
	english.Stem(env)
	return env.Current()
}
