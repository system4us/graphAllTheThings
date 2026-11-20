package codebase

// String-keyed dispatch resolution: connects a call site passing a literal
// string key — or a constant/enum resolving to one — (queueJob("createPdf"),
// io.emit("connected"), bus.Emit(Events.ORDER_PAID)) to the function(s) that
// string names elsewhere in the codebase:
//
//  1. Lookup tables: JS/TS object literals ({createPdf: createPdfHandler}),
//     Go maps (map[string]Handler{"paid": handlePaid}), Python dicts
//     (HANDLERS = {"paid": handle_paid}).
//  2. Registration calls to a known registration verb (on, addEventListener,
//     addListener, once, subscribe, Subscribe, On, Handle, Register — extend
//     via .gatt/dispatch.json) whose first argument is the event key and second
//     argument is a function identifier, member expression (this.onPaid), or
//     inline callback whose body calls handler function(s).
//  3. Switch/case event dispatch (switch (evt.type) { case "paid": handlePaid() })
//     in JS/TS and Go.
//
// Fan-out is 1-to-N: every registered listener for an event key receives a
// heuristic CALLS edge (inferred=true, via=dispatch, event=<key>) from every
// matching trigger's enclosing function.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"graphallthethings/internal/graph"

	sitter "github.com/smacker/go-tree-sitter"
)

// defaultDispatchVerbs are the built-in registration-call names recognized
// without configuration — common EventEmitter, DOM, Go, and Python pub/sub verbs.
var defaultDispatchVerbs = map[string]bool{
	"on":               true,
	"On":               true,
	"addEventListener": true,
	"addListener":      true,
	"add_listener":     true,
	"once":             true,
	"Once":             true,
	"subscribe":        true,
	"Subscribe":        true,
	"Handle":           true,
	"handle":           true,
	"Register":         true,
	"register":         true,
}

// dispatchAssoc is one discovered key→function association. Either key is a
// literal string, or keyConst is a constant/enum reference resolved in
// wireDispatch via dispatchConstValues (or matched literally when both sides
// reference the same qualified enum like Events.PAID).
type dispatchAssoc struct {
	key          string
	keyConst     string
	identName    string
	targetFuncID string // optional direct function node ID (e.g. inline callback fallback)
}

// dispatchTrigger is one call site whose first argument is a string literal or
// a constant/enum reference — resolved against the registry built in wireDispatch.
type dispatchTrigger struct {
	callerFuncID string
	arg          string
	argConst     string
}

// loadDispatchVerbs reads .gatt/dispatch.json once —
// {"registration_verbs": ["receivedMsj", ...]} — extending
// defaultDispatchVerbs with call names an in-house event/queue wrapper uses.
func (c *Connector) loadDispatchVerbs() {
	c.dispatchVerbs = map[string]bool{}
	for v := range defaultDispatchVerbs {
		c.dispatchVerbs[v] = true
	}
	data, err := os.ReadFile(filepath.Join(c.dir, ".gatt", "dispatch.json"))
	if err != nil {
		return
	}
	var payload struct {
		RegistrationVerbs []string `json:"registration_verbs"`
	}
	if json.Unmarshal(data, &payload) == nil {
		for _, v := range payload.RegistrationVerbs {
			if v != "" {
				c.dispatchVerbs[v] = true
			}
		}
	}
}

func (c *Connector) recordDispatchConst(name, val string) {
	if name == "" || val == "" {
		return
	}
	if c.dispatchConstValues == nil {
		c.dispatchConstValues = map[string]string{}
	}
	c.dispatchConstValues[name] = val
}

// isStringLiteral reports whether a tree-sitter node is a string literal in
// any supported language.
func isStringLiteral(n *sitter.Node) bool {
	if n == nil {
		return false
	}
	switch n.Type() {
	case "string", "interpreted_string_literal", "raw_string_literal",
		"string_literal", "verbatim_string_literal", "template_string":
		return true
	}
	return false
}

