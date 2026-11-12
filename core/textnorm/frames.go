package textnorm

import (
	"regexp"
	"strings"
)

// Query frames: the words a person or an agent wraps a question in — "remind me", "what did we
// decide about", «нагадай», «що вирішили щодо», «що вийшло в підсумку з». They say what kind of answer is wanted,
// not what it is about; records state facts, so these words are rare in them and IDF takes them for key words
// (Bendersky & Croft, SIGIR 2008), pulling in records that merely contain «вирішили». StripFrame takes them off the
// start of a query (Huston & Croft, CIKM 2010: stop structures). The list is closed and grammatical — imperative
// request verbs and interrogative frames about decisions and outcomes — written from the grammar, not from the gate's
// queries, and frozen with the experiment. Only the start of the query: inside it the same words may be the topic.
// Texts are Ukrainian and English only — NO RUSSIAN SUPPORT.

// Every frame ends at a space or the end of the query: RE2's \b is an ASCII word boundary and never fires after a
// Cyrillic letter.
var frameRes = []*regexp.Regexp{
	// a request: «нагадай(те)», «підкажи(ть)», «покажи(ть)», «знайди(ть)», «скажи(ть)»; remind / show / tell me, find
	regexp.MustCompile(`(?i)^(нагадай(те)?|підкажи(ть)?|покажи(ть)?|знайди(ть)?|скажи(ть)?)(\s+(мені|нам))?(\s+|$)`),
	regexp.MustCompile(`(?i)^(remind|show|tell)\s+(me|us)(\s+(of|about))?(\s+|$)|^find(\s+|$)`),
	// a decision: «що / яке рішення (ми, я) вирішили / ухвалили / прийняли (щодо / про / з / по / стосовно)»
	regexp.MustCompile(`(?i)^(що|яке\s+рішення)(\s+(ми|я))?\s+(вирішили|вирішив|вирішила|вирішено|ухвалили|ухвалено|прийняли|прийнято)(\s+(щодо|про|з|із|по|стосовно))?(\s+|$)`),
	// an outcome: «що (не) вийшло (в / у підсумку) (з / із / по / щодо)», «чим закінчилось / завершилось (з)»
	regexp.MustCompile(`(?i)^(що(\s+не)?\s+вийшло|чим\s+(закінчилось|закінчилося|завершилось|завершилося))(\s+(в|у)\s+підсумку)?(\s+(з|із|по|щодо|з\s+приводу))?(\s+|$)`),
	// English: "what did we / I decide (about / on)", "what was decided (about / on)", "what came of"
	regexp.MustCompile(`(?i)^what\s+((did|have)\s+(we|i)\s+(decide|decided)|was\s+decided|came\s+of)(\s+(about|on|for|with))?(\s+|$)`),
}

// StripFrame returns the query without its leading frames (a request, then a question frame), its case kept, and
// the frames taken off, lower-cased. A query that is nothing but a frame is kept as it is.
func StripFrame(q string) (rest, frame string) {
	s := strings.TrimSpace(Normalize(q)) // case kept: CamelCase is an identifier signal
	var taken []string
	for changed := true; changed; {
		changed = false
		for _, re := range frameRes {
			if m := re.FindString(s); m != "" {
				taken = append(taken, strings.ToLower(strings.TrimSpace(m)))
				s = strings.TrimLeft(strings.TrimSpace(s[len(m):]), ",:—- ")
				changed = true
			}
		}
	}
	if strings.TrimSpace(s) == "" {
		return q, ""
	}
	return s, strings.Join(taken, " ")
}
