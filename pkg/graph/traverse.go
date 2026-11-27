package graph

import (
	"maps"
	"sort"
)

// Direction controls which edge orientations a traversal follows.
type Direction int

const (
	// DirectionBoth follows edges in both directions (undirected). Zero value.
	DirectionBoth Direction = iota
	// DirectionOut follows only outgoing edges (e.From == current).
	DirectionOut
	// DirectionIn follows only incoming edges (e.To == current).
	DirectionIn
)

// TraverseOptions configures pathfinding and multi-hop subgraph queries.
type TraverseOptions struct {
	// EdgeTypes restricts traversal to these edge types. Empty allows all types.
	EdgeTypes []string
	// Direction selects undirected (DirectionBoth, default), outgoing, or incoming.
	Direction Direction
	// MaxDepth limits hop count from the start node. 0 means unlimited.
	MaxDepth int
	// AllowNode filters which nodes may be visited or returned. Nil allows all.
	// Useful for folder-scoped RBAC so restricted nodes are never traversed through.
	AllowNode func(*Node) bool
	// NodeCost adds extra traversal cost when passing through an intermediate
	// node in FindPath (endpoints are free). Nil uses unweighted BFS.
	NodeCost func(*Node) float64
}

func (o TraverseOptions) edgeAllowed(allowed map[string]bool, typ string) bool {
	return len(allowed) == 0 || allowed[typ]
}

func (o TraverseOptions) nodeAllowed(n *Node) bool {
	if n == nil {
		return false
	}
	return o.AllowNode == nil || o.AllowNode(n)
}

func makeAllowedMap(types []string) map[string]bool {
	if len(types) == 0 {
		return nil
	}
	m := make(map[string]bool, len(types))
	for _, t := range types {
		m[t] = true
	}
	return m
}

// stepNeighbor returns the next node ID if edge e can be traversed from cur
// under dir, or ("", false) otherwise.
func stepNeighbor(e Edge, cur string, dir Direction) (string, bool) {
	switch dir {
	case DirectionOut:
		if e.From == cur {
			return e.To, true
		}
		return "", false
	case DirectionIn:
		if e.To == cur {
			return e.From, true
		}
		return "", false
	default:
		if e.From == cur {
			return e.To, true
		}
		if e.To == cur {
			return e.From, true
		}
		return "", false
	}
}

// FindPath finds the shortest (or lowest-cost when opts.NodeCost != nil) path
// between from and to honoring opts.Direction, opts.EdgeTypes, opts.MaxDepth,
// and opts.AllowNode. Returns nil if unreachable or either endpoint is disallowed.
func (g *Graph) FindPath(from, to string, opts TraverseOptions) []Edge {
	adj := g.buildAdj()
	startNode, endNode := g.Nodes[from], g.Nodes[to]
	if !opts.nodeAllowed(startNode) || !opts.nodeAllowed(endNode) {
		return nil
	}
	if from == to {
		return []Edge{}
	}
	allowed := makeAllowedMap(opts.EdgeTypes)
	if opts.NodeCost != nil {
		return g.findCheapestPath(adj, from, to, allowed, opts)
	}
	type step struct {
		node  string
		edge  int
		prev  int
		depth int
	}
	order := []step{{node: from, edge: -1, prev: -1, depth: 0}}
	seen := map[string]bool{from: true}

	g.adjMu.RLock()
	defer g.adjMu.RUnlock()

	for qi := 0; qi < len(order); qi++ {
		cur := order[qi]
		if cur.node == to {
			var path []Edge
			for s := cur; s.edge >= 0; s = order[s.prev] {
				path = append(path, g.Edges[s.edge])
			}
			for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
				path[i], path[j] = path[j], path[i]
			}
			return path
		}
		if opts.MaxDepth > 0 && cur.depth >= opts.MaxDepth {
			continue
		}
		for _, ei := range adj[cur.node] {
			e := g.Edges[ei]
			if !opts.edgeAllowed(allowed, e.Type) {
				continue
			}
			next, ok := stepNeighbor(e, cur.node, opts.Direction)
			if !ok || seen[next] {
				continue
			}
			if !opts.nodeAllowed(g.Nodes[next]) {
				continue
			}
			seen[next] = true
			order = append(order, step{node: next, edge: ei, prev: qi, depth: cur.depth + 1})
		}
	}
	return nil
}

