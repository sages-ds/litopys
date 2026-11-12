// Package executors holds the vendor presets of the runner's executors: gen/providers.json is
// generated from domain/providers.yaml with the help of each vendor from domain/executor-keys.md — the settings
// page fills its form from a preset and shows the same help. It also seals the provider keys a person enters there
// (keys.go).
package executors

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

//go:embed gen/providers.json
var raw []byte

// Preset is one vendor of providers.yaml.
type Preset struct {
	Key            string          `json:"key"`
	Title          string          `json:"title"`
	Adapter        string          `json:"adapter"` // openai | anthropic
	BaseURL        string          `json:"base_url"`
	Fields         []string        `json:"fields"`
	Model          string          `json:"model"`
	KeyKind        string          `json:"key_kind"`
	Limits         map[string]int  `json:"limits,omitempty"`
	DataPolicy     map[string]bool `json:"data_policy,omitempty"`
	DataClass      string          `json:"data_class"`
	TermsURL       string          `json:"terms_url,omitempty"`
	TermsCheckedAt string          `json:"terms_checked_at,omitempty"`
	Help           string          `json:"help"`
}

var (
	presets = map[string]Preset{}
	order   []string
)

func init() {
	var doc struct {
		Order     []string          `json:"order"`
		Providers map[string]Preset `json:"providers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		panic("executors: gen/providers.json: " + err.Error())
	}
	for k, p := range doc.Providers {
		p.Key = k
		presets[k] = p
	}
	order = doc.Order
}

// Presets lists the vendors in the order of providers.yaml.
func Presets() []Preset {
	out := make([]Preset, 0, len(order))
	for _, k := range order {
		out = append(out, presets[k])
	}
	return out
}

// Lookup gives the preset of a vendor.
func Lookup(key string) (Preset, bool) {
	p, ok := presets[key]
	return p, ok
}

var fieldRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,120}$`)

// URL is the preset's base_url with its fields filled in ({account_id}, {host}); a missing or unsafe field is an
// error — a value lands in a URL, so only plain host / id characters pass.
func (p Preset) URL(fields map[string]string) (string, error) {
	u := p.BaseURL
	for _, f := range p.Fields {
		v := strings.TrimSpace(fields[f])
		if !fieldRe.MatchString(v) {
			return "", fmt.Errorf("%s: required, letters, digits and . _ : - only", f)
		}
		u = strings.ReplaceAll(u, "{"+f+"}", v)
	}
	return u, nil
}
