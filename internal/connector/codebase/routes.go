package codebase

import (
	"fmt"
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
	path := strings.Trim(pathNode.Content(data), "\"'`")
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
	// is a middleware/handler candidate.
	var args []routeArg
	for i := 1; i < int(argsNode.NamedChildCount()); i++ {
		n := argsNode.NamedChild(i)
		switch n.Type() {
		case "identifier":
			args = append(args, routeArg{raw: n.Content(data)})
		case "arrow_function", "function_expression":
			args = append(args, routeArg{resolvedID: c.emitInlineHandler(g, n, relPath, fileID, data, srcLines, gen, funcs)})
		}
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
