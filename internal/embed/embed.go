// Package embed produces text embeddings via a local Ollama server
// (POST /api/embed). Any Ollama-compatible endpoint works.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	DefaultURL   = "http://localhost:11434"
	DefaultModel = "nomic-embed-text"
)

type Client struct {
	URL   string
	Model string
	http  *http.Client
}

func New(url, model string) *Client {
	if url == "" {
		url = DefaultURL
	}
	if model == "" {
		model = DefaultModel
	}
	return &Client{URL: url, Model: model, http: &http.Client{Timeout: 120 * time.Second}}
}

// Embed returns one vector per input text.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": c.Model, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return nil, fmt.Errorf("embed: %s: %s", resp.Status, e.Error)
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed: got %d vectors for %d texts", len(out.Embeddings), len(texts))
	}
	return out.Embeddings, nil
}
