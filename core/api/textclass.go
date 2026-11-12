package api

import (
	"bytes"
	"encoding/json"
	"strings"

	"litopys.smart-gw.co.ua/core/privacy"
)

// The privacy write pipeline at the API: every text field of a command is cleaned by its class, whatever the verb
// does next. The classes come from domain/verbs.yaml (text_classes), the generator lists each tool's paths in surface.json
// and refuses a string field without a class. Refusal stays in dispatch (secretIn): it reads every string, ids included.

const (
	textLine = "line" // a one-line field: private blocks, whitespace collapsed
	textFree = "free" // free text: private blocks
	textJSON = "json" // an object of any shape: private blocks in its strings
)

// cleanArgs returns the arguments with the text fields of the tool cleaned; arguments that do not parse go on as they
// are, for decode to refuse with a message that names the problem.
func cleanArgs(t Tool, args json.RawMessage) json.RawMessage {
	if len(t.TextFields) == 0 {
		return args
	}
	d := json.NewDecoder(bytes.NewReader(args))
	d.UseNumber() // large numbers stay exact
	var v any
	if d.Decode(&v) != nil {
		return args
	}
	before, err := json.Marshal(v)
	if err != nil {
		return args
	}
	out, err := json.Marshal(cleanValue(v, "", t.TextFields))
	if err != nil || bytes.Equal(out, before) {
		return args // nothing cleaned: the client's bytes go on as they came
	}
	return out
}

func cleanValue(v any, path string, fields map[string]string) any {
	switch x := v.(type) {
	case string:
		return cleanText(x, fields[path])
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cleanValue(e, path+"[]", fields)
		}
		return out
	case map[string]any:
		if fields[path] == textJSON {
			return privacy.MaskValue(stripDeep(x))
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = cleanValue(e, childPath(path, k, fields), fields)
		}
		return out
	default:
		return v
	}
}

// childPath: a named property, or the entry of a map with free keys ("fields{}").
func childPath(path, key string, fields map[string]string) string {
	p := key
	if path != "" {
		p = path + "." + key
	}
	if _, ok := fields[path+"{}"]; ok {
		if _, named := fields[p]; !named {
			return path + "{}"
		}
	}
	return p
}

func cleanText(s, class string) string {
	switch class {
	case textLine:
		return privacy.MaskTitle(privacy.NormalizeOneLine(privacy.StripPrivate(s)))
	case textFree, textJSON:
		return privacy.MaskText(privacy.StripPrivate(s))
	default:
		return s
	}
}

func stripDeep(v any) any {
	switch x := v.(type) {
	case string:
		if strings.Contains(strings.ToLower(x), "<private>") {
			return privacy.StripPrivate(x)
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = stripDeep(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = stripDeep(e)
		}
		return out
	default:
		return v
	}
}