// unquoteDispatchKey strips quotes from a string literal. Template strings
// with interpolations (${...}) return "" so we don't match dynamic templates.
func unquoteDispatchKey(n *sitter.Node, data []byte) string {
	if n == nil {
		return ""
	}
	raw := n.Content(data)
	if n.Type() == "template_string" && strings.Contains(raw, "${") {
		return ""
	}
	// Python prefixed strings like r"..." or f"..."
	if n.Type() == "string" && len(raw) >= 2 && (raw[0] == 'r' || raw[0] == 'b' || raw[0] == 'u') && (raw[1] == '"' || raw[1] == '\'') {
		raw = raw[1:]
	}
	if strings.HasPrefix(raw, `"""`) && strings.HasSuffix(raw, `"""`) && len(raw) >= 6 {
		return raw[3 : len(raw)-3]
	}
	if strings.HasPrefix(raw, `'''`) && strings.HasSuffix(raw, `'''`) && len(raw) >= 6 {
		return raw[3 : len(raw)-3]
	}
	return strings.Trim(raw, "\"'`")
}

// extractDispatchKey classifies a key expression as either a literal string or
// a constant/enum reference (identifier or dotted member expression).
func extractDispatchKey(n *sitter.Node, data []byte) (literal, constRef string) {
	if n == nil {
		return "", ""
	}
	if isStringLiteral(n) {
		return unquoteDispatchKey(n, data), ""
	}
	switch n.Type() {
	case "identifier", "property_identifier", "field_identifier":
		return "", strings.TrimSpace(n.Content(data))
	case "member_expression", "selector_expression", "attribute", "field_access", "member_access_expression":
		txt := strings.TrimSpace(n.Content(data))
		// Keep simple dotted paths like Events.ORDER_PAID (avoid chained calls).
		if txt != "" && !strings.ContainsAny(txt, "()[] \t\n") {
			return "", txt
		}
	}
	return "", ""
}

// extractHandlerIdent extracts a target function name from an identifier or a
// dotted member/selector expression (e.g. this.onPaid -> onPaid).
func extractHandlerIdent(n *sitter.Node, data []byte) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "identifier":
		return n.Content(data)
	case "member_expression":
		if prop := n.ChildByFieldName("property"); prop != nil {
			return prop.Content(data)
		}
	case "selector_expression":
		if field := n.ChildByFieldName("field"); field != nil {
			return field.Content(data)
		}
	case "attribute":
		if attr := n.ChildByFieldName("attribute"); attr != nil {
			return attr.Content(data)
		}
	case "member_access_expression":
		if name := n.ChildByFieldName("name"); name != nil {
			return name.Content(data)
		}
	}
	return ""
}

// isInlineCallback reports whether n is an inline function/arrow/lambda literal.
func isInlineCallback(n *sitter.Node) bool {
	if n == nil {
		return false
	}
	switch n.Type() {
	case "arrow_function", "function_expression", "function", "func_literal", "lambda", "lambda_expression":
		return true
	}
	return false
}

// collectBodyCallIdents walks an inline callback or switch-case body and
// returns up to maxCalls direct callee names (identifier or final method name).
func collectBodyCallIdents(root *sitter.Node, data []byte, maxCalls int) []string {
	if root == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil || len(out) >= maxCalls {
			return
		}
		switch n.Type() {
		case "call_expression", "call", "invocation_expression":
			fn := n.ChildByFieldName("function")
			if fn == nil && n.NamedChildCount() > 0 {
				fn = n.NamedChild(0)
			}
			if name := extractHandlerIdent(fn, data); name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		case "method_invocation":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := nameNode.Content(data)
				if name != "" && !seen[name] {
					seen[name] = true
					out = append(out, name)
				}
			}
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
	return out
}

// dispatchKeyText extracts a pair/argument's key as plain text: an
// unquoted string, or a bare identifier's own name.
func dispatchKeyText(n *sitter.Node, data []byte) string {
	if isStringLiteral(n) {
		return unquoteDispatchKey(n, data)
	}
	return n.Content(data)
}

