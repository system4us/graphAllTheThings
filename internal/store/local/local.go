// Package local is the default VectorStore: vectors persisted as JSON next
// to graph.json, cosine search brute-forced in memory. Metadata graphs are
// small (hundreds to a few thousand nodes), so this is microseconds — no
// external vector database needed.
package local

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"graphallthethings/internal/lock"
	"graphallthethings/internal/store"
)

type entry struct {
	NodeID string    `json:"id"`
	Type   string    `json:"type"`
	Name   string    `json:"name"`
	Hash   string    `json:"h,omitempty"`      // hash of the text this vector was embedded from
	Vector []float32 `json:"vector,omitempty"` // legacy files only; "split-1" keeps vectors in vectors.bin
}

type file struct {
	Model string `json:"model"` // embedding model used at index time
	Dim   int    `json:"dim"`
	// Format "split-1": entries carry no vectors; they are float32 little-endian
	// in the sibling vectors.bin (16-byte Gen header, then entries in order).
	// Parsing 8M floats from JSON dominated every pass; raw floats are ~3x
	// smaller and load in milliseconds. Empty Format = legacy all-in-JSON.
	Format  string  `json:"format,omitempty"`
	Gen     string  `json:"gen,omitempty"`
	Entries []entry `json:"entries"`
}

const splitFormat = "split-1"

// binPath is the vector blob next to the metadata file.
func binPath(path string) string { return strings.TrimSuffix(path, filepath.Ext(path)) + ".bin" }

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

func (s *Store) Upsert(ctx context.Context, points []store.Point) error {
	return s.write(points, true)
}

// Replace makes the index contain exactly points: vectors for nodes that are no
// longer in the graph are dropped. Use it only with the complete live set (a
// full `index` pass); a partial set would discard the rest.
func (s *Store) Replace(_ context.Context, points []store.Point) error {
	return s.write(points, false)
}

// write persists points under a cross-process lock. With merge it re-reads the
// file inside the lock, so a concurrent writer's changes are not lost.
func (s *Store) write(points []store.Point, merge bool) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	release, err := lock.Acquire(s.Path+".lock", 30*time.Second)
	if err != nil {
		return err
	}
	defer release()

	// Merge over the existing index so a partial upsert (post-refresh
	// re-embed of a few changed nodes) doesn't wipe the other vectors.
	// A missing file or a model switch starts from empty.
	desired := s.Model
	s.loaded = false // another process may have written since we last read
	_ = s.load()
	if desired != "" && desired != s.Model {
		s.entries = nil
		s.Model = desired
	}
	if !merge {
		s.entries = nil
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
	for _, e := range f.Entries {
		if len(e.Vector) != f.Dim {
			return fmt.Errorf("vector index: node %q has %d dims, index has %d (embedding model changed? run `gatt index --full`)", e.NodeID, len(e.Vector), f.Dim)
		}
	}
	var genRaw [8]byte
	if _, err := rand.Read(genRaw[:]); err != nil {
		return err
	}
	gen := hex.EncodeToString(genRaw[:]) // 16 ASCII bytes
	bin := make([]byte, 0, len(gen)+4*f.Dim*len(f.Entries))
	bin = append(bin, gen...)
	meta := make([]entry, len(f.Entries))
	for i, e := range f.Entries {
		for _, x := range e.Vector {
			bin = binary.LittleEndian.AppendUint32(bin, math.Float32bits(x))
		}
		e.Vector = nil
		meta[i] = e
	}
	data, err := json.Marshal(file{Model: f.Model, Dim: f.Dim, Format: splitFormat, Gen: gen, Entries: meta})
	if err != nil {
		return err
	}
	// Each file is written to a temp name and renamed, so a crash or a concurrent
	// reader never sees a truncated file. The blob goes first: a reader that
	// catches the gap sees a Gen mismatch and retries (see load).
	if err := atomicWrite(binPath(s.Path), bin); err != nil {
		return err
	}
	if err := atomicWrite(s.Path, data); err != nil {
		return err
	}
	s.entries = f.Entries
	s.loaded = true
	return nil
}

// attachVectors fills f.Entries[i].Vector from the blob, verifying it belongs to
// the same write as the metadata.
func attachVectors(f *file, path string) error {
	if f.Format != splitFormat {
		return fmt.Errorf("vector index: unknown format %q (written by a newer gatt?)", f.Format)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("vector blob missing: %w", err)
	}
	const genLen = 16
	want := genLen + 4*f.Dim*len(f.Entries)
	if len(blob) != want || string(blob[:genLen]) != f.Gen {
		return fmt.Errorf("vector blob does not match its index (gen/size mismatch); run `gatt index --full`")
	}
	floats := blob[genLen:]
	for i := range f.Entries {
		v := make([]float32, f.Dim)
		off := i * 4 * f.Dim
		for j := range v {
			v[j] = math.Float32frombits(binary.LittleEndian.Uint32(floats[off+4*j:]))
		}
		f.Entries[i].Vector = v
	}
	return nil
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vectors-*.tmp")
	if err != nil {
		return err
	}
	fail := func(err error) error { tmp.Close(); os.Remove(tmp.Name()); return err }
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	var f file
	// A writer replaces vectors.bin before vectors.json; if we read in between,
	// the generations differ. That window is milliseconds, so retry briefly.
	for attempt := 0; ; attempt++ {
		data, err := os.ReadFile(s.Path)
		if err != nil {
			return fmt.Errorf("vector index not found (run `gatt index` first): %w", err)
		}
		f = file{}
		if err := json.Unmarshal(data, &f); err != nil {
			return err
		}
		if f.Format == "" {
			break // legacy: vectors are inline in the JSON
		}
		err = attachVectors(&f, binPath(s.Path))
		if err == nil {
			break
		}
		if attempt >= 20 {
			return err
		}
		time.Sleep(50 * time.Millisecond)
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
