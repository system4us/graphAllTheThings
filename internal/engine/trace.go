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
// event listener -> handler).
//
// Order of answers, each labelled as such:
//  1. a directed path from -> to;
//  2. a directed path to -> from (the second argument reaches the first);
//  3. no flow either way: the nearest common caller both are reached from, or
//     the nearest thing both lead to (a shared callee, model, sqlc query) —
//     printed as two chains, never as one path stitched through reversed
//     edges. Test code is never used as a hop or a meeting point: a test
//     calling both says nothing about how production code connects them.
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

	var b strings.Builder
	if steps := e.bfsTrace(src.ID, dst.ID, maxDepth, true); len(steps) > 0 {
		fmt.Fprintf(&b, "trace_path (%d hop(s), directed):\n", len(steps))
		e.writeChain(&b, src, steps)
		return b.String(), nil
	}
	if steps := e.bfsTrace(dst.ID, src.ID, maxDepth, true); len(steps) > 0 {
		fmt.Fprintf(&b, "trace_path (%d hop(s), directed, reversed — the second node reaches the first):\n", len(steps))
		e.writeChain(&b, dst, steps)
		return b.String(), nil
	}
	if out := e.commonFlow(src, dst, maxDepth); out != "" {
		return out, nil
	}
	return fmt.Sprintf("no path found within %d hops between %s and %s (no flow either way, no common caller or shared dependency outside tests)\n",
		maxDepth, formatTraceNode(src), formatTraceNode(dst)), nil
}

func (e *Engine) writeChain(b *strings.Builder, start *graph.Node, steps []traceStep) {
	fmt.Fprintf(b, "  %s\n", formatTraceNode(start))
	for _, st := range steps {
		arrow := "-->"
		if st.reverse {
			arrow = "<--"
		}
		fmt.Fprintf(b, "    --[%s]%s %s\n", describeTraceEdge(st.edge), arrow, formatTraceNode(e.G.Nodes[st.nextID]))
	}
}

// isTestNode reports a node declared in a test file.
func isTestNode(n *graph.Node) bool {
	if n == nil {
		return false
	}
	f := n.Attrs["file"]
	if n.Type == graph.NodeFile {
		f = n.Name
	}
	return f != "" && IsTestFilePath(f)
}

// flowReach walks flow edges from start — forward (towards callees/models)
// or backward (towards callers) — and returns each reached node's distance
// plus the edge that reached it (edge.From/To in graph direction). Test,
// external and UI-component nodes are not entered.
func (e *Engine) flowReach(start string, forward bool, maxDepth int) (map[string]int, map[string]graph.Edge, map[string]string) {
	dist := map[string]int{start: 0}
	via := map[string]graph.Edge{}
	parent := map[string]string{}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if dist[cur] >= maxDepth {
			continue
		}
		for _, ed := range e.G.EdgesOf(cur) {
			if !flowEdgeTypes[ed.Type] {
				continue
			}
			next := ""
			switch {
			case forward && ed.From == cur:
				next = ed.To
			case !forward && ed.To == cur:
				next = ed.From
			default:
				continue
			}
			if _, ok := dist[next]; ok {
				continue
			}
			nn := e.G.Nodes[next]
			if nn == nil || nn.Attrs["external"] == "true" || nn.Type == graph.NodeComponent || isTestNode(nn) {
				continue
			}
			dist[next] = dist[cur] + 1
			via[next] = ed
			parent[next] = cur
			queue = append(queue, next)
		}
	}
	return dist, via, parent
}

// commonFlow explains how two nodes with no flow between them still meet:
// the nearest common caller, or the nearest shared dependency.
func (e *Engine) commonFlow(src, dst *graph.Node, maxDepth int) string {
	type meet struct {
		id    string
		total int
		up    bool // true: common caller; false: shared dependency
	}
	var best *meet
	for _, up := range []bool{true, false} {
		ds, _, _ := e.flowReach(src.ID, !up, maxDepth)
		dd, _, _ := e.flowReach(dst.ID, !up, maxDepth)
		for id, a := range ds {
			c, ok := dd[id]
			if !ok || id == src.ID || id == dst.ID {
				continue
			}
			if best == nil || a+c < best.total || (a+c == best.total && id < best.id) {
				best = &meet{id, a + c, up}
			}
		}
	}
	if best == nil {
		return ""
	}
	meetNode := e.G.Nodes[best.id]
	var b strings.Builder
	if best.up {
		fmt.Fprintf(&b, "trace_path: no flow between the two either way; both are reached from a common caller, %s:\n", formatTraceNode(meetNode))
	} else {
		fmt.Fprintf(&b, "trace_path: no flow between the two either way; both lead to %s:\n", formatTraceNode(meetNode))
	}
	for _, end := range []*graph.Node{src, dst} {
		_, via, parent := e.flowReach(end.ID, !best.up, maxDepth)
		// Walk from the meeting point back to this end.
		var chain []string
		for cur := best.id; cur != end.ID; cur = parent[cur] {
			chain = append(chain, cur)
		}
		chain = append(chain, end.ID)
		b.WriteString("\n")
		if best.up {
			// caller ... -> end, in call direction.
			fmt.Fprintf(&b, "  %s\n", formatTraceNode(meetNode))
			for i := 1; i < len(chain); i++ {
				ed := via[chain[i-1]]
				fmt.Fprintf(&b, "    --[%s]--> %s\n", describeTraceEdge(ed), formatTraceNode(e.G.Nodes[chain[i]]))
			}
		} else {
			// end -> ... -> shared dependency, in call direction.
			fmt.Fprintf(&b, "  %s\n", formatTraceNode(end))
			for i := len(chain) - 2; i >= 0; i-- {
				ed := via[chain[i]]
				fmt.Fprintf(&b, "    --[%s]--> %s\n", describeTraceEdge(ed), formatTraceNode(e.G.Nodes[chain[i]]))
			}
		}
	}
	return b.String()
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
			if visited[nextID] {
				continue
			}
			nn := e.G.Nodes[nextID]
			if nn == nil {
				continue
			}
			if nextID != dstID && (nn.Attrs["external"] == "true" || nn.Type == graph.NodeComponent || isTestNode(nn)) {
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
