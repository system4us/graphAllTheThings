package codebase

// Native route registrations for Go and Python, hung off the same
// call.func/call.sel captures as CALLS and the generic client probe:
//
//   - Go: `r.GET("/x", handler)` (gin/echo), `r.Get("/x", h)` (chi),
//     `mux.HandleFunc("GET /x", h)` (net/http 1.22 method-in-pattern),
//     `r.HandleFunc("/x", h).Methods("POST")` (gorilla). Registrations are
//     bare statements; client calls are values (`resp, err := http.Get(…)`),
//     the same disambiguation detectRoute uses for axios-vs-Express.
//   - Python: verb/route decorator calls (@app.route("/x", methods=["POST"]),
//     @app.get("/x") — Flask 2 / FastAPI / APIRouter), handler = the
//     decorated function.
//
// A detected registration suppresses the generic client probe for that call
// site, which also removes the gin/chi false client candidates that existed
// before this detector.

import (
	"fmt"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"graphallthethings/internal/graph"
)

var (
	gorillaMethodsRe = regexp.MustCompile(`\.Methods\(\s*"([A-Za-z]+)"`)
	flaskParamRe     = regexp.MustCompile(`<[^/>]+>`) // /users/<int:uid> → /users/:param
)

// detectNativeRoute inspects one call capture for a Go/Python route
// registration. Returns true when the call site is consumed (a route was
// emitted, or it's decorator context that must never become a client call).
func (c *Connector) detectNativeRoute(g *graph.Graph, nameNode *sitter.Node, callName, relPath, fileID, ext string, data []byte) bool {
	switch ext {
	case ".go":
		return c.detectGoRoute(g, nameNode, callName, relPath, fileID, data)
	case ".py":
		return c.detectPyRoute(g, nameNode, callName, relPath, fileID, data)
	}
	return false
}

func (c *Connector) detectGoRoute(g *graph.Graph, nameNode *sitter.Node, callName, relPath, fileID string, data []byte) bool {
	if !strings.HasPrefix(callName, ".") {
		return false
	}
	name := callName[1:]
	verb, isVerb := clientHTTPVerbs[strings.ToLower(name)]
	isHandle := name == "HandleFunc" || name == "Handle"
	if !isVerb && !isHandle {
		return false
	}
	callNode := enclosingCallNode(nameNode)
	if callNode == nil {
		return false
	}
	// Statement context: climb the selector/call chain (gorilla appends
	// .Methods(...) around the registration); anything used as a value —
	// `resp, err := http.Get(...)` — is a client call, not a registration.
	top := callNode
	for p := top.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "selector_expression", "call_expression":
			top = p
			continue
		case "expression_statement":
		default:
			return false
		}
		break
	}
	argsNode := callArguments(callNode)
	if argsNode == nil {
		return false
	}
	argNodes := unwrapArgs(argsNode)
	// path + at least a handler: a bare `http.Get("/health")` statement
	// stays a client call.
	if len(argNodes) < 2 || !isStringNode(argNodes[0]) {
		return false
	}
	rawPath := strings.Trim(argNodes[0].Content(data), "\"`")
	method := verb
	if isHandle {
		method = ""
		if sp := strings.SplitN(rawPath, " ", 2); len(sp) == 2 { // "POST /orders"
			if v, ok := clientHTTPVerbs[strings.ToLower(sp[0])]; ok {
				method, rawPath = v, sp[1]
			}
		}
		if method == "" {
			if m := gorillaMethodsRe.FindStringSubmatch(top.Content(data)); m != nil {
				if v, ok := clientHTTPVerbs[strings.ToLower(m[1])]; ok {
					method = v
				}
			}
		}
		// still "": net/http handler that serves any method — wildcard route.
	}
	path := normalizeClientPath(`"` + rawPath + `"`) // chi {id} → :param; gin/echo :id stays
	if !strings.HasPrefix(path, "/") {
		return false
	}
	var rargs []routeArg
	for _, n := range argNodes[1:] {
		switch n.Type() {
		case "identifier":
			rargs = append(rargs, routeArg{raw: n.Content(data)})
		case "selector_expression": // ctrl.Create — resolve by field name
			if f := n.ChildByFieldName("field"); f != nil {
				rargs = append(rargs, routeArg{raw: f.Content(data)})
			}
		}
	}
	c.emitNativeRoute(g, method, path, relPath, fileID, int(callNode.StartPoint().Row)+1, rargs)
	return true
}

