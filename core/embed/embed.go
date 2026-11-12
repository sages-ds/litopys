// Package embed turns record text into vectors for meaning search: any OpenAI-compatible
// /v1/embeddings — a local model by default (Ollama bge-m3 on the pilot, owner's decision 2026-11-01), a vendor by
// choice. A vendor receives the text, so the pass sends only records up to MaxClass.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client is one embedding model behind an OpenAI-compatible endpoint.
type Client struct {
	URL      string // base, e.g. http://127.0.0.1:11434/v1
	Model    string // e.g. bge-m3
	Key      string // vendors only
	MaxClass string // the highest data class sent to it: local for a local model
	HTTP     *http.Client
}

// FromEnv: LITOPYS_EMBED_URL, LITOPYS_EMBED_MODEL, LITOPYS_EMBED_KEY, LITOPYS_EMBED_MAX_CLASS (default local);
// no URL — nil, meaning search off.
func FromEnv() (*Client, error) {
	u := strings.TrimRight(os.Getenv("LITOPYS_EMBED_URL"), "/")
	if u == "" {
		return nil, nil
	}
	c := &Client{URL: u, Model: os.Getenv("LITOPYS_EMBED_MODEL"), Key: os.Getenv("LITOPYS_EMBED_KEY"),
		MaxClass: os.Getenv("LITOPYS_EMBED_MAX_CLASS"), HTTP: &http.Client{Timeout: 2 * time.Minute}}
	if c.Model == "" {
		return nil, errors.New("LITOPYS_EMBED_URL is set without LITOPYS_EMBED_MODEL")
	}
	switch c.MaxClass {
	case "":
		c.MaxClass = "local"
	case "public", "internal", "local":
	default:
		return nil, fmt.Errorf("LITOPYS_EMBED_MAX_CLASS=%q: public | internal | local", c.MaxClass)
	}
	return c, nil
}

// Embed returns one L2-normalised vector per text.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": c.Model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("embeddings: HTTP %d: %.200s", resp.StatusCode, raw)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: %d vectors for %d texts", len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, errors.New("embeddings: index out of range")
		}
		vecs[d.Index] = Normalize(d.Embedding)
	}
	return vecs, nil
}

// Normalize scales a vector to length 1, so the dot product is the cosine.
func Normalize(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return v
	}
	k := float32(1 / math.Sqrt(n))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * k
	}
	return out
}

// Text is what a record is embedded from: its title and the head of its body.
func Text(title string, body *string) string {
	t := title
	if body != nil && *body != "" {
		b := []rune(*body)
		if len(b) > 2000 {
			b = b[:2000]
		}
		t += "\n" + string(b)
	}
	return t
}

// EmbedQuery embeds one search query (query.Embedder).
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second) // a slow model must not hold the search: lexical then
	defer cancel()
	v, err := c.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

// ModelName is the model the vectors belong to (query.Embedder).
func (c *Client) ModelName() string { return c.Model }
