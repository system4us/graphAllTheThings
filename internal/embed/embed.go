// Package embed produces text embeddings from a local embedding server.
//
// Two wire formats are supported, chosen by the server URL:
//   - Ollama (default): POST {url}/api/embed
//   - OpenAI-compatible: when the URL path ends in /v1, POST {url}/embeddings.
//     This is what Text Embeddings Inference, llama.cpp's server, LM Studio and
//     recent Ollama versions expose, so any of them can be pointed at with
//     --embed-url http://host:port/v1 and no change on the gatt side.
//
// An optional bearer token is read from GATT_EMBED_API_KEY (OpenAI mode only).
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	DefaultURL = "http://localhost:11434"
	// DefaultModel is multilingual (bge-m3): schema names are English but
	// user questions often aren't; nomic-embed-text missed obvious tables
	// on Spanish queries.
	DefaultModel = "bge-m3"
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

// openAI reports whether the configured URL selects the OpenAI-compatible API.
func (c *Client) openAI() bool {
	return strings.HasSuffix(strings.TrimRight(c.URL, "/"), "/v1")
}

// Embed returns one vector per input text.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if c.openAI() {
		return c.embedOpenAI(ctx, texts)
	}
	return c.embedOllama(ctx, texts)
}

func (c *Client) embedOllama(ctx context.Context, texts []string) ([][]float32, error) {
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

func (c *Client) embedOpenAI(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": c.Model, "input": texts})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(c.URL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := os.Getenv("GATT_EMBED_API_KEY"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Servers differ: {"error":{"message":..}} (OpenAI), {"error":".."} (TEI).
		var raw json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&raw)
		msg := string(raw)
		var e struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && len(e.Error) > 0 {
			msg = string(e.Error)
		}
		return nil, fmt.Errorf("embed: %s: %s", resp.Status, msg)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embed: got %d vectors for %d texts", len(out.Data), len(texts))
	}
	// The spec lets servers return items out of order; "index" is authoritative.
	sort.SliceStable(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })
	vecs := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}
