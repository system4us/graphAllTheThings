package codebase

import (
	"fmt"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"graphallthethings/internal/graph"
)

// httpMethods whitelists the Express-style router/app method names that,
// combined with a path-shaped ("/"-prefixed) first argument, identify a
// route registration — this two-part filter means detection doesn't depend
// on the receiver being named "router" or "app", which isn't reliable.
var httpMethods = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "delete": "DELETE",
	"patch": "PATCH", "all": "ALL", "use": "USE", "options": "OPTIONS", "head": "HEAD",
}

// routeArg is one argument after the path in a route registration: either an
// identifier resolved against funcsByName in wireRoutes (raw != ""), same as
// a CALLS target, or a handler already materialized as a function node
// during parsing (resolvedID != ", an inline arrow/function expression).
type routeArg struct {
	raw        string
	resolvedID string
}

// routeInfo is one detected route registration, collected during parseFiles
// and wired to its handler/middleware functions afterward by wireRoutes —
// the same two-pass shape CALLS uses, so a handler defined later in the file
// (or in another file) still resolves.
type routeInfo struct {
	id        string
	file      string
	lineStart int
	args      []routeArg
}

// detectRoute inspects one route.call match for the Express shape
// `<obj>.<method>("/path", ...middleware, handler)`. Scoped to JS/TS/JSX
// (the only langConfigs whose queryStr captures route.call); Go/Python/Rust
// route frameworks are a different grammar shape each and out of scope here —
// for those (or any Express call this shape misses), tag the handler function
// node directly via the annotate_entity MCP tool's route_method/route_path/
// route_framework fields (internal/mcpserver/server.go), surfaced by
// Engine.Routes alongside these statically-detected ones.
func (c *Connector) detectRoute(g *graph.Graph, caps map[string]*sitter.Node, relPath, fileID string, data []byte, srcLines []string, gen bool, funcs *[]funcInfo) {
	callNode, ok := caps["route.call"]
	if !ok {
		return
	}
	methodNode, ok := caps["route.method"]
	if !ok {
		return
	}
	method, ok := httpMethods[methodNode.Content(data)]
	if !ok {
		return
	}
	pathNode, ok := caps["route.path"]
	if !ok {
		return
	}
	isTemplate := pathNode.Type() == "template_string"
	path := normalizeClientPath(pathNode.Content(data))
	if !strings.HasPrefix(path, "/") {
		return
	}
	argsNode := callNode.ChildByFieldName("arguments")
	if argsNode == nil {
		return
	}

	lineStart := int(callNode.StartPoint().Row) + 1
	routeID := fmt.Sprintf("route:%s:%s:%d", relPath, method, lineStart)

	// The query anchors route.path as the arguments node's first named
	// child (`.` in the pattern), so every remaining named child (index 1+)
	// is a middleware/handler candidate. Registration args are function-shaped:
	// identifiers, inline functions, controller members (ctrl.create), or
	// middleware factory calls (requirePermission('x')). Data-shaped args
	// (objects, literals) mean a client-side HTTP call instead.
	var args []routeArg
	functionShaped := false
	extraCount := 0
	for i := 1; i < int(argsNode.NamedChildCount()); i++ {
		n := argsNode.NamedChild(i)
		extraCount++
		switch n.Type() {
		case "identifier":
			functionShaped = true
			args = append(args, routeArg{raw: n.Content(data)})
		case "member_expression":
			// ctrl.createProductFull: resolve by the property name, same
			// unique-name rule as any selector call.
			functionShaped = true
			if prop := n.ChildByFieldName("property"); prop != nil {
				args = append(args, routeArg{raw: prop.Content(data)})
			}
		case "call_expression":
			// middleware factory (requirePermission('x')): function-shaped
			// evidence for classification, but nothing to resolve to.
			functionShaped = true
		case "arrow_function", "function_expression":
			functionShaped = true
			args = append(args, routeArg{resolvedID: c.emitInlineHandler(g, n, relPath, fileID, data, srcLines, gen, funcs)})
		}
	}

	// Client-side HTTP call, not a route registration: a template path
	// (`/x/${id}` — Express paths are never templates), or a plain
	// get/post/put/delete/patch whose extra args are all data-shaped
	// (axios.get('/x'), api.post('/x', {payload})). `use`/`all` stay server-side.
	if method != "USE" && method != "ALL" && (isTemplate || (!functionShaped && extraCount >= 0)) {
		c.pendingClientCalls = append(c.pendingClientCalls, clientCall{
			method: method, path: path, file: relPath, line: lineStart,
		})
		return
	}
	if len(args) == 0 {
		return
	}

	g.AddNode(&graph.Node{
		ID:   routeID,
		Type: graph.NodeRoute,
		Name: method + " " + path,
		Attrs: map[string]string{
			"method":     method,
			"path":       path,
			"file":       relPath,
			"line_start": fmt.Sprint(lineStart),
		},
	})
	g.AddEdge(routeID, fileID, graph.EdgeBelongsTo, nil)
	c.pendingRoutes = append(c.pendingRoutes, routeInfo{id: routeID, file: relPath, lineStart: lineStart, args: args})
}

