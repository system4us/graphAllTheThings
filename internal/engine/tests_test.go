package engine

import (
	"context"
	"strings"
	"testing"

	"graphallthethings/internal/graph"
)

func TestAffectedTestsAndBudget(t *testing.T) {
	g := graph.New("codebase:/tmp/demo")
	g.AddNode(&graph.Node{ID: "file:pkg/calc.go", Type: graph.NodeFile, Name: "pkg/calc.go", Attrs: map[string]string{"file": "pkg/calc.go"}})
	g.AddNode(&graph.Node{ID: "file:pkg/calc_test.go", Type: graph.NodeFile, Name: "pkg/calc_test.go", Attrs: map[string]string{"file": "pkg/calc_test.go"}})
	g.AddNode(&graph.Node{ID: "fn:Add", Type: graph.NodeFunction, Name: "Add", Attrs: map[string]string{"file": "pkg/calc.go", "line_start": "3"}})
	g.AddNode(&graph.Node{ID: "fn:TestAdd", Type: graph.NodeFunction, Name: "TestAdd", Attrs: map[string]string{"file": "pkg/calc_test.go", "line_start": "5"}})
	g.AddEdge("fn:TestAdd", "fn:Add", graph.EdgeCalls, nil)

	e := New(g, nil, nil)
	res, err := e.AffectedTests(context.Background(), "fn:Add", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.TestFiles) != 1 || res.TestFiles[0] != "pkg/calc_test.go" {
		t.Fatalf("test files = %v, want [pkg/calc_test.go]", res.TestFiles)
	}
	if len(res.TestFunctions) != 1 || res.TestFunctions[0].Name != "TestAdd" {
		t.Fatalf("test functions = %v, want [TestAdd]", res.TestFunctions)
	}
	if len(res.Commands) == 0 || !strings.Contains(res.Commands[0], "go test ./pkg -run '^(TestAdd)$'") {
		t.Fatalf("commands = %v", res.Commands)
	}

	// Token budget trimmer
	big := "## Sec1\n" + strings.Repeat("a", 500) + "\n## Sec2\n" + strings.Repeat("b", 500)
	trimmed := TrimToTokenBudget(big, 150) // ~600 chars
	if !strings.Contains(trimmed, "trimmed to ~150 token budget") || strings.Contains(trimmed, "Sec2") {
		t.Fatalf("unexpected budget trim: %s", trimmed)
	}

	// Issue extraction
	issues := extractIssueRefs("fix: resolve login bug #42 and PROJ-109 (#42)")
	if len(issues) != 2 || issues[0] != "#42" || issues[1] != "PROJ-109" {
		t.Fatalf("issues = %v", issues)
	}
}
