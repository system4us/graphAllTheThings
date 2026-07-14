// Package graph defines the semantic metadata graph: typed nodes and edges
// extracted from a source (database schema, OpenAPI spec, ...), with
// traversal helpers used by the MCP tools.
package graph

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Node types
const (
	NodeDatabase = "database"
	NodeTable    = "table"
	NodeColumn   = "column"
	NodeView     = "view"
	NodeIndex    = "index"
)

// Edge types
const (
	EdgeHasTable   = "HAS_TABLE"
	EdgeHasColumn  = "HAS_COLUMN"
	EdgeHasIndex   = "HAS_INDEX"
	EdgeIndexes    = "INDEXES"
	EdgeForeignKey = "FOREIGN_KEY" // column -> column
	EdgeReferences = "REFERENCES"  // table -> table (derived from FKs) or view -> table
)

type Node struct {
	ID    string            `json:"id"`
	Type  string            `json:"type"`
	Name  string            `json:"name"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

type Edge struct {
	From  string            `json:"from"`
	To    string            `json:"to"`
	Type  string            `json:"type"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

type Graph struct {
	Source string           `json:"source"` // e.g. "sqlite:/path/to/db"
	Nodes  map[string]*Node `json:"nodes"`
	Edges  []Edge           `json:"edges"`

	adj map[string][]int // node id -> edge indexes (both directions), built lazily
}

func New(source string) *Graph {
	return &Graph{Source: source, Nodes: map[string]*Node{}}
}

func (g *Graph) AddNode(n *Node) {
	if n.Attrs == nil {
		n.Attrs = map[string]string{}
	}
	g.Nodes[n.ID] = n
	g.adj = nil
}

func (g *Graph) AddEdge(from, to, typ string, attrs map[string]string) {
	g.Edges = append(g.Edges, Edge{From: from, To: to, Type: typ, Attrs: attrs})
	g.adj = nil
}

func (g *Graph) buildAdj() {
	if g.adj != nil {
		return
	}
	g.adj = map[string][]int{}
	for i, e := range g.Edges {
		g.adj[e.From] = append(g.adj[e.From], i)
		g.adj[e.To] = append(g.adj[e.To], i)
	}
}

// EdgesOf returns all edges touching the node, in both directions.
func (g *Graph) EdgesOf(id string) []Edge {
	g.buildAdj()
	var out []Edge
	for _, i := range g.adj[id] {
		out = append(out, g.Edges[i])
	}
	return out
}

// ShortestPath finds the shortest undirected path between two nodes using
// only the given edge types (all types if none given). Returns the edges
// along the path, or nil if unreachable.
func (g *Graph) ShortestPath(from, to string, edgeTypes ...string) []Edge {
	g.buildAdj()
	if g.Nodes[from] == nil || g.Nodes[to] == nil {
		return nil
	}
	allowed := map[string]bool{}
	for _, t := range edgeTypes {
		allowed[t] = true
	}
	type step struct {
		node string
		edge int // edge index used to arrive, -1 for start
		prev int // index into visitedOrder
	}
	order := []step{{from, -1, -1}}
	seen := map[string]bool{from: true}
	for qi := 0; qi < len(order); qi++ {
		cur := order[qi]
		if cur.node == to {
			var path []Edge
			for s := cur; s.edge >= 0; s = order[s.prev] {
				path = append([]Edge{g.Edges[s.edge]}, path...)
			}
			return path
		}
		for _, ei := range g.adj[cur.node] {
			e := g.Edges[ei]
			if len(allowed) > 0 && !allowed[e.Type] {
				continue
			}
			next := e.To
			if next == cur.node {
				next = e.From
			}
			if !seen[next] {
				seen[next] = true
				order = append(order, step{next, ei, qi})
			}
		}
	}
	return nil
}

// CheapestPath is Dijkstra over undirected edges of the given types, where
// passing *through* an intermediate node costs nodeCost(id) on top of the
// hop. Endpoints are free. Used to route join paths around hub tables
// (tenant_id-style FKs that every table carries) which BFS would happily
// cut through.
func (g *Graph) CheapestPath(from, to string, nodeCost func(id string) float64, edgeTypes ...string) []Edge {
	g.buildAdj()
	if g.Nodes[from] == nil || g.Nodes[to] == nil {
		return nil
	}
	allowed := map[string]bool{}
	for _, t := range edgeTypes {
		allowed[t] = true
	}
	dist := map[string]float64{from: 0}
	prevEdge := map[string]int{}
	prevNode := map[string]string{}
	done := map[string]bool{}
	for {
		cur, best := "", 0.0
		for id, d := range dist {
			if !done[id] && (cur == "" || d < best) {
				cur, best = id, d
			}
		}
		if cur == "" || cur == to {
			break
		}
		done[cur] = true
		for _, ei := range g.adj[cur] {
			e := g.Edges[ei]
			if len(allowed) > 0 && !allowed[e.Type] {
				continue
			}
			next := e.To
			if next == cur {
				next = e.From
			}
			cost := best + 1
			if next != to {
				cost += nodeCost(next)
			}
			if d, ok := dist[next]; !ok || cost < d {
				dist[next] = cost
				prevEdge[next] = ei
				prevNode[next] = cur
			}
		}
	}
	if _, ok := dist[to]; !ok {
		return nil
	}
	var path []Edge
	for at := to; at != from; at = prevNode[at] {
		path = append([]Edge{g.Edges[prevEdge[at]]}, path...)
	}
	return path
}

// NodesByType returns nodes of the given type sorted by name.
func (g *Graph) NodesByType(typ string) []*Node {
	var out []*Node
	for _, n := range g.Nodes {
		if n.Type == typ {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NodeText builds the natural-language description of a node used for
// embedding and keyword search: name, type, attributes and immediate
// relationships.
func (g *Graph) NodeText(id string) string {
	n := g.Nodes[id]
	if n == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s.", n.Type, n.Name)
	if c := n.Attrs["comment"]; c != "" {
		fmt.Fprintf(&b, " %s.", c)
	}
	if ev := n.Attrs["enum_values"]; ev != "" {
		fmt.Fprintf(&b, " Allowed values: %s.", ev)
	}
	if dt := n.Attrs["data_type"]; dt != "" {
		fmt.Fprintf(&b, " Type %s.", dt)
	}
	var cols, refs, refBy []string
	for _, e := range g.EdgesOf(id) {
		other := e.To
		if other == id {
			other = e.From
		}
		on := g.Nodes[other]
		if on == nil {
			continue
		}
		switch {
		case e.Type == EdgeHasColumn && e.From == id:
			cols = append(cols, on.Name)
		case e.Type == EdgeReferences && e.From == id:
			refs = append(refs, on.Name)
		case e.Type == EdgeReferences && e.To == id:
			refBy = append(refBy, on.Name)
		}
	}
	if len(cols) > 0 {
		fmt.Fprintf(&b, " Columns: %s.", strings.Join(cols, ", "))
	}
	if len(refs) > 0 {
		fmt.Fprintf(&b, " References: %s.", strings.Join(refs, ", "))
	}
	if len(refBy) > 0 {
		fmt.Fprintf(&b, " Referenced by: %s.", strings.Join(refBy, ", "))
	}
	return b.String()
}

func (g *Graph) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func Load(path string) (*Graph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var g Graph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, err
	}
	if g.Nodes == nil {
		g.Nodes = map[string]*Node{}
	}
	return &g, nil
}