// emitInlineHandler materializes an anonymous arrow/function-expression
// route handler as a regular function node — same attrs shape as a named
// func.name capture (signature, doc-free, short body inlined) — and appends
// it to funcs so calls made from inside its body attribute to it correctly,
// the same as any other function.
func (c *Connector) emitInlineHandler(g *graph.Graph, node *sitter.Node, relPath, fileID string, data []byte, srcLines []string, gen bool, funcs *[]funcInfo) string {
	lineStart := int(node.StartPoint().Row) + 1
	lineEnd := int(node.EndPoint().Row) + 1
	name := "<anonymous handler>"
	id := fmt.Sprintf("func:%s:%s:%d", relPath, name, lineStart)

	sig := name
	if params := node.ChildByFieldName("parameters"); params != nil {
		sig = params.Content(data)
	}
	attrs := map[string]string{
		"file":       relPath,
		"line_start": fmt.Sprint(lineStart),
		"line_end":   fmt.Sprint(lineEnd),
		"signature":  sig,
	}
	if gen {
		attrs["generated"] = "true"
	}
	if n := lineEnd - lineStart + 1; n > 0 && n <= 15 && lineEnd <= len(srcLines) {
		if body := strings.Join(srcLines[lineStart-1:lineEnd], "\n"); len(body) <= 600 {
			attrs["body"] = body
		}
	}
	g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: name, Attrs: attrs})
	g.AddEdge(id, fileID, graph.EdgeBelongsTo, nil)
	*funcs = append(*funcs, funcInfo{id: id, name: name, file: relPath, lineStart: lineStart, lineEnd: lineEnd, signature: sig})
	return id
}

// clientCall is one client-side HTTP call site (axios.get('/x'),
// api.post(`/x/${id}`), fetch('/x')), wired to the matching route node by
// wireClientCalls — the frontend→backend half of the full-stack chain.
type clientCall struct {
	method string // "" = unknown (fetch without options)
	path   string
	file   string
	line   int
}

// detectClientFetch handles bare fetch('/path'|`/path/${id}`) calls.
func (c *Connector) detectClientFetch(caps map[string]*sitter.Node, relPath string, data []byte) {
	if caps["fetch.fn"].Content(data) != "fetch" {
		return
	}
	path := normalizeClientPath(caps["fetch.path"].Content(data))
	if !strings.HasPrefix(path, "/") {
		return
	}
	c.pendingClientCalls = append(c.pendingClientCalls, clientCall{
		method: "", path: path, file: relPath,
		line: int(caps["fetch.call"].StartPoint().Row) + 1,
	})
}

