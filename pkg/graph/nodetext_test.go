package graph

import (
	"strings"
	"testing"
)

func TestNodeTextIndependentOfEdgeOrder(t *testing.T) {
	build := func(callers []string) string {
		g := New("test")
		g.AddNode(&Node{ID: "target", Type: NodeFunction, Name: "target"})
		for _, c := range callers {
			g.AddNode(&Node{ID: c, Type: NodeFunction, Name: c})
		}
		for _, c := range callers {
			g.AddEdge(c, "target", EdgeCalls, nil)
		}
		return g.NodeText("target")
	}
	a := build([]string{"alpha", "beta", "gamma"})
	b := build([]string{"gamma", "alpha", "beta"})
	if a != b {
		t.Errorf("text depends on edge order:\n  %q\n  %q", a, b)
	}
	if want := "Called by: alpha, beta, gamma."; !strings.Contains(a, want) {
		t.Errorf("text %q does not contain %q", a, want)
	}
}

func TestNodeTextCustomDomainAndHook(t *testing.T) {
	g := New("enterprise:acme")
	g.AddNode(&Node{
		ID:   "ent:dept-it",
		Type: "department",
		Name: "IT",
		Attrs: map[string]string{
			"folder":  "it",
			"summary": "Infraestructura y compras tecnológicas",
		},
	})
	g.AddNode(&Node{
		ID:   "ent:vendor-dell",
		Type: "vendor",
		Name: "Dell",
		Attrs: map[string]string{
			"folder": "compras",
			"year":   "2021",
		},
	})
	g.AddEdge("ent:vendor-dell", "ent:dept-it", "supplies", nil)

	txt := g.NodeText("ent:dept-it")
	for _, want := range []string{"department IT.", "folder: it.", "summary: Infraestructura", "supplies from: Dell."} {
		if !strings.Contains(txt, want) {
			t.Errorf("NodeText(ent:dept-it) = %q, missing %q", txt, want)
		}
	}

	g.NodeTextFunc = func(_ *Graph, n *Node) string {
		return "CUSTOM:" + n.Name
	}
	if got := g.NodeText("ent:dept-it"); got != "CUSTOM:IT" {
		t.Errorf("NodeTextFunc override failed: got %q", got)
	}
}
