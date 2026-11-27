package graph

import (
	"path/filepath"
	"strings"
	"testing"
)

func buildEnterpriseGraph() *Graph {
	g := New("enterprise:acme")
	g.AddNode(&Node{ID: "dept:it", Type: "department", Name: "IT", Attrs: map[string]string{"folder": "it"}})
	g.AddNode(&Node{ID: "emp:ana", Type: "employee", Name: "Ana", Attrs: map[string]string{"folder": "it"}})
	g.AddNode(&Node{ID: "emp:hr-secret", Type: "employee", Name: "Secret HR", Attrs: map[string]string{"folder": "hr-private"}})
	g.AddNode(&Node{ID: "proj:erp", Type: "project", Name: "ERP 2021", Attrs: map[string]string{"folder": "it"}})
	g.AddNode(&Node{ID: "doc:compras-2021", Type: NodeDocument, Name: "Compras IT 2021", Attrs: map[string]string{"folder": "it", "year": "2021", "summary": "Adquisición de servidores y licencias"}})
	g.AddNode(&Node{ID: "vendor:dell", Type: "vendor", Name: "Dell", Attrs: map[string]string{"folder": "compras"}})

	g.AddEdge("emp:ana", "dept:it", "member_of", nil)
	g.AddEdge("emp:ana", "proj:erp", "works_on", nil)
	g.AddEdge("proj:erp", "doc:compras-2021", "has_doc", nil)
	g.AddEdge("doc:compras-2021", "vendor:dell", "mentions", nil)
	// Shorter path via restricted HR folder:
	g.AddEdge("emp:ana", "emp:hr-secret", "reports_to", nil)
	g.AddEdge("emp:hr-secret", "vendor:dell", "approved_vendor", nil)
	return g
}

func TestOutAndInEdges(t *testing.T) {
	g := buildEnterpriseGraph()
	if got := len(g.OutEdges("emp:ana")); got != 3 {
		t.Fatalf("OutEdges(emp:ana) = %d, want 3", got)
	}
	if got := len(g.InEdges("emp:ana")); got != 0 {
		t.Fatalf("InEdges(emp:ana) = %d, want 0", got)
	}
	if got := len(g.InEdges("vendor:dell")); got != 2 {
		t.Fatalf("InEdges(vendor:dell) = %d, want 2", got)
	}
}

func TestFindPathRBACAndDirection(t *testing.T) {
	g := buildEnterpriseGraph()

	// Unrestricted: shortest path cuts through emp:hr-secret (2 hops).
	pAll := g.FindPath("emp:ana", "vendor:dell", TraverseOptions{Direction: DirectionOut})
	if len(pAll) != 2 || pAll[0].To != "emp:hr-secret" {
		t.Fatalf("expected 2-hop unrestricted path via hr-secret, got %+v", pAll)
	}

	// RBAC filter excluding hr-private must route around it (3 hops: ana -> erp -> compras-2021 -> dell).
	allow := func(n *Node) bool { return n.Attrs["folder"] != "hr-private" }
	pRBAC := g.FindPath("emp:ana", "vendor:dell", TraverseOptions{
		Direction: DirectionOut,
		AllowNode: allow,
	})
	if len(pRBAC) != 3 {
		t.Fatalf("expected 3-hop RBAC path, got %d: %+v", len(pRBAC), pRBAC)
	}
	for _, e := range pRBAC {
		if e.From == "emp:hr-secret" || e.To == "emp:hr-secret" {
			t.Fatalf("RBAC path leaked restricted node: %+v", pRBAC)
		}
	}

	// DirectionIn from emp:ana cannot reach vendor:dell.
	if pIn := g.FindPath("emp:ana", "vendor:dell", TraverseOptions{Direction: DirectionIn}); pIn != nil {
		t.Fatalf("expected nil for DirectionIn, got %+v", pIn)
	}

	// MaxDepth 2 with RBAC cannot reach vendor:dell.
	if pShort := g.FindPath("emp:ana", "vendor:dell", TraverseOptions{Direction: DirectionOut, MaxDepth: 2, AllowNode: allow}); pShort != nil {
		t.Fatalf("expected nil with MaxDepth=2, got %+v", pShort)
	}
}