// collectDispatchTable walks a module-scope object literal's pairs
// (key: identifier | obj.method) and queues one dispatchAssoc per entry.
func (c *Connector) collectDispatchTable(objNode *sitter.Node, data []byte) {
	if objNode == nil {
		return
	}
	for i := 0; i < int(objNode.NamedChildCount()); i++ {
		pair := objNode.NamedChild(i)
		if pair.Type() != "pair" {
			continue
		}
		keyNode := pair.ChildByFieldName("key")
		valNode := pair.ChildByFieldName("value")
		if keyNode == nil || valNode == nil {
			continue
		}
		ident := extractHandlerIdent(valNode, data)
		if ident == "" {
			continue
		}
		// Computed key: {[Events.PAID]: handlePaid}
		if keyNode.Type() == "computed_property_name" && keyNode.NamedChildCount() > 0 {
			lit, constRef := extractDispatchKey(keyNode.NamedChild(0), data)
			if lit != "" || constRef != "" {
				c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, dispatchAssoc{
					key: lit, keyConst: constRef, identName: ident,
				})
			}
			continue
		}
		if key := dispatchKeyText(keyNode, data); key != "" {
			c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, dispatchAssoc{
				key: key, identName: ident,
			})
		}
	}
}

// collectDispatchFile scans one parsed file for:
//   - string constants and enum/const objects (for resolving emit(EVT_PAID))
//   - Go map & Python dict dispatch tables
//   - switch/case string-literal event dispatch (JS/TS, Go)
func (c *Connector) collectDispatchFile(root *sitter.Node, ext string, data []byte) {
	if root == nil {
		return
	}
	isJS := ext == ".js" || ext == ".jsx" || ext == ".ts" || ext == ".tsx"
	isGo := ext == ".go"
	isPy := ext == ".py"
	if !isJS && !isGo && !isPy {
		return
	}

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "variable_declarator":
			if isJS {
				nameNode := n.ChildByFieldName("name")
				valNode := n.ChildByFieldName("value")
				if nameNode != nil && valNode != nil && nameNode.Type() == "identifier" {
					name := nameNode.Content(data)
					if isStringLiteral(valNode) {
						c.recordDispatchConst(name, unquoteDispatchKey(valNode, data))
					} else if valNode.Type() == "object" {
						// Record string-valued properties like const Events = { PAID: "order.paid" }
						for i := 0; i < int(valNode.NamedChildCount()); i++ {
							p := valNode.NamedChild(i)
							if p.Type() != "pair" {
								continue
							}
							k := p.ChildByFieldName("key")
							v := p.ChildByFieldName("value")
							if k != nil && v != nil && isStringLiteral(v) {
								c.recordDispatchConst(name+"."+dispatchKeyText(k, data), unquoteDispatchKey(v, data))
							}
						}
					}
				}
			}
		case "enum_declaration":
			// TypeScript enum Events { PAID = "order.paid" }
			if isJS {
				enumName := ""
				if nn := n.ChildByFieldName("name"); nn != nil {
					enumName = nn.Content(data)
				}
				if body := n.ChildByFieldName("body"); enumName != "" && body != nil {
					for i := 0; i < int(body.NamedChildCount()); i++ {
						m := body.NamedChild(i)
						if m.Type() == "enum_assignment" {
							mn := m.ChildByFieldName("name")
							mv := m.ChildByFieldName("value")
							if mn != nil && mv != nil && isStringLiteral(mv) {
								c.recordDispatchConst(enumName+"."+mn.Content(data), unquoteDispatchKey(mv, data))
							}
						}
					}
				}
			}
		case "const_spec":
			// Go: const EvtPaid = "order.paid"
			if isGo {
				nameNode := n.ChildByFieldName("name")
				valNode := n.ChildByFieldName("value")
				if nameNode != nil && valNode != nil {
					// value in tree-sitter-go const_spec is an expression_list
					val := valNode
					if val.Type() == "expression_list" && val.NamedChildCount() > 0 {
						val = val.NamedChild(0)
					}
					if isStringLiteral(val) {
						c.recordDispatchConst(nameNode.Content(data), unquoteDispatchKey(val, data))
					}
				}
			}
		case "assignment":
			// Python: EVT_PAID = "order.paid" or HANDLERS = {"paid": handle_paid}
			if isPy {
				left := n.ChildByFieldName("left")
				right := n.ChildByFieldName("right")
				if left != nil && right != nil && left.Type() == "identifier" {
					if isStringLiteral(right) {
						c.recordDispatchConst(left.Content(data), unquoteDispatchKey(right, data))
					} else if right.Type() == "dictionary" {
						c.collectPyDispatchDict(right, data)
					}
				}
			}
		case "literal_value":
			// Go: map[string]func(...){ "paid": handlePaid }
			if isGo && n.Parent() != nil && n.Parent().Type() == "composite_literal" {
				c.collectGoDispatchMap(n, data)
			}
		case "switch_case":
			// JS/TS: case "order.paid": handlePaid()
			if isJS {
				valNode := n.ChildByFieldName("value")
				lit, constRef := extractDispatchKey(valNode, data)
				if lit != "" || constRef != "" {
					for _, fn := range collectBodyCallIdents(n, data, 3) {
						c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, dispatchAssoc{
							key: lit, keyConst: constRef, identName: fn,
						})
					}
				}
			}
		case "expression_case":
			// Go: case "order.paid": handlePaid()
			if isGo {
				valNode := n.ChildByFieldName("value")
				if valNode != nil && valNode.Type() == "expression_list" && valNode.NamedChildCount() > 0 {
					valNode = valNode.NamedChild(0)
				}
				lit, constRef := extractDispatchKey(valNode, data)
				if lit != "" || constRef != "" {
					for _, fn := range collectBodyCallIdents(n, data, 3) {
						c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, dispatchAssoc{
							key: lit, keyConst: constRef, identName: fn,
						})
					}
				}
			}
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
}

