package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"graphallthethings/internal/graph"
)

func TestGrepOptionsLimitLockfilesAndGlobs(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"src/app.ts":        "const TAX = 0.21;\nconst TAX2 = 0.21;\n",
		"src/app_test.ts":   "expect(0.21).toBe(0.21);\n",
		"docs/guide.md":     "Tax rate is 0.21.\n",
		"package-lock.json": "{\"version\": \"0.21\"}\n",
		"go.sum":            "example.com/x v0.21.0\n",
	}
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	g := graph.New("codebase:" + dir)
	e := New(g, nil, nil)

	// Default skips lockfiles (package-lock.json, go.sum) -> 4 hits across 3 files.
	res, err := e.GrepWithOptions("0.21", GrepOptions{Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 4 || res.Shown != 4 || res.Truncated {
		t.Fatalf("default unlimited: total=%d shown=%d truncated=%v\n%s", res.Total, res.Shown, res.Truncated, res.Text)
	}
	if strings.Contains(res.Text, "package-lock.json") || strings.Contains(res.Text, "go.sum") {
		t.Errorf("lockfiles should be skipped by default:\n%s", res.Text)
	}

	// IncludeLockfiles: true brings in both lockfiles -> 6 hits.
	resAll, err := e.GrepWithOptions("0.21", GrepOptions{Limit: 0, IncludeLockfiles: true})
	if err != nil {
		t.Fatal(err)
	}
	if resAll.Total != 6 {
		t.Errorf("IncludeLockfiles total = %d, want 6", resAll.Total)
	}

	// Limit caps Shown and sets Truncated, while Total stays exact.
	resCap, err := e.GrepWithOptions("0.21", GrepOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !resCap.Truncated || resCap.Shown != 2 || resCap.Total != 4 {
		t.Errorf("capped: total=%d shown=%d truncated=%v", resCap.Total, resCap.Shown, resCap.Truncated)
	}

	// Include + Exclude globs.
	resFilt, err := e.GrepWithOptions("0.21", GrepOptions{
		Limit:   0,
		Include: []string{"src/**"},
		Exclude: []string{"*_test.ts"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resFilt.Total != 2 || !strings.Contains(resFilt.Text, "src/app.ts") || strings.Contains(resFilt.Text, "app_test.ts") {
		t.Errorf("filtered:\n%s", resFilt.Text)
	}
}

func TestRenderDefinitionConstKindAndCodeContextNotes(t *testing.T) {
	g := graph.New("codebase:/tmp/fake")
	g.AddNode(&graph.Node{
		ID:    "def:a.ts:TAX_RATE",
		Type:  graph.NodeDefinition,
		Name:  "TAX_RATE",
		Attrs: map[string]string{"file": "a.ts", "line_start": "1", "kind": "const"},
	})
	g.AddNode(&graph.Node{
		ID:    "def:a.ts:Invoice",
		Type:  graph.NodeDefinition,
		Name:  "Invoice",
		Attrs: map[string]string{"file": "a.ts", "line_start": "3"},
	})
	e := New(g, nil, nil)
	e.VectorsStale = true

	constOut, err := e.RenderDefinition("def:a.ts:TAX_RATE")
	if err != nil || !strings.HasPrefix(constOut, "## const TAX_RATE\n") {
		t.Errorf("const render = %q, %v", constOut, err)
	}
	typeOut, err := e.RenderDefinition("def:a.ts:Invoice")
	if err != nil || !strings.HasPrefix(typeOut, "## type Invoice\n") {
		t.Errorf("type render = %q, %v", typeOut, err)
	}

	pack, err := e.CodeContextPack(context.Background(), "TAX_RATE", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pack, "semantic vectors are stale") {
		t.Errorf("missing stale vectors warning:\n%s", pack)
	}
	if !strings.Contains(pack, "not exhaustive") || !strings.Contains(pack, "gatt grep") {
		t.Errorf("missing non-exhaustive / gatt grep note:\n%s", pack)
	}
}
