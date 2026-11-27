package engine

import (
	"strings"
	"testing"

	"github.com/system4us/graphAllTheThings/pkg/graph"
)

// trace answers, in order: forward path, reversed path, common caller /
// shared dependency — and never stitches a path through a test that happens
// to call both ends.
func TestTraceReversedCommonAndNoTestHops(t *testing.T) {
	g := graph.New("codebase:/tmp/fake")
	fn := func(name, file string) {
		g.AddNode(&graph.Node{ID: "func:" + file + ":" + name + ":1", Type: graph.NodeFunction, Name: name,
			Attrs: map[string]string{"file": file, "line_start": "1"}})
	}
	call := func(a, fa, b, fb string) {
		g.AddEdge("func:"+fa+":"+a+":1", "func:"+fb+":"+b+":1", graph.EdgeCalls, nil)
	}
	fn("handler", "api.go")
	fn("save", "store.go")
	fn("insert", "audit.go")
	fn("verify", "verify.go")
	fn("hash", "chain.go")
	fn("render", "ui.go")
	fn("encode", "codec.go")
	fn("TestBoth", "x_test.go")
	call("handler", "api.go", "save", "store.go")
	call("insert", "audit.go", "hash", "chain.go")
	call("verify", "verify.go", "hash", "chain.go")
	call("TestBoth", "x_test.go", "render", "ui.go")
	call("TestBoth", "x_test.go", "encode", "codec.go")
	e := New(g, nil, nil)

	out, _ := e.TracePath("save", "handler", 8)
	if !strings.Contains(out, "reversed") || !strings.Contains(out, "handler") {
		t.Errorf("save is reached from handler: want a reversed path, got:\n%s", out)
	}
	out, _ = e.TracePath("insert", "verify", 8)
	if !strings.Contains(out, "both lead to [function] hash") {
		t.Errorf("insert and verify share hash: want a shared-dependency answer, got:\n%s", out)
	}
	out, _ = e.TracePath("render", "encode", 8)
	if !strings.HasPrefix(out, "no path found") || strings.Contains(out, "TestBoth") {
		t.Errorf("only a test connects render and encode: want no path, got:\n%s", out)
	}
}