func (c *Connector) collectGoDispatchMap(litVal *sitter.Node, data []byte) {
	for i := 0; i < int(litVal.NamedChildCount()); i++ {
		el := litVal.NamedChild(i)
		if el.Type() != "keyed_element" || el.NamedChildCount() < 2 {
			continue
		}
		k := el.NamedChild(0)
		v := el.NamedChild(1)
		if k.Type() == "literal_element" && k.NamedChildCount() > 0 {
			k = k.NamedChild(0)
		}
		if v.Type() == "literal_element" && v.NamedChildCount() > 0 {
			v = v.NamedChild(0)
		}
		lit, constRef := extractDispatchKey(k, data)
		if lit == "" && constRef == "" {
			continue
		}
		if ident := extractHandlerIdent(v, data); ident != "" {
			c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, dispatchAssoc{
				key: lit, keyConst: constRef, identName: ident,
			})
		}
	}
}

func (c *Connector) collectPyDispatchDict(dictNode *sitter.Node, data []byte) {
	for i := 0; i < int(dictNode.NamedChildCount()); i++ {
		pair := dictNode.NamedChild(i)
		if pair.Type() != "pair" {
			continue
		}
		k := pair.ChildByFieldName("key")
		v := pair.ChildByFieldName("value")
		lit, constRef := extractDispatchKey(k, data)
		if lit == "" && constRef == "" {
			continue
		}
		if ident := extractHandlerIdent(v, data); ident != "" {
			c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, dispatchAssoc{
				key: lit, keyConst: constRef, identName: ident,
			})
		}
	}
}

// findCallArgs locates the argument list node for a call expression across
// JS/TS, Go, Python, Java, and C#.
func findCallArgs(calleeNode *sitter.Node) *sitter.Node {
	cur := calleeNode
	for hops := 0; hops < 3 && cur != nil; hops++ {
		cur = cur.Parent()
		if cur == nil {
			return nil
		}
		switch cur.Type() {
		case "call_expression", "call", "method_invocation", "invocation_expression":
			if args := cur.ChildByFieldName("arguments"); args != nil {
				return args
			}
			// Fallback: look for an argument_list child.
			for i := 0; i < int(cur.NamedChildCount()); i++ {
				ch := cur.NamedChild(i)
				if ch.Type() == "arguments" || ch.Type() == "argument_list" {
					return ch
				}
			}
			return nil
		}
	}
	return nil
}

