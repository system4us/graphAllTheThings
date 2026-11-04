package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"graphallthethings/internal/embed"
	"graphallthethings/internal/graph"
	"graphallthethings/internal/store/local"
)

// fakeEmbedder counts how many texts reach the embedding server.
func fakeEmbedder(t *testing.T, sent *atomic.Int64) *embed.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		sent.Add(int64(len(in.Input)))
		out := make([][]float32, len(in.Input))
		for i := range out {
			out[i] = []float32{1, 0, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": out})
	}))
	t.Cleanup(srv.Close)
	return embed.New(srv.URL, "m1")
}

func testGraph(names ...string) *graph.Graph {
	g := graph.New("codebase:/x")
	for _, n := range names {
		g.AddNode(&graph.Node{ID: "func:" + n, Type: graph.NodeFunction, Name: n})
	}
	return g
}

func ids(g *graph.Graph) []string {
	var out []string
	for id := range g.Nodes {
		out = append(out, id)
	}
	return out
}

func TestReindexNodesSkipsUnchangedText(t *testing.T) {
	var sent atomic.Int64
	emb := fakeEmbedder(t, &sent)
	vs := local.New(filepath.Join(t.TempDir(), "vectors.json"))
	g := testGraph("a", "b", "c")

	n, err := ReindexNodes(context.Background(), g, vs, emb, "m1", ids(g))
	if err != nil || n != 3 || sent.Load() != 3 {
		t.Fatalf("first run: embedded=%d sent=%d err=%v, want 3/3", n, sent.Load(), err)
	}

	// Same nodes re-added (as a re-parse does) with identical text: nothing to embed.
	sent.Store(0)
	vs = local.New(vs.Path)
	n, err = ReindexNodes(context.Background(), g, vs, emb, "m1", ids(g))
	if err != nil || n != 0 || sent.Load() != 0 {
		t.Fatalf("unchanged run: embedded=%d sent=%d err=%v, want 0/0", n, sent.Load(), err)
	}

	// One node's text changes (name feeds NodeText): only that one is embedded,
	// and the untouched vectors survive the merge.
	g.Nodes["func:b"].Name = "b_renamed"
	vs = local.New(vs.Path)
	n, err = ReindexNodes(context.Background(), g, vs, emb, "m1", ids(g))
	if err != nil || n != 1 || sent.Load() != 1 {
		t.Fatalf("changed run: embedded=%d sent=%d err=%v, want 1/1", n, sent.Load(), err)
	}
	if got := len(local.New(vs.Path).Indexed()); got != 3 {
		t.Errorf("index has %d vectors after partial upsert, want 3", got)
	}
}

func TestReindexNodesRefusesModelSwitch(t *testing.T) {
	var sent atomic.Int64
	emb := fakeEmbedder(t, &sent)
	path := filepath.Join(t.TempDir(), "vectors.json")
	g := testGraph("a", "b", "c")
	if _, err := ReindexNodes(context.Background(), g, local.New(path), emb, "m1", ids(g)); err != nil {
		t.Fatal(err)
	}

	g.Nodes["func:a"].Name = "a2"
	_, err := ReindexNodes(context.Background(), g, local.New(path), emb, "other-model", []string{"func:a"})
	if !errors.Is(err, ErrModelMismatch) {
		t.Fatalf("err = %v, want ErrModelMismatch", err)
	}
	if got := len(local.New(path).Indexed()); got != 3 {
		t.Errorf("model mismatch wiped the index: %d vectors left, want 3", got)
	}
}

func TestUpsertIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vectors.json")
	var sent atomic.Int64
	g := testGraph("a")
	if _, err := ReindexNodes(context.Background(), g, local.New(path), fakeEmbedder(t, &sent), "m1", ids(g)); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "vectors.json" {
		t.Errorf("leftover temp files after upsert: %v", entries)
	}
}
