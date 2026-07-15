// Package local is the default VectorStore: vectors persisted as JSON next
// to graph.json, cosine search brute-forced in memory. Metadata graphs are
// small (hundreds to a few thousand nodes), so this is microseconds — no
// external vector database needed.
package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"graphallthethings/internal/store"
)

type entry struct {
	NodeID string    `json:"id"`
	Type   string    `json:"type"`
	Name   string    `json:"name"`
	Hash   string    `json:"h,omitempty"` // hash of the text this vector was embedded from
	Vector []float32 `json:"vector"`
}

type file struct {
	Model   string  `json:"model"` // embedding model used at index time
	Dim     int     `json:"dim"`
	Entries []entry `json:"entries"`
}

type Store struct {
	Path string // e.g. gatt-out/vectors.json
	// Model is the embedding model recorded at Upsert time; queries must
	// use the same one or vectors won't be comparable.
	Model   string
	entries []entry
	loaded  bool
}

func New(path string) *Store { return &Store{Path: path} }

// normalize scales v to unit length so dot product == cosine similarity.
func normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(sum))
	if n == 0 {
		return
	}
	for i := range v {
		v[i] /= n
	}
}

// TextHash is the content hash of the text a node was embedded from. `index`
// compares it against the stored hash to skip re-embedding unchanged nodes.
func TextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

func (s *Store) Upsert(_ context.Context, points []store.Point) error {
	// Merge over the existing index so a partial upsert (post-refresh
	// re-embed of a few changed nodes) doesn't wipe the other vectors.
	// A missing file or a model switch starts from empty.
	desired := s.Model
	_ = s.load()
	if desired != "" && desired != s.Model {
		s.entries = nil
		s.Model = desired
	}
	byID := map[string]int{}
	for i, e := range s.entries {
		byID[e.NodeID] = i
	}
	f := file{Model: s.Model, Entries: s.entries}
	for _, p := range points {
		normalize(p.Vector)
		e := entry{
			NodeID: p.NodeID, Type: p.Type, Name: p.Name,
			Hash: TextHash(p.Text), Vector: p.Vector,
		}
		if i, ok := byID[p.NodeID]; ok {
			f.Entries[i] = e
		} else {
			byID[p.NodeID] = len(f.Entries)
			f.Entries = append(f.Entries, e)
		}
		f.Dim = len(p.Vector)
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.Path, data, 0o644); err != nil {
		return err
	}
	s.entries = f.Entries
	s.loaded = true
	return nil
}

func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return fmt.Errorf("vector index not found (run `gatt index` first): %w", err)
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	s.entries = f.Entries
	s.Model = f.Model
	s.loaded = true
	return nil
}

// StoredModel returns the embedding model recorded at index time, or ""
// when no index exists yet.
func (s *Store) StoredModel() string {
	_ = s.load()
	return s.Model
}

// PeekModel reads just the "model" field from a vectors.json file without
// unmarshaling the entries array — full load() CPU-parses every embedded
// vector (float32 per dimension, thousands of nodes) just to answer "which
// model made these," which measured 3s+ on an 11k-node index and is paid by
// every command (openEngine calls this via resolveModel), not just the
// search/code-query ones that actually need the vectors. "model" is the
// first field the file struct marshals, so the decoder returns before ever
// reaching "entries". Returns "" on any error — callers already treat that
// as "no index yet", same as a failed StoredModel().
func PeekModel(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return ""
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return ""
		}
		key, _ := keyTok.(string)
		if key == "model" {
			var v string
			if dec.Decode(&v) == nil {
				return v
			}
			return ""
		}
		// Not the field we want — skip its value (object/array/scalar,
		// whatever it is) without decoding into typed Go values.
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return ""
		}
	}
	return ""
}

// Cached is a previously-embedded node: the text hash it was embedded from
// and its (already normalized) vector, so `index` can reuse it unchanged.
type Cached struct {
	Hash   string
	Vector []float32
}

// Indexed returns the node ids already in the index keyed to their cached
// vector, so incremental indexing can reuse unchanged nodes. Returns an empty
// map (no error) when no index exists yet.
func (s *Store) Indexed() map[string]Cached {
	if err := s.load(); err != nil {
		return map[string]Cached{}
	}
	out := make(map[string]Cached, len(s.entries))
	for _, e := range s.entries {
		out[e.NodeID] = Cached{Hash: e.Hash, Vector: e.Vector}
	}
	return out
}

func (s *Store) Search(_ context.Context, vector []float32, limit int, nodeType string) ([]store.Hit, error) {
	if err := s.load(); err != nil {
		return nil, err
	}
	normalize(vector)
	var hits []store.Hit
	for _, e := range s.entries {
		if nodeType != "" && e.Type != nodeType {
			continue
		}
		if len(e.Vector) != len(vector) {
			continue
		}
		var dot float32
		for i, x := range e.Vector {
			dot += x * vector[i]
		}
		hits = append(hits, store.Hit{NodeID: e.NodeID, Score: dot, Type: e.Type, Name: e.Name})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}
