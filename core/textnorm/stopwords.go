package textnorm

import "strings"

// Function words carry no topic: a query "why the windows upgrade failed" must not require "why" and "the" in the
// record. Closed classes only (prepositions, conjunctions, particles, pronouns, auxiliaries), Ukrainian and
// English. THERE IS NO RUSSIAN LIST (owner 2026-10-19). Matched on the lower-cased word before stemming.
var stopwords = map[string]bool{}

func init() {
	for _, list := range []string{
		// Ukrainian
		`а або аж але б би бо був була були було бути в вам вас ви від він вона вони воно все всі де для до
		 дуже є же з за зі і із її їй їх й його йому к коли котрий котра котре котрі куди лише мене мені ми
		 мій моя моє мої на навіть над нам нас не нею неї ні ним них ніж ну о об однак окрім от па по під
		 поки при про проте та так також там те тебе теж тепер ти то тобто той тим тих ту тут у уже усі хоч
		 хоча хто це цей ця ці цим цих цю чи чим чого чому що щоб щодо як яка яке які який якщо ще`,
		// English
		`a about above after again against all am an and any are as at be because been before being below
		 between both but by can could did do does doing down during each few for from further had has have
		 having he her here hers him his how i if in into is it its itself just me more most my no nor not of
		 off on once only or other our ours out over own same she should so some such than that the their
		 theirs them then there these they this those through to too under until up very was we were what
		 when where which while who whom why will with would you your yours`,
	} {
		for _, w := range strings.Fields(list) {
			stopwords[w] = true
		}
	}
}
