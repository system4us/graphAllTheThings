package codebase

// Generic, language-agnostic client-side HTTP call detection. The JS/TS
// route.call / fetch.call queries in langFor catch the Express-adjacent
// shapes; everything else (Go net/http, Python requests, Rust reqwest, Java
// RestTemplate/WebClient, C# HttpClient, and any in-house wrapper) funnels
// through here, hung off the same call.func/call.sel captures that feed
// CALLS edges — no per-framework tree-sitter queries.
//
// Precision comes from two gates, not from detection accuracy:
//  1. a candidate needs explicit HTTP-verb evidence (verb-shaped callee,
//     verb string argument, method: key in an options object/dict, or a
//     wrapper declared in .gatt/clients.json) plus a rooted path literal;
//  2. wireClientCalls only emits an edge when method + path tail match a
//     detected route uniquely — an unmatched candidate costs nothing.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// clientHTTPVerbs are the request-side method names; unlike httpMethods it
// excludes use/all, which only exist as Express registration calls.
var clientHTTPVerbs = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "delete": "DELETE",
	"patch": "PATCH", "head": "HEAD", "options": "OPTIONS",
}

// clientWrapper is one .gatt/clients.json entry describing an in-house HTTP
// wrapper the verb heuristics can't see (callApi('/x'), request({url})):
// {"wrappers": [{"name": "apiRequest", "method_arg": 0, "path_arg": 1},
//
//	{"name": "getJSON", "method": "GET", "path_arg": 0}]}
type clientWrapper struct {
	Name      string `json:"name"`
	Method    string `json:"method"`     // fixed verb, or "" when method_arg is used
	MethodArg *int   `json:"method_arg"` // index of the verb-string argument
	PathArg   int    `json:"path_arg"`   // index of the path argument
}

// loadClientWrappers reads .gatt/clients.json once; a missing or malformed
// file leaves an empty (non-nil) map so the lookup is a cheap no-op.
func (c *Connector) loadClientWrappers() {
	c.clientWrappers = map[string]clientWrapper{}
	data, err := os.ReadFile(filepath.Join(c.dir, ".gatt", "clients.json"))
	if err != nil {
		return
	}
	var payload struct {
		Wrappers []clientWrapper `json:"wrappers"`
	}
	if json.Unmarshal(data, &payload) == nil {
		for _, w := range payload.Wrappers {
			if w.Name != "" {
				c.clientWrappers[w.Name] = w
			}
		}
	}
}

