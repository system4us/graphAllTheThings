package search_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system4us/graphAllTheThings/pkg/docs"
	"github.com/system4us/graphAllTheThings/pkg/graph"
	"github.com/system4us/graphAllTheThings/pkg/search"
)

// topicEmbedder is a deterministic semantic mock that projects enterprise
// synonyms into shared dimensions:
//
//	dim 0: procurement ("compras", "adquisicion", "orden", "compra", "factura", "proveedor")
//	dim 1: IT/tech     ("it", "tecnologia", "servidores", "hardware", "licencias", "infraestructura")
//	dim 2: year 2021   ("2021")
//	dim 3: marketing   ("marketing", "campana", "publicidad")
//	dim 4: hr/payroll  ("sueldos", "rrhh", "nomina")
type topicEmbedder struct{}

func (topicEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, raw := range texts {
		s := search.FoldAccents(strings.ToLower(raw))
		v := make([]float32, 5)
		for _, w := range []string{"compras", "compra", "adquisicion", "orden", "factura", "proveedor"} {
			if strings.Contains(s, w) {
				v[0] = 1
				break
			}
		}
		for _, w := range []string{" it", "it.", "it:", "tecnologia", "servidor", "hardware", "licencia", "infraestructura"} {
			if strings.Contains(s, w) {
				v[1] = 1
				break
			}
		}
		if strings.Contains(s, "2021") {
			v[2] = 1
		}
		for _, w := range []string{"marketing", "campana", "publicidad"} {
			if strings.Contains(s, w) {
				v[3] = 1
				break
			}
		}
		for _, w := range []string{"sueldos", "rrhh", "nomina"} {
			if strings.Contains(s, w) {
				v[4] = 1
				break
			}
		}
		out[i] = v
	}
	return out, nil
}

