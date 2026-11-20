package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"graphallthethings/internal/graph"
)

func TestTracePathAcrossRouteDispatchAndSqlc(t *testing.T) {
	dir := t.TempDir()
	srcFile := filepath.Join(dir, "orders.go")
	if err := os.WriteFile(srcFile, []byte("package app\n\nfunc HandleCheckout() {\n\tcheckout()\n}\n\nfunc checkout() {\n\t// emits order.paid\n}\n\nfunc sendReceipt() {\n\t// sends email\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("codebase:" + dir)
	g.AddNode(&graph.Node{ID: "route:orders.go:POST:1", Type: graph.NodeRoute, Name: "POST /checkout", Attrs: map[string]string{"file": "orders.go", "line_start": "1"}})
	g.AddNode(&graph.Node{ID: "func:orders.go:HandleCheckout:3", Type: graph.NodeFunction, Name: "HandleCheckout", Attrs: map[string]string{"file": "orders.go", "line_start": "3", "line_end": "5"}})
	g.AddNode(&graph.Node{ID: "func:orders.go:checkout:7", Type: graph.NodeFunction, Name: "checkout", Attrs: map[string]string{"file": "orders.go", "line_start": "7", "line_end": "9", "invariant": "must run inside tx", "gotcha": "idempotent by order_id"}})
	g.AddNode(&graph.Node{ID: "func:orders.go:sendReceipt:11", Type: graph.NodeFunction, Name: "sendReceipt", Attrs: map[string]string{"file": "orders.go", "line_start": "11", "line_end": "13"}})
	g.AddNode(&graph.Node{ID: "model:orders", Type: graph.NodeModel, Name: "orders", Attrs: map[string]string{"table": "orders"}})

	g.AddEdge("route:orders.go:POST:1", "func:orders.go:HandleCheckout:3", graph.EdgeHandledBy, nil)
	g.AddEdge("func:orders.go:HandleCheckout:3", "func:orders.go:checkout:7", graph.EdgeCalls, nil)
	g.AddEdge("func:orders.go:checkout:7", "func:orders.go:sendReceipt:11", graph.EdgeCalls, map[string]string{"inferred": "true", "via": "dispatch", "event": "order.paid"})
	g.AddEdge("func:orders.go:sendReceipt:11", "model:orders", graph.EdgeUsesModel, map[string]string{"via": "sqlc", "query": "GetOrder"})

	e := New(g, nil, nil)

	out, err := e.TracePath("POST /checkout", "orders", 8)
	if err != nil {
		t.Fatalf("TracePath: %v", err)
	}
	for _, want := range []string{
		"4 hop(s), directed",
		"handled by",
		`emits "order.paid" (inferred)`,
		"uses model (sqlc:GetOrder)",
		"[model] orders",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("TracePath output missing %q:\n%s", want, out)
		}
	}

	// ReadSymbols batch read with annotations
	symOut, err := e.ReadSymbols([]string{"checkout", "sendReceipt"}, 2000)
	if err != nil {
		t.Fatalf("ReadSymbols: %v", err)
	}
	for _, want := range []string{
		"## checkout [function]",
		"invariant: must run inside tx",
		"gotcha: idempotent by order_id",
		"## sendReceipt [function]",
		"// sends email",
	} {
		if !strings.Contains(symOut, want) {
			t.Errorf("ReadSymbols output missing %q:\n%s", want, symOut)
		}
	}

	// Impact surfaces invariant and gotcha
	impOut, err := e.Impact("checkout", 2)
	if err != nil {
		t.Fatalf("Impact: %v", err)
	}
	if !strings.Contains(impOut, "invariant: must run inside tx") || !strings.Contains(impOut, "gotcha: idempotent by order_id") {
		t.Errorf("Impact output missing annotations:\n%s", impOut)
	}
}
