package graph_test

import (
	"fmt"
	"path/filepath"
	"os"

	"github.com/system4us/graphAllTheThings/pkg/graph"
)

func Example() {
	g := graph.New("enterprise:acme")
	g.AddNode(&graph.Node{ID: "dept:it", Type: "department", Name: "IT", Attrs: map[string]string{"folder": "it"}})
	g.AddNode(&graph.Node{ID: "doc:compras-2021", Type: graph.NodeDocument, Name: "Compras IT 2021", Attrs: map[string]string{"folder": "it", "year": "2021"}})
	g.AddNode(&graph.Node{ID: "vendor:acme", Type: "vendor", Name: "Acme Hardware", Attrs: map[string]string{"folder": "compras"}})

	g.AddEdge("dept:it", "doc:compras-2021", "owns", nil)
	g.AddEdge("doc:compras-2021", "vendor:acme", "mentions", nil)

	dir, _ := os.MkdirTemp("", "gatt-graph-example-*")
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "graph.db")
	_ = g.Save(dbPath)

	path := g.FindPath("dept:it", "vendor:acme", graph.TraverseOptions{
		Direction: graph.DirectionOut,
		AllowNode: func(n *graph.Node) bool { return n.Attrs["folder"] != "restricted" },
	})
	fmt.Println("hops:", len(path))

	hits := graph.FTSQueryTerms(dbPath, []string{"compras", "2021", "it"}, graph.NodeDocument, 5)
	fmt.Println("top hit:", hits[0])
	// Output:
	// hops: 2
	// top hit: doc:compras-2021
}
