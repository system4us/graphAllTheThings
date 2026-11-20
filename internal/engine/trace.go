package engine

import (
	"fmt"
	"path/filepath"
	"strings"

	"graphallthethings/internal/graph"
)

// flowEdgeTypes are the semantic execution/data-flow edges traversed by
// TracePath on a codebase graph.
var flowEdgeTypes = map[string]bool{
	graph.EdgeCallsEndpoint:  true,
	graph.EdgeHandledBy:      true,
	graph.EdgeUsesMiddleware: true,
	graph.EdgeCalls:          true,
	graph.EdgeUsesModel:      true,
	graph.EdgeDependsOn:      true,
	graph.EdgeGenerates:      true,
	graph.EdgeReferences:     true,
	graph.EdgeImports:        true,
	graph.EdgeWritesState:    true,
	graph.EdgeReadsState:     true,
}

type traceStep struct {
	edge    graph.Edge
	reverse bool
	nextID  string
}

// resolveTraceNode resolves a user-supplied node reference (exact ID, route
// name like "GET /orders/{id}", symbol name, file path, or model name) to a
// canonical node ID.
func (e *Engine) resolveTraceNode(ref string) (*graph.Node, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("empty node reference")
	}
	if n := e.G.Nodes[ref]; n != nil {
		return n, nil
	}
	// Exact match on route name (e.g. "GET /orders/:param" or "GET /orders/{id}")
	normRef := normalizeRouteParam(strings.ToLower(ref))
	for _, n := range e.G.Nodes {
		if n.Type == graph.NodeRoute && normalizeRouteParam(strings.ToLower(n.Name)) == normRef {
			return n, nil
		}
	}
	// Exact match on file path or model name
	slashRef := filepath.ToSlash(strings.TrimPrefix(ref, "./"))
	var symMatches []*graph.Node
	for _, n := range e.G.Nodes {
		if n.Attrs["external"] == "true" {
			continue
		}
		if n.Type == graph.NodeFile && filepath.ToSlash(n.Name) == slashRef {
			return n, nil
		}
		if n.Type == graph.NodeModel && (strings.EqualFold(n.Name, ref) || strings.EqualFold(n.Attrs["table"], ref)) {
			return n, nil
		}
		if (n.Type == graph.NodeFunction || n.Type == graph.NodeDefinition) && n.Name == ref {
			symMatches = append(symMatches, n)
		}
	}
	if len(symMatches) == 1 {
		return symMatches[0], nil
	}
	if len(symMatches) > 1 {
		var ids []string
		for _, m := range symMatches {
			ids = append(ids, m.ID)
		}
		return nil, fmt.Errorf("ambiguous symbol %q — pick one id: %s", ref, strings.Join(ids, ", "))
	}
	// Fallback to Engine.Describe (handles short IDs, unique function/def/table names)
	if d, err := e.Describe(ref); err == nil {
		if n := e.G.Nodes[d.ID]; n != nil {
			return n, nil
		}
	}
	return nil, fmt.Errorf("node not found: %q (try exact node id, function name, route, model, or file path)", ref)
}