// normalizeClientPath strips quotes/backticks and turns `${expr}` template
// holes into :param so client paths compare against Express route paths.
func normalizeClientPath(raw string) string {
	s := strings.Trim(raw, "\"'`")
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], "}")
		if j < 0 {
			break
		}
		s = s[:i] + ":param" + s[i+j+1:]
	}
	return s
}

// pathSegmentsMatch compares a client path against a route path from the
// tail: the client sees the mounted prefix (`/api/products/:id`) while the
// route declares only its suffix (`/:id` under a products router), so the
// shorter path must match the longer one's tail segment-by-segment, with
// :param wildcards on either side. Root-only paths ("/") never match.
func pathSegmentsMatch(client, route string) int {
	cs := splitPathSegments(client)
	rs := splitPathSegments(route)
	n := len(cs)
	if len(rs) < n {
		n = len(rs)
	}
	if n == 0 {
		return 0
	}
	for i := 1; i <= n; i++ {
		a, b := cs[len(cs)-i], rs[len(rs)-i]
		if a != b && !strings.HasPrefix(a, ":") && !strings.HasPrefix(b, ":") {
			return 0
		}
	}
	return n
}

func splitPathSegments(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// wireClientCalls links each client call site to the best-matching route:
// same HTTP method (or unknown), longest tail-segment path match, ties and
// zero-segment matches skipped — no edge beats a wrong one. The edge source
// is the innermost function containing the call, or the file node when the
// call sits at module top level.
func (c *Connector) wireClientCalls(g *graph.Graph, funcs []funcInfo) {
	if len(c.pendingClientCalls) == 0 {
		return
	}
	routes := []*graph.Node{}
	for _, n := range g.Nodes {
		if n.Type == graph.NodeRoute {
			routes = append(routes, n)
		}
	}
	seen := map[string]bool{}
	for _, e := range g.Edges {
		if e.Type == graph.EdgeCallsEndpoint {
			seen[e.From+"\x00"+e.To] = true
		}
	}
	for _, cc := range c.pendingClientCalls {
		bestScore, bestID, ties := 0, "", 0
		for _, r := range routes {
			if cc.method != "" && r.Attrs["method"] != cc.method {
				continue
			}
			if r.Attrs["file"] == cc.file {
				continue // a router calling itself is registration noise
			}
			score := pathSegmentsMatch(cc.path, r.Attrs["path"])
			switch {
			case score > bestScore:
				bestScore, bestID, ties = score, r.ID, 1
			case score == bestScore && score > 0:
				ties++
			}
		}
		if bestScore == 0 || ties > 1 {
			continue
		}
		src := ""
		bestStart := -1
		for i := range funcs {
			f := &funcs[i]
			if f.file == cc.file && f.lineStart <= cc.line && f.lineEnd >= cc.line && f.lineStart > bestStart {
				bestStart, src = f.lineStart, f.id
			}
		}
		if src == "" {
			src = "file:" + filepath.Join(c.dir, cc.file)
		}
		if g.Nodes[src] == nil || seen[src+"\x00"+bestID] {
			continue
		}
		seen[src+"\x00"+bestID] = true
		g.AddEdge(src, bestID, graph.EdgeCallsEndpoint, map[string]string{
			"path": cc.path, "line": fmt.Sprint(cc.line),
		})
	}
	c.pendingClientCalls = nil
}

// wireRouteModels materializes route → model edges: from each route's
// handler, walk resolved CALLS up to 3 levels, then map the files visited
// (handler's own file plus everything those files import) onto model nodes.
// Graph-level, so it is language-agnostic: any detector that produced the
// model and any language whose imports resolve participate.
func (c *Connector) wireRouteModels(g *graph.Graph) {
	modelsByFile := map[string][]string{} // model's file relPath → model ids
	for id, n := range g.Nodes {
		if n.Type == graph.NodeModel {
			modelsByFile[n.Attrs["file"]] = append(modelsByFile[n.Attrs["file"]], id)
		}
	}
	if len(modelsByFile) == 0 {
		return
	}
	// file relPath → imported files' relPaths (resolved IMPORTS only)
	fileImports := map[string][]string{}
	for _, e := range g.Edges {
		if e.Type != graph.EdgeImports {
			continue
		}
		fn, tn := g.Nodes[e.From], g.Nodes[e.To]
		if fn != nil && tn != nil && fn.Type == graph.NodeFile && tn.Type == graph.NodeFile {
			fileImports[fn.Name] = append(fileImports[fn.Name], tn.Name)
		}
	}
	seen := map[string]bool{}
	for _, e := range g.Edges {
		if e.Type == graph.EdgeUsesModel {
			seen[e.From+"\x00"+e.To] = true
		}
	}

	for id, n := range g.Nodes {
		if n.Type != graph.NodeRoute {
			continue
		}
		var handler string
		for _, e := range g.EdgesOf(id) {
			if e.Type == graph.EdgeHandledBy && e.From == id {
				handler = e.To
			}
		}
		if handler == "" {
			continue
		}
		// Transitive callee files from the handler, depth 3, capped.
		files := map[string]bool{}
		visited := map[string]bool{handler: true}
		level := []string{handler}
		for d := 0; d < 3 && len(level) > 0 && len(visited) < 200; d++ {
			var next []string
			for _, cur := range level {
				if fn := g.Nodes[cur]; fn != nil && fn.Attrs["file"] != "" {
					files[fn.Attrs["file"]] = true
				}
				for _, e := range g.EdgesOf(cur) {
					if e.Type == graph.EdgeCalls && e.From == cur && !visited[e.To] {
						visited[e.To] = true
						next = append(next, e.To)
					}
				}
			}
			level = next
		}
		modelHit := map[string]bool{}
		for f := range files {
			for _, mid := range modelsByFile[f] {
				modelHit[mid] = true
			}
			for _, imp := range fileImports[f] {
				for _, mid := range modelsByFile[imp] {
					modelHit[mid] = true
				}
			}
		}
		for mid := range modelHit {
			if !seen[id+"\x00"+mid] {
				seen[id+"\x00"+mid] = true
				g.AddEdge(id, mid, graph.EdgeUsesModel, nil)
			}
		}
	}
}

// wireRoutes resolves each detected route's identifier arguments against
// funcsByName (the exact resolveCall rules CALLS uses — same-file tiebreak,
// ambiguous cross-file names skipped), wiring the last function-like
// argument as HANDLED_BY and every earlier one as USES_MIDDLEWARE in order.
// An unresolved identifier gets an external stub, the same fallback
// resolveAndWire uses for an unresolved call.
func (c *Connector) wireRoutes(g *graph.Graph, funcsByName map[string][]string) {
	if len(c.pendingRoutes) == 0 {
		return
	}
	for _, r := range c.pendingRoutes {
		var resolved []string
		for _, a := range r.args {
			if a.resolvedID != "" {
				resolved = append(resolved, a.resolvedID)
				continue
			}
			if target := resolveCall(g, r.file, a.raw, funcsByName); target != "" {
				resolved = append(resolved, target)
				continue
			}
			if len(funcsByName[a.raw]) > 1 {
				continue // ambiguous cross-file name: same rule as resolveAndWire — no edge beats a wrong one
			}
			stubID := "call:" + a.raw
			if g.Nodes[stubID] == nil {
				g.AddNode(&graph.Node{ID: stubID, Type: graph.NodeFunction, Name: a.raw, Attrs: map[string]string{"external": "true"}})
			}
			resolved = append(resolved, stubID)
		}
		for i, target := range resolved {
			if g.Nodes[target] == nil {
				continue
			}
			if i == len(resolved)-1 {
				g.AddEdge(r.id, target, graph.EdgeHandledBy, nil)
			} else {
				g.AddEdge(r.id, target, graph.EdgeUsesMiddleware, map[string]string{"order": fmt.Sprint(i)})
			}
		}
	}
	c.pendingRoutes = nil
}
