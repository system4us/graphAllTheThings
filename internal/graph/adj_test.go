package graph

import (
	"fmt"
	"reflect"
	"testing"
)

// AddEdge must keep an already-built adjacency index in sync: EdgesOf after
// incremental AddEdge calls has to equal EdgesOf after a from-scratch rebuild.
func TestAddEdgeKeepsAdjacencyInSync(t *testing.T) {
	g := New("test")
	for _, id := range []string{"a", "b", "c"} {
		g.AddNode(&Node{ID: id})
	}
	g.AddEdge("a", "b", "CALLS", nil)
	_ = g.EdgesOf("a") // builds the index
	g.AddEdge("b", "c", "CALLS", nil)
	g.AddEdge("c", "c", "CALLS", nil) // self-loop: recorded once
	g.AddNode(&Node{ID: "d"})         // must not invalidate or corrupt it
	g.AddEdge("d", "a", "CALLS", nil)

	for _, id := range []string{"a", "b", "c", "d"} {
		got := g.EdgesOf(id)
		g.adj = nil // force a rebuild for the reference answer
		want := g.EdgesOf(id)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("EdgesOf(%q): incremental %v != rebuilt %v", id, got, want)
		}
	}
	if n := len(g.EdgesOf("c")); n != 2 {
		t.Errorf("self-loop counted twice: EdgesOf(c) has %d edges, want 2", n)
	}
}

// Mirrors resolveAndWire: one EdgesOf then one AddEdge per function.
func BenchmarkEdgesOfThenAddEdge(b *testing.B) {
	for _, n := range []int{2000, 8000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for b.Loop() {
				g := New("bench")
				for i := 0; i < n; i++ {
					g.AddNode(&Node{ID: fmt.Sprint("f", i)})
				}
				for i := 0; i < n; i++ {
					g.AddEdge(fmt.Sprint("f", i), fmt.Sprint("f", (i+1)%n), "CALLS", nil)
				}
				for i := 0; i < n; i++ {
					_ = g.EdgesOf(fmt.Sprint("f", i))
					g.AddEdge(fmt.Sprint("f", i), fmt.Sprint("f", (i+7)%n), "CALLS", nil)
				}
			}
		})
	}
}