func normalizeRouteParam(s string) string {
	// Normalize {id} and :id to :param for matching route names.
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '{' {
			if end := strings.IndexByte(s[i:], '}'); end > 0 {
				b.WriteString(":param")
				i += end + 1
				continue
			}
		}
		if s[i] == ':' {
			j := i + 1
			for j < len(s) && s[j] != '/' && s[j] != ' ' {
				j++
			}
			b.WriteString(":param")
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// TracePath finds the shortest execution/data-flow path between two nodes in a
// codebase graph (e.g. route -> handler -> sqlc query -> model, or emitter ->
// event listener -> handler). Tries directed edges first, falling back to
// undirected traversal if no directed path exists.
func (e *Engine) TracePath(fromRef, toRef string, maxDepth int) (string, error) {
	if !e.IsCodebase() {
		return "", fmt.Errorf("trace_path requires a codebase graph (use join_path for DB/API graphs)")
	}
	if maxDepth <= 0 {
		maxDepth = 8
	}
	if maxDepth > 15 {
		maxDepth = 15
	}
	src, err := e.resolveTraceNode(fromRef)
	if err != nil {
		return "", fmt.Errorf("from: %w", err)
	}
	dst, err := e.resolveTraceNode(toRef)
	if err != nil {
		return "", fmt.Errorf("to: %w", err)
	}
	if src.ID == dst.ID {
		return fmt.Sprintf("same node: %s\n", formatTraceNode(src)), nil
	}

	steps := e.bfsTrace(src.ID, dst.ID, maxDepth, true)
	directed := true
	if len(steps) == 0 {
		steps = e.bfsTrace(src.ID, dst.ID, maxDepth, false)
		directed = false
	}
	if len(steps) == 0 {
		return fmt.Sprintf("no path found within %d hops between %s and %s\n",
			maxDepth, formatTraceNode(src), formatTraceNode(dst)), nil
	}

	var b strings.Builder
	mode := "directed"
	if !directed {
		mode = "undirected fallback"
	}
	fmt.Fprintf(&b, "trace_path (%d hop(s), %s):\n", len(steps), mode)
	fmt.Fprintf(&b, "  %s\n", formatTraceNode(src))
	for _, st := range steps {
		arrow := "-->"
		if st.reverse {
			arrow = "<--"
		}
		fmt.Fprintf(&b, "    --[%s]%s %s\n", describeTraceEdge(st.edge), arrow, formatTraceNode(e.G.Nodes[st.nextID]))
	}
	return b.String(), nil
}

func (e *Engine) bfsTrace(srcID, dstID string, maxDepth int, directedOnly bool) []traceStep {
	type queueItem struct {
		id    string
		depth int
	}
	visited := map[string]bool{srcID: true}
	parent := map[string]string{}
	parentStep := map[string]traceStep{}
	queue := []queueItem{{id: srcID, depth: 0}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= maxDepth {
			continue
		}
		for _, ed := range e.G.EdgesOf(cur.id) {
			if !flowEdgeTypes[ed.Type] {
				continue
			}
			var nextID string
			var rev bool
			if ed.From == cur.id {
				nextID = ed.To
				rev = false
			} else if !directedOnly && ed.To == cur.id {
				nextID = ed.From
				rev = true
			} else {
				continue
			}
			if visited[nextID] || e.G.Nodes[nextID] == nil {
				continue
			}
			visited[nextID] = true
			parent[nextID] = cur.id
			parentStep[nextID] = traceStep{edge: ed, reverse: rev, nextID: nextID}
			if nextID == dstID {
				// Reconstruct path
				var revPath []traceStep
				for node := dstID; node != srcID; node = parent[node] {
					revPath = append(revPath, parentStep[node])
				}
				for i, j := 0, len(revPath)-1; i < j; i, j = i+1, j-1 {
					revPath[i], revPath[j] = revPath[j], revPath[i]
				}
				return revPath
			}
			queue = append(queue, queueItem{id: nextID, depth: cur.depth + 1})
		}
	}
	return nil
}

func formatTraceNode(n *graph.Node) string {
	if n == nil {
		return "(unknown)"
	}
	loc := ""
	if f := n.Attrs["file"]; f != "" {
		if ls := n.Attrs["line_start"]; ls != "" {
			loc = fmt.Sprintf(" (%s:%s)", f, ls)
		} else {
			loc = fmt.Sprintf(" (%s)", f)
		}
	}
	return fmt.Sprintf("[%s] %s%s", n.Type, n.Name, loc)
}

func describeTraceEdge(ed graph.Edge) string {
	switch ed.Type {
	case graph.EdgeCalls:
		if ed.Attrs["via"] == "dispatch" {
			if ev := ed.Attrs["event"]; ev != "" {
				return fmt.Sprintf("emits %q (inferred)", ev)
			}
			return "dispatch (inferred)"
		}
		if ed.Attrs["inferred"] == "true" {
			return "calls (inferred)"
		}
		return "calls"
	case graph.EdgeCallsEndpoint:
		return "calls endpoint"
	case graph.EdgeHandledBy:
		return "handled by"
	case graph.EdgeUsesMiddleware:
		return "middleware"
	case graph.EdgeUsesModel:
		if q := ed.Attrs["query"]; q != "" {
			return fmt.Sprintf("uses model (sqlc:%s)", q)
		}
		return "uses model"
	case graph.EdgeDependsOn:
		if ed.Attrs["via"] == "sqlc" {
			if q := ed.Attrs["query"]; q != "" {
				return fmt.Sprintf("sqlc query %s", q)
			}
			return "sqlc"
		}
		return "depends on"
	case graph.EdgeGenerates:
		if ed.Attrs["via"] == "sqlc" {
			return "generates (sqlc)"
		}
		return "generates"
	case graph.EdgeReferences:
		return "references"
	case graph.EdgeImports:
		return "imports"
	case graph.EdgeWritesState:
		return "writes state"
	case graph.EdgeReadsState:
		return "reads state"
	default:
		return strings.ToLower(ed.Type)
	}
}