func TestSubgraphAndReachable(t *testing.T) {
	g := buildEnterpriseGraph()
	allow := func(n *Node) bool { return n.Attrs["folder"] != "hr-private" }

	sub := g.Subgraph("emp:ana", TraverseOptions{
		Direction: DirectionOut,
		MaxDepth:  2,
		AllowNode: allow,
	})
	// Expected nodes within 2 outgoing hops: emp:ana, dept:it, proj:erp, doc:compras-2021.
	if len(sub.Nodes) != 4 {
		t.Fatalf("Subgraph nodes = %d, want 4 (%v)", len(sub.Nodes), sub.Nodes)
	}
	if sub.Nodes["emp:hr-secret"] != nil {
		t.Fatal("Subgraph leaked restricted node emp:hr-secret")
	}
	if sub.Nodes["vendor:dell"] != nil {
		t.Fatal("Subgraph exceeded MaxDepth=2")
	}
	if len(sub.Edges) != 3 {
		t.Fatalf("Subgraph edges = %d, want 3", len(sub.Edges))
	}

	reach := g.Reachable("emp:ana", TraverseOptions{
		Direction: DirectionOut,
		MaxDepth:  2,
		AllowNode: allow,
	})
	if len(reach) != 3 {
		t.Fatalf("Reachable = %d, want 3", len(reach))
	}
	// Hop 1 nodes (dept:it, proj:erp) come before hop 2 (doc:compras-2021).
	if reach[2].ID != "doc:compras-2021" {
		t.Errorf("expected hop-2 node last, got %s", reach[2].ID)
	}
}

func TestSQLitePersistenceQueryByTypeAndFTS(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "context.db")

	g := buildEnterpriseGraph()
	if err := g.Save(dbPath); err != nil {
		t.Fatalf("Save SQLite failed: %v", err)
	}

	meta, err := LoadMeta(dbPath)
	if err != nil {
		t.Fatalf("LoadMeta failed: %v", err)
	}
	if meta["source"] != "enterprise:acme" {
		t.Errorf("meta source = %q, want enterprise:acme", meta["source"])
	}

	docs, err := QueryNodesByType(dbPath, NodeDocument)
	if err != nil {
		t.Fatalf("QueryNodesByType failed: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "doc:compras-2021" {
		t.Fatalf("QueryNodesByType(document) = %+v", docs)
	}

	hits := FTSQueryTerms(dbPath, []string{"compras", "2021", "it"}, NodeDocument, 5)
	if len(hits) == 0 || hits[0] != "doc:compras-2021" {
		t.Fatalf("FTSQueryTerms(compras 2021 it, document) = %v, want doc:compras-2021 first", hits)
	}
	allHits := FTSQueryTerms(dbPath, []string{"compras", "2021", "it"}, "", 5)
	foundDoc := false
	for _, id := range allHits {
		if id == "doc:compras-2021" {
			foundDoc = true
		}
	}
	if !foundDoc {
		t.Fatalf("FTSQueryTerms(compras 2021 it) = %v, expected doc:compras-2021 in results", allHits)
	}

	// Incremental journaled delta save: remove vendor:dell and add vendor:hp.
	loaded, err := LoadRaw(dbPath)
	if err != nil {
		t.Fatalf("LoadRaw failed: %v", err)
	}
	loaded.StartJournal()
	removed := loaded.RemoveNodesWhere(func(n *Node) bool { return n.ID == "vendor:dell" })
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	loaded.AddNode(&Node{ID: "vendor:hp", Type: "vendor", Name: "HP", Attrs: map[string]string{"folder": "compras"}})
	loaded.AddEdge("doc:compras-2021", "vendor:hp", "mentions", nil)
	if added := loaded.JournalAddedNodeIDs(); len(added) != 1 || added[0] != "vendor:hp" {
		t.Fatalf("JournalAddedNodeIDs = %v, want [vendor:hp]", added)
	}
	if err := loaded.Save(dbPath); err != nil {
		t.Fatalf("delta Save failed: %v", err)
	}

	reloaded, err := LoadRaw(dbPath)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.Nodes["vendor:dell"] != nil || reloaded.Nodes["vendor:hp"] == nil {
		t.Fatalf("delta save mismatch: dell=%v hp=%v", reloaded.Nodes["vendor:dell"], reloaded.Nodes["vendor:hp"])
	}

	// DiffGraphs detects the change cleanly.
	diff := DiffGraphs(g, reloaded)
	if diff.Empty() {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(diff.Text(), "+ vendor:hp") || !strings.Contains(diff.Text(), "- vendor:dell") {
		t.Errorf("unexpected diff text:\n%s", diff.Text())
	}
}