// detectGenericClientCall inspects one call.func/call.sel capture (any
// language) for a client-side HTTP call and, on verb + path evidence,
// queues a clientCall candidate for wireClientCalls.
func (c *Connector) detectGenericClientCall(nameNode *sitter.Node, callName, relPath, ext string, data []byte) {
	if c.clientWrappers == nil {
		c.loadClientWrappers()
	}
	isMember := strings.HasPrefix(callName, ".")
	name := strings.TrimPrefix(callName, ".")

	// JS/TS/JSX: bare fetch() and obj.<verb>/use/all member calls are owned
	// by the dedicated fetch.call/route.call queries (detectClientFetch,
	// detectRoute) — skip them here so one call site can't queue twice.
	jsFamily := ext == ".js" || ext == ".jsx" || ext == ".ts" || ext == ".tsx"
	if jsFamily {
		if !isMember && name == "fetch" {
			return
		}
		if isMember && httpMethods[strings.ToLower(name)] != "" {
			return
		}
	}

	wrapper, hasWrapper := c.clientWrappers[name]

	// Verb from the callee itself: exact (axios.get, http.Get, requests.post)
	// for any call, camel-prefixed (getForObject, GetAsync, postForEntity)
	// for member calls only — a bare get()/post() identifier is common as a
	// local helper, a bare getSomething() even more so.
	method := ""
	if !hasWrapper {
		if v, ok := clientHTTPVerbs[strings.ToLower(name)]; ok {
			method = v
		} else if isMember {
			method = verbCamelPrefix(name)
		}
	}

	callNode := enclosingCallNode(nameNode)
	if callNode == nil {
		return
	}
	argsNode := callArguments(callNode)
	if argsNode == nil {
		return
	}
	argNodes := unwrapArgs(argsNode)

	path, relative := "", false
	if hasWrapper {
		if wrapper.PathArg < 0 || wrapper.PathArg >= len(argNodes) || !isStringNode(argNodes[wrapper.PathArg]) {
			return
		}
		// Wrapper paths skip the "/" gate: a wrapper may prepend its own base.
		path = normalizeClientPath(argNodes[wrapper.PathArg].Content(data))
		if wrapper.Method != "" {
			method = strings.ToUpper(wrapper.Method)
		} else if wrapper.MethodArg != nil && *wrapper.MethodArg >= 0 && *wrapper.MethodArg < len(argNodes) {
			v := strings.ToLower(strings.Trim(argNodes[*wrapper.MethodArg].Content(data), "\"'`"))
			method = clientHTTPVerbs[v] // unknown verb → "" (fetch-style wildcard)
		}
	} else {
		for _, n := range argNodes {
			if p, rel := rootedPathIn(n, data, 1); p != "" {
				path, relative = p, rel
				break
			}
		}
		// Options-object shape: axios({method: 'post', url: '/x'}),
		// fetch-alikes with {method: 'PUT'}, Python dict configs.
		optMethod, optPath, optRel := scanOptionsObjects(argNodes, data)
		if path == "" {
			path, relative = optPath, optRel
		}
		if method == "" {
			if optMethod != "" {
				method = optMethod
			} else {
				// Verb-shaped sibling string: apiRequest("post", "/x"),
				// http.NewRequest("POST", "/x", nil), requests.request("GET", "/x").
				for _, n := range argNodes {
					if !isStringNode(n) {
						continue
					}
					if v, ok := clientHTTPVerbs[strings.ToLower(strings.Trim(n.Content(data), "\"'`"))]; ok {
						method = v
						break
					}
				}
			}
		}
		if method == "" {
			// Enum-shaped verb argument: Alamofire `method: .post`, Spring
			// `HttpMethod.POST`, Retrofit-adjacent `Method.GET` — a bare or
			// dotted member path whose last segment is a verb.
			for _, n := range argNodes {
				t := strings.TrimSpace(n.Content(data))
				if len(t) > 40 || strings.ContainsAny(t, "(\"'` \n") {
					continue
				}
				if i := strings.LastIndexByte(t, '.'); i >= 0 {
					if v, ok := clientHTTPVerbs[strings.ToLower(t[i+1:])]; ok {
						method = v
						break
					}
				}
			}
		}
		// Builder chains: `.url("https://…")`/`.uri(…)` carries the path and
		// the verb lives on a sibling link (OkHttp `.post(body)`, or
		// `.method("POST", …)`); no verb in the chain = wildcard, OkHttp's
		// default GET included.
		urlBuilder := isMember && path != "" && (strings.EqualFold(name, "url") || strings.EqualFold(name, "uri"))
		if urlBuilder && method == "" {
			method = verbInChain(callNode, data)
		}
		// Both gates or nothing: verb evidence without a path is any getX()
		// call; a path without verb evidence is any string that starts
		// with "/" (strings.HasPrefix, file paths, lookup keys).
		if path == "" || (method == "" && !urlBuilder) {
			return
		}
	}
	if path == "" {
		return
	}

	c.pendingClientCalls = append(c.pendingClientCalls, clientCall{
		method: method, path: path, file: relPath,
		line: int(callNode.StartPoint().Row) + 1, relative: relative,
	})
}

// enclosingCallNode walks up from a captured callee-name node to the call
// expression that owns it: identifier → (selector/member/attribute/field
// wrapper) → call node. Node type names differ per grammar; three hops
// bounds the walk.
func enclosingCallNode(nameNode *sitter.Node) *sitter.Node {
	n := nameNode.Parent()
	for hops := 0; n != nil && hops < 3; hops++ {
		switch n.Type() {
		case "call_expression", "call", "method_invocation", "invocation_expression":
			return n
		}
		n = n.Parent()
	}
	return nil
}

// unwrapArgs returns the argument expression nodes, unwrapping the extra
// wrapper node some grammars insert (C# `argument`, Python
// `keyword_argument` — for the latter the value is the last named child).
func unwrapArgs(argsNode *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	for i := 0; i < int(argsNode.NamedChildCount()); i++ {
		n := argsNode.NamedChild(i)
		if n.Type() == "argument" || n.Type() == "keyword_argument" || n.Type() == "value_argument" {
			if v := n.NamedChild(int(n.NamedChildCount()) - 1); v != nil {
				n = v
			}
		}
		out = append(out, n)
	}
	return out
}

// isStringNode matches every grammar's string-literal node ("string",
// "template_string", "interpreted_string_literal", "string_literal",
// "interpolated_string_expression", ...).
func isStringNode(n *sitter.Node) bool {
	return strings.Contains(n.Type(), "string")
}

// isRootedPath gates non-wrapper path candidates: rooted, more than "/",
// not protocol-relative ("//cdn..."), no whitespace.
func isRootedPath(p string) bool {
	return strings.HasPrefix(p, "/") && len(p) > 1 &&
		!strings.HasPrefix(p, "//") && !strings.ContainsAny(p, " \t\n")
}

