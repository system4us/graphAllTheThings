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
	"strings"
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
	return c.create(ctx, dim)
}

func (c *Client) create(ctx context.Context, dim int) error {
	return c.do(ctx, http.MethodPut, "/collections/"+c.Collection, map[string]any{
		"vectors": map[string]any{"size": dim, "distance": "Cosine"},
	}, nil)
}

// ensureExists creates the collection only if it is missing, so a partial
// upsert never wipes the points already there. A vector of the wrong size is
// then rejected by Qdrant itself (e.g. after switching embedding models).
func (c *Client) ensureExists(ctx context.Context, dim int) error {
	err := c.do(ctx, http.MethodGet, "/collections/"+c.Collection, nil, nil)
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "404") {
		return err
	}
	return c.create(ctx, dim)
}

// pointID derives a deterministic UUID from the node ID, since Qdrant only
// accepts uint64 or UUID point ids.
func pointID(nodeID string) string {
	h := sha256.Sum256([]byte(nodeID))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// Upsert adds or updates the given points; the rest of the collection is kept.
func (c *Client) Upsert(ctx context.Context, points []store.Point) error {
	if len(points) == 0 {
		return fmt.Errorf("nothing to index")
	}
	if err := c.ensureExists(ctx, len(points[0].Vector)); err != nil {
		return err
	}
	return c.upload(ctx, points)
}

// Replace makes the collection contain exactly points (it is recreated).
func (c *Client) Replace(ctx context.Context, points []store.Point) error {
	if len(points) == 0 {
		return fmt.Errorf("nothing to index")
	}
	if err := c.EnsureCollection(ctx, len(points[0].Vector)); err != nil {
		return err
	}
	return c.upload(ctx, points)
}

func (c *Client) upload(ctx context.Context, points []store.Point) error {
	batch := 128
	for i := 0; i < len(points); i += batch {
		end := min(i+batch, len(points))
		var ps []map[string]any
		for _, p := range points[i:end] {
			ps = append(ps, map[string]any{
				"id":     pointID(p.NodeID),
				"vector": p.Vector,
				"payload": map[string]any{
					"node_id": p.NodeID, "type": p.Type, "name": p.Name, "text": p.Text, "hash": p.Hash,
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

// Hashes returns node id -> text hash for every point in the collection
// (empty when the collection does not exist yet). Points written before hashes
// were stored come back with an empty hash and are simply re-embedded once.
func (c *Client) Hashes(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	var offset any
	for {
		body := map[string]any{
			"limit": 1000, "with_vector": false,
			"with_payload": map[string]any{"include": []string{"node_id", "hash"}},
		}
		if offset != nil {
			body["offset"] = offset
		}
		var resp struct {
			Result struct {
				Points []struct {
					Payload map[string]any `json:"payload"`
				} `json:"points"`
				Next any `json:"next_page_offset"`
			} `json:"result"`
		}
		if err := c.do(ctx, http.MethodPost, "/collections/"+c.Collection+"/points/scroll", body, &resp); err != nil {
			if strings.Contains(err.Error(), "404") {
				return out, nil
			}
			return nil, err
		}
		for _, p := range resp.Result.Points {
			id, _ := p.Payload["node_id"].(string)
			if id == "" {
				continue
			}
			h, _ := p.Payload["hash"].(string)
			out[id] = h
		}
		if resp.Result.Next == nil {
			return out, nil
		}
		offset = resp.Result.Next
	}
}

// Delete removes the given nodes from the collection.
func (c *Client) Delete(ctx context.Context, nodeIDs []string) error {
	const batch = 500
	for i := 0; i < len(nodeIDs); i += batch {
		end := min(i+batch, len(nodeIDs))
		ids := make([]string, 0, end-i)
		for _, id := range nodeIDs[i:end] {
			ids = append(ids, pointID(id))
		}
		if err := c.do(ctx, http.MethodPost, "/collections/"+c.Collection+"/points/delete?wait=true",
			map[string]any{"points": ids}, nil); err != nil {
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
