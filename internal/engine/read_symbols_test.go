package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system4us/graphAllTheThings/pkg/graph"
)

// read_symbols must say "cut" only when it cut, and never drop a requested
// symbol silently when the budget runs out.
func TestReadSymbolsTruncationAndBudget(t *testing.T) {
	dir := t.TempDir()
	var long strings.Builder
	long.WriteString("func long() {\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&long, "\tx%d := %d\n", i, i)
	}
	long.WriteString("}\n")
	src := "package p\n\nfunc small() int {\n\treturn 1\n}\n\nfunc other() int {\n\treturn 2\n}\n\n" + long.String()
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	g := graph.New("codebase:" + dir)
	add := func(name string, ls, le int) {
		g.AddNode(&graph.Node{ID: fmt.Sprintf("func:p.go:%s:%d", name, ls), Type: graph.NodeFunction, Name: name,
			Attrs: map[string]string{"file": "p.go", "line_start": fmt.Sprint(ls), "line_end": fmt.Sprint(le)}})
	}
	add("small", 3, 5)
	add("other", 7, 9)
	add("long", 11, 312)
	e := New(g, nil, nil)

	out, err := e.ReadSymbols([]string{"small", "other"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "cut") || strings.Contains(out, "truncated") || strings.Contains(out, "not shown") {
		t.Errorf("complete symbols must not be reported as cut:\n%s", out)
	}
	if !strings.Contains(out, "   5: }") || !strings.Contains(out, "   9: }") {
		t.Errorf("both bodies expected in full:\n%s", out)
	}

	out, _ = e.ReadSymbols([]string{"long"}, 100000)
	if !strings.Contains(out, "cut at the 250-line cap; the symbol continues at p.go:261-312") {
		t.Errorf("a symbol over the line cap must say where it continues:\n%s", out)
	}

	// The oversized symbol is skipped and listed; smaller ones after it that
	// still fit are shown.
	out, _ = e.ReadSymbols([]string{"small", "long", "other"}, 60)
	for _, want := range []string{"## small", "## other", "not shown", "long (p.go:11-312)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "## long") {
		t.Errorf("long does not fit the budget and must only be listed:\n%s", out)
	}

	// A first symbol bigger than the whole budget is cut with a resume line.
	out, _ = e.ReadSymbols([]string{"long"}, 100)
	if !strings.Contains(out, "trimmed to the token budget; continue from line") || strings.Count(out, "```")%2 != 0 {
		t.Errorf("oversized first symbol must be cut cleanly with a resume point:\n%s", out)
	}
}