// acceptClientPath additionally admits *relative* paths (BaseAddress-style
// C#/Java clients, Retrofit values like "users/{id}"): at least two clean
// segments, no dots (rejects "img/logo.png"-shaped resource strings). The
// relative flag makes wiring demand a ≥2-segment route match.
func acceptClientPath(p string) (relative, ok bool) {
	if isRootedPath(p) {
		return false, true
	}
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, " \t\n.") {
		return false, false
	}
	segs := splitPathSegments(p)
	if len(segs) < 2 || len(segs) != strings.Count(p, "/")+1 {
		return false, false
	}
	return true, true
}

// rootedPathIn returns the normalized acceptable path in an argument node:
// the node itself when it's a string literal, or — descending depth more
// level(s) of nested call arguments — a format-wrapper's template, the common
// Go idiom http.NewRequest("DELETE", fmt.Sprintf("/x/%d", id), nil).
func rootedPathIn(n *sitter.Node, data []byte, depth int) (string, bool) {
	if isStringNode(n) {
		p := normalizeClientPath(n.Content(data))
		if rel, ok := acceptClientPath(p); ok {
			return p, rel
		}
		return "", false
	}
	if depth <= 0 {
		return "", false
	}
	switch n.Type() {
	case "call_expression", "call", "method_invocation", "invocation_expression":
		if args := callArguments(n); args != nil {
			for _, inner := range unwrapArgs(args) {
				if p, rel := rootedPathIn(inner, data, depth-1); p != "" {
					return p, rel
				}
			}
		}
	}
	return "", false
}

// callArguments finds a call node's argument list: the "arguments" field
// where the grammar has one, else a value_arguments/call_suffix child
// (Kotlin and Swift grammars are field-less here).
func callArguments(callNode *sitter.Node) *sitter.Node {
	if args := callNode.ChildByFieldName("arguments"); args != nil {
		return args
	}
	for i := 0; i < int(callNode.NamedChildCount()); i++ {
		ch := callNode.NamedChild(i)
		switch ch.Type() {
		case "value_arguments", "call_suffix":
			if ch.Type() == "call_suffix" {
				for j := 0; j < int(ch.NamedChildCount()); j++ {
					if g := ch.NamedChild(j); g.Type() == "value_arguments" {
						return g
					}
				}
				continue
			}
			return ch
		}
	}
	return nil
}

// scanOptionsObjects pulls method/url evidence out of object/dict literal
// arguments: a "method" key with a verb string value, and a "url" or "path"
// key with an acceptable string value.
func scanOptionsObjects(argNodes []*sitter.Node, data []byte) (method, path string, relative bool) {
	for _, n := range argNodes {
		t := n.Type()
		if t != "object" && t != "dictionary" {
			continue
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			pair := n.NamedChild(i)
			key := pair.ChildByFieldName("key")
			val := pair.ChildByFieldName("value")
			if key == nil || val == nil || !isStringNode(val) {
				continue
			}
			switch strings.Trim(key.Content(data), "\"'`") {
			case "method":
				if v, ok := clientHTTPVerbs[strings.ToLower(strings.Trim(val.Content(data), "\"'`"))]; ok && method == "" {
					method = v
				}
			case "url", "path":
				if p := normalizeClientPath(val.Content(data)); path == "" {
					if rel, ok := acceptClientPath(p); ok {
						path, relative = p, rel
					}
				}
			}
		}
	}
	return method, path, relative
}

// verbInChain climbs from a builder link (`.url(...)`) to the top of its
// call chain and text-scans the whole chain for a verb link: `.post(` /
// `.Post(` or a quoted "POST". "" when the chain never names a method.
func verbInChain(callNode *sitter.Node, data []byte) string {
	top := callNode
	for p := top.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "call_expression", "call", "method_invocation", "invocation_expression",
			"navigation_expression", "member_access_expression", "selector_expression",
			"member_expression", "field_expression", "attribute",
			"call_suffix", "navigation_suffix":
			top = p
		default:
			p = nil
		}
		if p == nil {
			break
		}
	}
	text := top.Content(data)
	if len(text) > 2000 {
		text = text[:2000]
	}
	lower := strings.ToLower(text)
	for verb, m := range clientHTTPVerbs {
		if strings.Contains(lower, "."+verb+"(") || strings.Contains(text, `"`+m+`"`) {
			return m
		}
	}
	return ""
}

// verbCamelPrefix maps a camelCase verb-prefixed member name to its HTTP
// method: getForObject → GET, GetAsync → GET, postForEntity → POST. The
// character after the verb must be uppercase (camel boundary) so "getter"
// or "posture" never match.
func verbCamelPrefix(name string) string {
	for verb, m := range clientHTTPVerbs {
		if len(name) > len(verb) && strings.EqualFold(name[:len(verb)], verb) {
			if r := name[len(verb)]; r >= 'A' && r <= 'Z' {
				return m
			}
		}
	}
	return ""
}
