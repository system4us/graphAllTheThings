package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/system4us/graphAllTheThings/internal/engine"
	"github.com/system4us/graphAllTheThings/pkg/graph"
)

type stubProvider struct {
	eng       *engine.Engine
	root      string
	graphPath string
}

func (s *stubProvider) Mode() string { return "single" }
func (s *stubProvider) ListRepos() []RepoInfo {
	return []RepoInfo{{Root: s.root, Key: "demo"}}
}
func (s *stubProvider) EngineFor(_ context.Context, _ string) (*engine.Engine, string, error) {
	return s.eng, s.root, nil
}
func (s *stubProvider) TriggerRefresh(_ context.Context, _ string) (string, error) {
	return "started", nil
}
func (s *stubProvider) GraphPathFor(_ context.Context, _ string) (string, error) {
	return s.graphPath, nil
}
func (s *stubProvider) ReloadRepo(_ context.Context, _ string) error {
	return nil
}
func (s *stubProvider) StatusFor(_ context.Context, _ string) (RepoStatus, error) {
	return RepoStatus{Running: false, LastSummary: "up to date"}, nil
}

func sampleGraph(t *testing.T) (*graph.Graph, string) {
	t.Helper()
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "app.go"), []byte("package pkg\n\nfunc Handle() {\n\tHelper()\n}\n\nfunc Helper() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("codebase:" + dir)
	g.AddNode(&graph.Node{ID: "file:pkg/app.go", Type: graph.NodeFile, Name: "pkg/app.go", Attrs: map[string]string{"file": "pkg/app.go", "lang": "go"}})
	g.AddNode(&graph.Node{ID: "file:pkg/util.go", Type: graph.NodeFile, Name: "pkg/util.go", Attrs: map[string]string{"file": "pkg/util.go", "lang": "go"}})
	g.AddNode(&graph.Node{ID: "fn:Handle", Type: graph.NodeFunction, Name: "Handle", Attrs: map[string]string{"file": "pkg/app.go", "line_start": "3", "line_end": "5", "lang": "go"}})
	g.AddNode(&graph.Node{ID: "fn:Helper", Type: graph.NodeFunction, Name: "Helper", Attrs: map[string]string{"file": "pkg/app.go", "line_start": "7", "line_end": "7", "lang": "go"}})
	g.AddNode(&graph.Node{ID: "route:GET:/ping", Type: graph.NodeRoute, Name: "GET /ping", Attrs: map[string]string{"file": "pkg/app.go", "method": "GET", "path": "/ping"}})

	g.AddEdge("file:pkg/app.go", "file:pkg/util.go", graph.EdgeImports, nil)
	g.AddEdge("fn:Handle", "fn:Helper", graph.EdgeCalls, nil)
	g.AddEdge("route:GET:/ping", "fn:Handle", graph.EdgeHandledBy, nil)
	return g, dir
}

func TestHTTPAPI(t *testing.T) {
	g, dir := sampleGraph(t)
	gp := filepath.Join(dir, "graph.db")
	h := New(&stubProvider{eng: engine.New(g, nil, nil), root: dir, graphPath: gp})

	get := func(path string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET %s bad json: %v", path, err)
		}
		return out
	}

	info := get("/api/info")
	if info["mode"] != "single" {
		t.Errorf("mode = %v, want single", info["mode"])
	}

	ov := get("/api/overview")
	if ov["is_codebase"] != true {
		t.Errorf("is_codebase = %v, want true", ov["is_codebase"])
	}

	arch := get("/api/graph?view=architecture")
	if len(arch["nodes"].([]any)) != 2 {
		t.Errorf("architecture nodes = %d, want 2", len(arch["nodes"].([]any)))
	}
	if len(arch["edges"].([]any)) != 1 {
		t.Errorf("architecture edges = %d, want 1", len(arch["edges"].([]any)))
	}

	impact := get("/api/graph?view=impact&focus=fn:Helper&depth=3")
	// Helper -> Handle -> route:GET:/ping
	if len(impact["nodes"].([]any)) != 3 {
		t.Errorf("impact nodes = %d, want 3", len(impact["nodes"].([]any)))
	}

	routes := get("/api/graph?view=routes")
	if len(routes["nodes"].([]any)) != 2 {
		t.Errorf("routes nodes = %d, want 2", len(routes["nodes"].([]any)))
	}

	node := get("/api/node?id=fn:Handle")
	src, ok := node["source"].(map[string]any)
	if !ok || src["content"] == "" {
		t.Fatalf("expected source preview for fn:Handle, got %v", node["source"])
	}

	search := get("/api/search?q=Handle")
	if len(search["hits"].([]any)) == 0 {
		t.Errorf("expected search hit for Handle")
	}

	tree := get("/api/tree")
	if len(tree["children"].([]any)) == 0 {
		t.Errorf("expected tree children")
	}

	st := get("/api/status")
	if st["last_summary"] != "up to date" {
		t.Errorf("status = %v", st)
	}

	grep := get("/api/grep?q=Helper")
	if int(grep["total"].(float64)) == 0 {
		t.Errorf("expected grep hits for Helper")
	}

	drift := get("/api/doc-drift")
	if drift["text"] == nil {
		t.Errorf("expected doc-drift text")
	}

	ctxPack := get("/api/context?q=Handle")
	if ctxPack["text"] == "" {
		t.Errorf("expected context pack text")
	}

	body, _ := json.Marshal(map[string]any{"id": "fn:Handle", "entity_note": "main handler"})
	req := httptest.NewRequest(http.MethodPost, "/api/annotate", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/annotate = %d: %s", w.Code, w.Body.String())
	}
}
