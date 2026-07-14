// Package qdrant is the opt-in VectorStore backed by a Qdrant server, for
// setups where the index outgrows the in-process default (many sources,
// shared index across machines).
package qdrant

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"graphallthethings/internal/store"
)

const DefaultURL = "http://localhost:6333"

type Client struct {
	URL        string
	Collection string
	http       *http.Client
}

func New(url, collection string) *Client {
	if url == "" {
		url = DefaultURL
	}
	return &Client{URL: url, Collection: collection, http: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("qdrant %s %s: %s: %s", method, path, resp.Status, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// EnsureCollection creates (or recreates) the collection for the given
// vector dimension.
func (c *Client) EnsureCollection(ctx context.Context, dim int) error {
	_ = c.do(ctx, http.MethodDelete, "/collections/"+c.Collection, nil, nil)
	return c.do(ctx, http.MethodPut, "/collections/"+c.Collection, map[string]any{
		"vectors": map[string]any{"size": dim, "distance": "Cosine"},
	}, nil)
}

// pointID derives a deterministic UUID from the node ID, since Qdrant only
// accepts uint64 or UUID point ids.
func pointID(nodeID string) string {
	h := sha256.Sum256([]byte(nodeID))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// Upsert recreates the collection and uploads all points.
func (c *Client) Upsert(ctx context.Context, points []store.Point) error {
	if len(points) == 0 {
		return fmt.Errorf("nothing to index")
	}
	if err := c.EnsureCollection(ctx, len(points[0].Vector)); err != nil {
		return err
	}
	batch := 128
	for i := 0; i < len(points); i += batch {
		end := min(i+batch, len(points))
		var ps []map[string]any
		for _, p := range points[i:end] {
			ps = append(ps, map[string]any{
				"id":     pointID(p.NodeID),
				"vector": p.Vector,
				"payload": map[string]any{
					"node_id": p.NodeID, "type": p.Type, "name": p.Name, "text": p.Text,
				},
			})
		}
		if err := c.do(ctx, http.MethodPut, "/collections/"+c.Collection+"/points?wait=true",
			map[string]any{"points": ps}, nil); err != nil {
			return err
		}
	}
	return nil
}

// Search returns the nodes closest to the vector, optionally filtered by
// node type ("table", "column", ...).
func (c *Client) Search(ctx context.Context, vector []float32, limit int, nodeType string) ([]store.Hit, error) {
	body := map[string]any{"vector": vector, "limit": limit, "with_payload": true}
	if nodeType != "" {
		body["filter"] = map[string]any{
			"must": []any{map[string]any{"key": "type", "match": map[string]any{"value": nodeType}}},
		}
	}
	var out struct {
		Result []struct {
			Score   float32        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if err := c.do(ctx, http.MethodPost, "/collections/"+c.Collection+"/points/search", body, &out); err != nil {
		return nil, err
	}
	var hits []store.Hit
	for _, r := range out.Result {
		h := store.Hit{Score: r.Score}
		if v, ok := r.Payload["node_id"].(string); ok {
			h.NodeID = v
		}
		if v, ok := r.Payload["type"].(string); ok {
			h.Type = v
		}
		if v, ok := r.Payload["name"].(string); ok {
			h.Name = v
		}
		hits = append(hits, h)
	}
	return hits, nil
}

// Ping checks the server is reachable.
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/collections", nil, nil)
}
