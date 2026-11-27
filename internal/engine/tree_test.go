package engine

import (
	"strings"
	"testing"

	"github.com/system4us/graphAllTheThings/pkg/graph"
)

// tree prints every directory as its own line, and summarizes a file by its
// header comment (above all declarations) before its first function's doc.
func TestTreeDirectoriesAndHeaderComment(t *testing.T) {
	g := graph.New("codebase:/tmp/fake")
	file := func(rel string) {
		g.AddNode(&graph.Node{ID: "file:/tmp/fake/" + rel, Type: graph.NodeFile, Name: rel,
			Attrs: map[string]string{"path": "/tmp/fake/" + rel}})
	}
	file("audit/chain.go")
	file("audit/db/db.go")
	file("audit/verify.go")
	g.AddNode(&graph.Node{ID: "comment:audit/chain.go:3", Type: graph.NodeComment, Name: "Per-event hash chain",
		Attrs: map[string]string{"file": "audit/chain.go", "line": "3", "text": "Per-event hash chain: each event stores prev_hash."}})
	g.AddNode(&graph.Node{ID: "comment:audit/chain.go:40", Type: graph.NodeComment, Name: "later note",
		Attrs: map[string]string{"file": "audit/chain.go", "line": "40", "text": "A note inside the file, below a declaration."}})
	g.AddNode(&graph.Node{ID: "func:audit/chain.go:chainVersionOf:20", Type: graph.NodeFunction, Name: "chainVersionOf",
		Attrs: map[string]string{"file": "audit/chain.go", "line_start": "20", "doc": "chainVersionOf is the chain version."}})
	g.AddNode(&graph.Node{ID: "func:audit/verify.go:Verify:10", Type: graph.NodeFunction, Name: "Verify",
		Attrs: map[string]string{"file": "audit/verify.go", "line_start": "10", "doc": "Verify checks the log."}})
	g.AddNode(&graph.Node{ID: "comment:audit/verify.go:30", Type: graph.NodeComment, Name: "inner",
		Attrs: map[string]string{"file": "audit/verify.go", "line": "30", "text": "An inner comment below Verify, not a header."}})
	e := New(g, nil, nil)

	out, err := e.Tree("audit", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"chain.go  — Per-event hash chain",
		"db/\n  db.go",
		"verify.go  — Verify checks the log.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tree missing %q:\n%s", want, out)
		}
	}
}