func (g *Graph) findCheapestPath(adj map[string][]int, from, to string, allowed map[string]bool, opts TraverseOptions) []Edge {
	g.adjMu.RLock()
	defer g.adjMu.RUnlock()

	dist := map[string]float64{from: 0}
	hops := map[string]int{from: 0}
	prevEdge := map[string]int{}
	prevNode := map[string]string{}
	done := map[string]bool{}

	for {
		cur, best := "", 0.0
		for id, d := range dist {
			if !done[id] && (cur == "" || d < best || (d == best && id < cur)) {
				cur, best = id, d
			}
		}
		if cur == "" || cur == to {
			break
		}
		done[cur] = true
		if opts.MaxDepth > 0 && hops[cur] >= opts.MaxDepth {
			continue
		}
		for _, ei := range adj[cur] {
			e := g.Edges[ei]
			if !opts.edgeAllowed(allowed, e.Type) {
				continue
			}
			next, ok := stepNeighbor(e, cur, opts.Direction)
			if !ok {
				continue
			}
			nextNode := g.Nodes[next]
			if !opts.nodeAllowed(nextNode) {
				continue
			}
			cost := best + 1
			if next != to {
				cost += opts.NodeCost(nextNode)
			}
			if d, ok := dist[next]; !ok || cost < d {
				dist[next] = cost
				hops[next] = hops[cur] + 1
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
		path = append(path, g.Edges[prevEdge[at]])
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// Reachable returns all allowed nodes reachable from startID (excluding startID
// itself), ordered by hop distance ascending and then by node ID.
func (g *Graph) Reachable(startID string, opts TraverseOptions) []*Node {
	adj := g.buildAdj()
	startNode := g.Nodes[startID]
	if !opts.nodeAllowed(startNode) {
		return nil
	}
	allowed := makeAllowedMap(opts.EdgeTypes)
	type item struct {
		id    string
		depth int
	}
	queue := []item{{id: startID, depth: 0}}
	depthOf := map[string]int{startID: 0}

	g.adjMu.RLock()
	for qi := 0; qi < len(queue); qi++ {
		cur := queue[qi]
		if opts.MaxDepth > 0 && cur.depth >= opts.MaxDepth {
			continue
		}
		for _, ei := range adj[cur.id] {
			e := g.Edges[ei]
			if !opts.edgeAllowed(allowed, e.Type) {
				continue
			}
			next, ok := stepNeighbor(e, cur.id, opts.Direction)
			if !ok {
				continue
			}
			if _, seen := depthOf[next]; seen {
				continue
			}
			if !opts.nodeAllowed(g.Nodes[next]) {
				continue
			}
			depthOf[next] = cur.depth + 1
			queue = append(queue, item{id: next, depth: cur.depth + 1})
		}
	}
	g.adjMu.RUnlock()

	out := make([]*Node, 0, len(depthOf)-1)
	for id := range depthOf {
		if id == startID {
			continue
		}
		out = append(out, g.Nodes[id])
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := depthOf[out[i].ID], depthOf[out[j].ID]
		if di != dj {
			return di < dj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Subgraph returns a new Graph containing startID, every allowed node reachable
// within opts.MaxDepth following opts.Direction and opts.EdgeTypes, and every
// allowed-type edge whose endpoints are both in the subgraph.
// Returns an empty graph if startID does not exist or is disallowed.
func (g *Graph) Subgraph(startID string, opts TraverseOptions) *Graph {
	sub := New(g.Source)
	sub.ExtractedAt = g.ExtractedAt
	startNode := g.Nodes[startID]
	if !opts.nodeAllowed(startNode) {
		return sub
	}
	sub.AddNode(cloneNode(startNode))
	for _, n := range g.Reachable(startID, opts) {
		sub.AddNode(cloneNode(n))
	}
	allowed := makeAllowedMap(opts.EdgeTypes)
	g.adjMu.RLock()
	for _, e := range g.Edges {
		if !opts.edgeAllowed(allowed, e.Type) {
			continue
		}
		if sub.Nodes[e.From] != nil && sub.Nodes[e.To] != nil {
			var attrs map[string]string
			if len(e.Attrs) > 0 {
				attrs = maps.Clone(e.Attrs)
			}
			sub.AddEdge(e.From, e.To, e.Type, attrs)
		}
	}
	g.adjMu.RUnlock()
	return sub
}

func cloneNode(n *Node) *Node {
	c := &Node{
		ID:   n.ID,
		Type: n.Type,
		Name: n.Name,
	}
	if len(n.Attrs) > 0 {
		c.Attrs = maps.Clone(n.Attrs)
	}
	return c
}
