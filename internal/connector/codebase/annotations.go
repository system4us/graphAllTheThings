package codebase

// Annotation/attribute-declared HTTP surface — one mechanism, two uses:
//
//   - client side: Retrofit-style verb annotations on interface methods
//     (@GET("users/{id}") in Java or Kotlin) queue clientCall candidates,
//     attributed to the annotated method's function node;
//   - server side: Spring (@GetMapping/@RequestMapping) and ASP.NET Core
//     ([HttpGet]/[Route]) method annotations become route nodes wired
//     HANDLED_BY to the annotated method, with class-level prefixes
//     (@RequestMapping("/api/products"), [Route("api/[controller]")])
//     joined in by line containment at end of file.
//
// Same conservative wiring as everything else: a Retrofit candidate only
// makes an edge when a detected route matches; a Spring/ASP route enters the
// same full-stack chain (USES_MODEL, CALLS_ENDPOINT) as an Express one.

import (
	"fmt"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"graphallthethings/internal/graph"
)

// retrofitAnns: verb annotations that mark a *client* call declaration.
var retrofitAnns = map[string]string{
	"GET": "GET", "POST": "POST", "PUT": "PUT", "DELETE": "DELETE",
	"PATCH": "PATCH", "HEAD": "HEAD", "OPTIONS": "OPTIONS",
}

// springMappings / aspAttrs: method annotations that declare a *route*.
var springMappings = map[string]string{
	"GetMapping": "GET", "PostMapping": "POST", "PutMapping": "PUT",
	"DeleteMapping": "DELETE", "PatchMapping": "PATCH",
	"RequestMapping": "", // verb parsed from `method = RequestMethod.X`
}
var aspAttrs = map[string]string{
	"HttpGet": "GET", "HttpPost": "POST", "HttpPut": "PUT",
	"HttpDelete": "DELETE", "HttpPatch": "PATCH", "HttpHead": "HEAD",
	"HttpOptions": "OPTIONS",
}

var (
	firstQuotedRe   = regexp.MustCompile(`"([^"]*)"`)
	requestMethodRe = regexp.MustCompile(`RequestMethod\.([A-Z]+)`)
)

// annPrefix is a class-level path prefix (@RequestMapping / [Route]).
type annPrefix struct {
	prefix    string
	className string
	startLine int
	endLine   int
}

// annServerRoute is one method-level route annotation, combined with its
// enclosing class prefix by emitAnnotationRoutes.
type annServerRoute struct {
	verb       string
	path       string // may be "" ([HttpPost] with the path on the class)
	methodName string
	lineStart  int
}

// annState accumulates one file's annotation captures during the match loop.
type annState struct {
	prefixes []annPrefix
	routes   []annServerRoute
}

// handleAnnotation processes one method- or class-level annotation capture.
// nameNode is the annotation's name identifier; argsText is the raw argument
// list content ("" for marker annotations); methodNameNode/classNameNode is
// whichever the pattern bound.
func (c *Connector) handleAnnotation(st *annState, annName, argsText string, methodNameNode, classNameNode *sitter.Node, relPath string, data []byte) {
	quoted := ""
	if m := firstQuotedRe.FindStringSubmatch(argsText); m != nil {
		quoted = m[1]
	}

	// Class-level: @RequestMapping / [Route] prefix for the methods inside.
	if classNameNode != nil {
		if (annName == "RequestMapping" || annName == "Route") && quoted != "" {
			cls := classNameNode.Parent()
			for cls != nil && cls.Type() != "class_declaration" && cls.Type() != "interface_declaration" {
				cls = cls.Parent()
			}
			if cls != nil {
				st.prefixes = append(st.prefixes, annPrefix{
					prefix:    quoted,
					className: classNameNode.Content(data),
					startLine: int(cls.StartPoint().Row) + 1,
					endLine:   int(cls.EndPoint().Row) + 1,
				})
			}
		}
		return
	}
	if methodNameNode == nil {
		return
	}
	lineStart, _ := declarationRange(methodNameNode)

	// Retrofit client declaration.
	if verb, ok := retrofitAnns[annName]; ok && quoted != "" {
		path := normalizeClientPath(`"` + quoted + `"`)
		if rel, ok := acceptClientPath(path); ok {
			c.pendingClientCalls = append(c.pendingClientCalls, clientCall{
				method: verb, path: path, file: relPath,
				line: lineStart, relative: rel,
			})
		}
		return
	}

	// Spring / ASP.NET server route declaration.
	verb, isSpring := springMappings[annName]
	if !isSpring {
		var isASP bool
		verb, isASP = aspAttrs[annName]
		if !isASP {
			return
		}
	}
	if verb == "" { // @RequestMapping: verb lives in `method = RequestMethod.X`
		m := requestMethodRe.FindStringSubmatch(argsText)
		if m == nil {
			return // no verb, no route — can't match clients against it
		}
		verb = m[1]
	}
	st.routes = append(st.routes, annServerRoute{
		verb: verb, path: quoted,
		methodName: methodNameNode.Content(data), lineStart: lineStart,
	})
}

// emitAnnotationRoutes joins method routes with their innermost class prefix
// and materializes route nodes + pendingRoutes entries (HANDLED_BY resolves
// in wireRoutes, models/client calls in the shared wiring passes).
func (c *Connector) emitAnnotationRoutes(g *graph.Graph, st *annState, relPath, fileID string) {
	for _, r := range st.routes {
		prefix, className := "", ""
		bestSpan := int(^uint(0) >> 1)
		for _, p := range st.prefixes {
			if p.startLine <= r.lineStart && r.lineStart <= p.endLine && p.endLine-p.startLine < bestSpan {
				prefix, className, bestSpan = p.prefix, p.className, p.endLine-p.startLine
			}
		}
		path := joinRoutePath(prefix, r.path)
		if strings.Contains(path, "[controller]") {
			ctrl := strings.ToLower(strings.TrimSuffix(className, "Controller"))
			path = strings.ReplaceAll(path, "[controller]", ctrl)
		}
		path = normalizeClientPath(`"` + path + `"`) // {id} → :param
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		if path == "/" && prefix == "" {
			continue
		}
		routeID := fmt.Sprintf("route:%s:%s:%d", relPath, r.verb, r.lineStart)
		g.AddNode(&graph.Node{
			ID:   routeID,
			Type: graph.NodeRoute,
			Name: r.verb + " " + path,
			Attrs: map[string]string{
				"method":     r.verb,
				"path":       path,
				"file":       relPath,
				"line_start": fmt.Sprint(r.lineStart),
			},
		})
		g.AddEdge(routeID, fileID, graph.EdgeBelongsTo, nil)
		c.pendingRoutes = append(c.pendingRoutes, routeInfo{
			id: routeID, file: relPath, lineStart: r.lineStart,
			args: []routeArg{{resolvedID: fmt.Sprintf("func:%s:%s:%d", relPath, r.methodName, r.lineStart)}},
		})
	}
}

func joinRoutePath(prefix, path string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	if path == "" {
		return prefix
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return prefix + path
}