func TestHybridSearchCompras2021ITWithRBACAndExpansion(t *testing.T) {
	ctx := context.Background()
	g := graph.New("enterprise:acme")

	// Entities from declarative connectors.
	g.AddNode(&graph.Node{ID: "dept:it", Type: "department", Name: "IT", Attrs: map[string]string{"folder": "it"}})
	g.AddNode(&graph.Node{ID: "vendor:dell", Type: "vendor", Name: "Dell", Attrs: map[string]string{"folder": "compras"}})
	g.AddNode(&graph.Node{ID: "dept:mkt", Type: "department", Name: "Marketing", Attrs: map[string]string{"folder": "marketing"}})

	// OneDrive doc ingested purely in memory: note the body uses Spanish synonyms
	// ("Adquisición de servidores", "tecnología") without repeating the exact word "compras".
	md1 := []byte("# Adquisición de Servidores 2021\n\nOrden de compra aprobada para hardware e infraestructura de tecnología con proveedor Dell.\n")
	p1, err := docs.ParseBytes(ctx, "infra-2021.md", "text/markdown", md1)
	if err != nil {
		t.Fatal(err)
	}
	docIT, _ := docs.AddToGraph(g, p1, docs.DocOptions{
		ID:           "onedrive:it-2021",
		Name:         "IT/2021/infra-2021.md",
		Attrs:        map[string]string{"web_url": "https://onedrive.example/it-2021"},
		InheritAttrs: map[string]string{"folder": "it", "year": "2021", "department": "IT"},
	})
	g.AddEdge("dept:it", docIT, "owns", nil)
	g.AddEdge(docIT, "vendor:dell", "mentions", nil)

	// Restricted HR document that ALSO contains exact keywords "compras 2021 it"
	// — must be hidden by AllowNode RBAC!
	mdSecret := []byte("# Compras 2021 IT Confidencial\n\nCompras 2021 IT de sueldos y nómina directiva.\n")
	pSecret, err := docs.ParseBytes(ctx, "secret-compras-2021.md", "text/markdown", mdSecret)
	if err != nil {
		t.Fatal(err)
	}
	docSecret, _ := docs.AddToGraph(g, pSecret, docs.DocOptions{
		ID:           "onedrive:hr-secret",
		Name:         "HR/secret-compras-2021.md",
		InheritAttrs: map[string]string{"folder": "hr-private", "year": "2021"},
	})
	g.AddEdge("dept:it", docSecret, "referenced_by", nil)

	// Unrelated Marketing document.
	mdMkt := []byte("# Campaña Publicidad 2023\n\nPresupuesto de marketing digital.\n")
	pMkt, err := docs.ParseBytes(ctx, "mkt-2023.md", "text/markdown", mdMkt)
	if err != nil {
		t.Fatal(err)
	}
	docMkt, _ := docs.AddToGraph(g, pMkt, docs.DocOptions{
		ID:           "onedrive:mkt-2023",
		Name:         "Marketing/mkt-2023.md",
		InheritAttrs: map[string]string{"folder": "marketing", "year": "2023"},
	})
	g.AddEdge("dept:mkt", docMkt, "owns", nil)

	// Persist to SQLite so FTS5 is also exercised.
	dbPath := filepath.Join(t.TempDir(), "enterprise.db")
	if err := g.Save(dbPath); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Build semantic index.
	vs := search.NewMemoryStore()
	nIndexed, err := search.IndexGraph(ctx, g, vs, topicEmbedder{}, nil)
	if err != nil || nIndexed == 0 {
		t.Fatalf("IndexGraph returned n=%d err=%v", nIndexed, err)
	}
	// Second pass must skip unchanged nodes.
	if nAgain, err := search.IndexGraph(ctx, g, vs, topicEmbedder{}, nil); err != nil || nAgain != 0 {
		t.Fatalf("expected 0 re-embedded on second pass, got n=%d err=%v", nAgain, err)
	}

	// Run hybrid search for "compras 2021 it" with RBAC excluding hr-private.
	allow := func(n *graph.Node) bool { return n.Attrs["folder"] != "hr-private" }
	res, err := search.Query(ctx, g, "compras 2021 it", search.Options{
		Limit:      5,
		FTSPath:    dbPath,
		Embedder:   topicEmbedder{},
		Vectors:    vs,
		AllowNode:  allow,
		ExpandHops: 2,
	})
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if res.Method != "hybrid" {
		t.Errorf("expected hybrid search method, got %q", res.Method)
	}
	// Must keep all 3 terms including "it".
	if len(res.Terms) != 3 || res.Terms[0] != "compras" || res.Terms[1] != "2021" || res.Terms[2] != "it" {
		t.Errorf("unexpected terms: %v", res.Terms)
	}
	if len(res.Hits) == 0 {
		t.Fatal("expected search hits")
	}

	// Verify no restricted node leaked in hits or expanded context, and IT 2021 chunk/doc is returned.
	foundIT2021 := false
	for _, h := range res.Hits {
		if strings.Contains(h.Node.ID, "it-2021") {
			foundIT2021 = true
			if h.Context == nil || h.Context.Nodes["onedrive:it-2021"] == nil {
				t.Errorf("expected expanded context for %s to include parent document onedrive:it-2021", h.Node.ID)
			}
		}
		if h.Node.Attrs["folder"] == "hr-private" {
			t.Fatalf("RBAC leak: restricted hit %s returned", h.Node.ID)
		}
		if h.Context != nil {
			for _, cn := range h.Context.Nodes {
				if cn.Attrs["folder"] == "hr-private" {
					t.Fatalf("RBAC leak: restricted node %s appeared in expanded context", cn.ID)
				}
			}
		}
	}
	if !foundIT2021 {
		t.Fatalf("expected onedrive:it-2021 in hybrid search hits, got %+v", res.Hits)
	}

	// Filtering by NodeChunk returns the IT 2021 chunk first (never HR secret or Marketing 2023).
	chunkRes, err := search.Query(ctx, g, "compras 2021 it", search.Options{
		NodeType:   graph.NodeChunk,
		Limit:      5,
		FTSPath:    dbPath,
		Embedder:   topicEmbedder{},
		Vectors:    vs,
		AllowNode:  allow,
		ExpandHops: 2,
	})
	if err != nil {
		t.Fatalf("chunk Query failed: %v", err)
	}
	if len(chunkRes.Hits) == 0 || !strings.Contains(chunkRes.Hits[0].Node.ID, "it-2021") {
		t.Fatalf("expected top chunk hit from it-2021, got %+v", chunkRes.Hits)
	}
}