func (c *Connector) detectPyRoute(g *graph.Graph, nameNode *sitter.Node, callName, relPath, fileID string, data []byte) bool {
	callNode := enclosingCallNode(nameNode)
	if callNode == nil {
		return false
	}
	dec := callNode.Parent()
	if dec == nil || dec.Type() != "decorator" {
		return false
	}
	// Decorator calls are never client calls: consume the site even when no
	// route comes out of it (@lru_cache, @retry(url=...), ...).
	name := strings.TrimPrefix(callName, ".")
	verb, isVerb := clientHTTPVerbs[strings.ToLower(name)]
	if !isVerb && name != "route" {
		return true
	}
	dd := dec.Parent()
	if dd == nil || dd.Type() != "decorated_definition" {
		return true
	}
	handler := ""
	for i := 0; i < int(dd.NamedChildCount()); i++ {
		if fn := dd.NamedChild(i); fn.Type() == "function_definition" {
			if n := fn.ChildByFieldName("name"); n != nil {
				handler = n.Content(data)
			}
		}
	}
	argsNode := callArguments(callNode)
	if argsNode == nil {
		return true
	}
	argNodes := unwrapArgs(argsNode)
	if len(argNodes) == 0 || !isStringNode(argNodes[0]) {
		return true
	}
	rawPath := flaskParamRe.ReplaceAllString(strings.Trim(argNodes[0].Content(data), `"'`), ":param")
	path := normalizeClientPath(`"` + rawPath + `"`)
	if !strings.HasPrefix(path, "/") {
		return true
	}
	var verbs []string
	if isVerb {
		verbs = []string{verb}
	} else {
		// @app.route(..., methods=["POST", "PUT"]) — the list arrives as an
		// unwrapped keyword_argument value.
		for _, n := range argNodes {
			if n.Type() != "list" {
				continue
			}
			for i := 0; i < int(n.NamedChildCount()); i++ {
				el := n.NamedChild(i)
				if !isStringNode(el) {
					continue
				}
				if v, ok := clientHTTPVerbs[strings.ToLower(strings.Trim(el.Content(data), `"'`))]; ok {
					verbs = append(verbs, v)
				}
			}
		}
		if len(verbs) == 0 {
			verbs = []string{"GET"} // Flask's default
		}
	}
	line := int(dec.StartPoint().Row) + 1
	var rargs []routeArg
	if handler != "" {
		rargs = []routeArg{{raw: handler}}
	}
	for _, v := range verbs {
		c.emitNativeRoute(g, v, path, relPath, fileID, line, rargs)
	}
	return true
}

// emitNativeRoute materializes a route node ("" method = any-method
// wildcard) and queues handler resolution through the shared wireRoutes.
func (c *Connector) emitNativeRoute(g *graph.Graph, method, path, relPath, fileID string, line int, rargs []routeArg) {
	label := method
	if label == "" {
		label = "ANY"
	}
	routeID := fmt.Sprintf("route:%s:%s:%d", relPath, label, line)
	g.AddNode(&graph.Node{
		ID:   routeID,
		Type: graph.NodeRoute,
		Name: label + " " + path,
		Attrs: map[string]string{
			"method":     method,
			"path":       path,
			"file":       relPath,
			"line_start": fmt.Sprint(line),
		},
	})
	g.AddEdge(routeID, fileID, graph.EdgeBelongsTo, nil)
	c.pendingRoutes = append(c.pendingRoutes, routeInfo{id: routeID, file: relPath, lineStart: line, args: rargs})
}
