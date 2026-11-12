package verbs

import (
	"slices"
	"strings"

	"litopys.smart-gw.co.ua/core/vocab"
)

// checkVocabularyFact: a fact of kind vocabulary turns a built-in vocabulary on or off for its layer;
// general is always on and is not a key.
func checkVocabularyFact(key string, value *string) error {
	if _, ok := vocab.Get(key); !ok || key == vocab.General {
		keys := slices.DeleteFunc(vocab.Keys(), func(k string) bool { return k == vocab.General })
		return invalid("key", "a built-in vocabulary other than general: %s", strings.Join(keys, ", "))
	}
	if value == nil || (*value != "on" && *value != "off") {
		return invalid("value", "on | off for kind=vocabulary")
	}
	return nil
}
