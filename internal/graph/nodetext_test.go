package graph

import "testing"

// The same graph content must give the same embedding text no matter in which
// order the edges were added (fresh extraction vs incremental refresh).
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
	if want := "Called by: alpha, beta, gamma."; !contains(a, want) {
		t.Errorf("text %q does not contain %q", a, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
