// Package local is the default VectorStore: vectors persisted as JSON next
// to graph.json, cosine search brute-forced in memory. Metadata graphs are
// small (hundreds to a few thousand nodes), so this is microseconds — no
// external vector database needed.
package local

import (
	"context"
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

func (s *Store) Upsert(_ context.Context, points []store.Point) error {
	f := file{Model: s.Model}
	for _, p := range points {
		normalize(p.Vector)
		f.Entries = append(f.Entries, entry{NodeID: p.NodeID, Type: p.Type, Name: p.Name, Vector: p.Vector})
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