// unwrapArg unwraps C# `argument` wrapper nodes to their inner expression.
func unwrapArg(n *sitter.Node) *sitter.Node {
	if n != nil && n.Type() == "argument" && n.NamedChildCount() > 0 {
		return n.NamedChild(0)
	}
	return n
}

// detectDispatchCall inspects one call.func/call.sel capture for dispatch
// triggers and registration calls across JS/TS/JSX, Go, Python, Java, and C#.
func (c *Connector) detectDispatchCall(calleeNode *sitter.Node, callName, callerFuncID string, data []byte) {
	if c.dispatchVerbs == nil {
		c.loadDispatchVerbs()
	}
	args := findCallArgs(calleeNode)
	if args == nil || args.NamedChildCount() == 0 {
		return
	}
	arg0 := unwrapArg(args.NamedChild(0))
	lit, constRef := extractDispatchKey(arg0, data)
	if lit == "" && constRef == "" {
		return
	}

	verb := strings.TrimPrefix(callName, ".")
	isReg := c.dispatchVerbs[verb]

	if !isReg && callerFuncID != "" {
		c.pendingDispatchTriggers = append(c.pendingDispatchTriggers, dispatchTrigger{
			callerFuncID: callerFuncID, arg: lit, argConst: constRef,
		})
	}
	if !isReg || args.NamedChildCount() < 2 {
		return
	}

	arg1 := unwrapArg(args.NamedChild(1))
	if ident := extractHandlerIdent(arg1, data); ident != "" {
		c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, dispatchAssoc{
			key: lit, keyConst: constRef, identName: ident,
		})
		return
	}
	if isInlineCallback(arg1) {
		innerCalls := collectBodyCallIdents(arg1, data, 5)
		if len(innerCalls) > 0 {
			for _, fn := range innerCalls {
				c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, dispatchAssoc{
					key: lit, keyConst: constRef, identName: fn,
				})
			}
		} else if callerFuncID != "" {
			c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, dispatchAssoc{
				key: lit, keyConst: constRef, targetFuncID: callerFuncID,
			})
		}
	}
}

func (c *Connector) resolveDispatchKey(lit, constRef string) string {
	if lit != "" {
		return lit
	}
	if constRef == "" {
		return ""
	}
	if v, ok := c.dispatchConstValues[constRef]; ok && v != "" {
		return v
	}
	// If both emitter and listener use the same qualified enum/const like
	// Events.ORDER_PAID (e.g. imported from another package), match on the
	// qualified token itself so we still connect them without guessing bare names.
	if strings.Contains(constRef, ".") {
		return constRef
	}
	return ""
}

// wireDispatch resolves every queued dispatchAssoc against the project-wide
// function index (supporting 1-to-N fan-out), then wires heuristic CALLS edges
// (inferred=true, via=dispatch, event=<key>) from each queued trigger's
// enclosing function to every registered listener.
func (c *Connector) wireDispatch(g *graph.Graph, funcsByName map[string][]string) {
	registry := map[string][]string{}
	seenTarget := map[string]bool{}
	addTarget := func(key, targetID string) {
		if key == "" || targetID == "" {
			return
		}
		dedup := key + "\x00" + targetID
		if seenTarget[dedup] {
			return
		}
		seenTarget[dedup] = true
		registry[key] = append(registry[key], targetID)
	}

	for _, a := range c.pendingDispatchAssocs {
		key := c.resolveDispatchKey(a.key, a.keyConst)
		if key == "" {
			continue
		}
		if a.targetFuncID != "" {
			addTarget(key, a.targetFuncID)
			continue
		}
		if ids := funcsByName[a.identName]; len(ids) == 1 {
			addTarget(key, ids[0])
		}
	}
	c.pendingDispatchAssocs = nil

	for _, t := range c.pendingDispatchTriggers {
		key := c.resolveDispatchKey(t.arg, t.argConst)
		if key == "" {
			continue
		}
		for _, target := range registry[key] {
			if target != t.callerFuncID {
				g.AddEdge(t.callerFuncID, target, graph.EdgeCalls, map[string]string{
					"inferred": "true",
					"via":      "dispatch",
					"event":    key,
				})
			}
		}
	}
	c.pendingDispatchTriggers = nil
}
