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
//
// Precision rules (a wrong edge costs more than a missing one — trace and
// impact present these edges as facts):
//   - A constant is resolved by scope: same file, then (Go) same package, then
//     a qualified pkg.Name / Enum.MEMBER reference. A bare name crosses files
//     only for a module-level ALL_CAPS constant that is unique in the project.
//     Constants declared inside a function never resolve: a test's local
//     `const d = "0123…"` must not turn every call passing a variable d into
//     an event.
//   - A lookup table counts only when its values are functions: a Go map must
//     have a value type that can hold one, and at least half of any table's
//     entries must name a known function ({"key": key} is data, not dispatch).
//   - A switch/case links only callers of the function holding the switch, or
//     callers using an emit-style verb (emit, dispatch, publish, …).
//   - A generic one-word key ("error", "done", "label") links only through an
//     emit-style verb; distinctive keys ("order.paid", "createPdf") through any
//     call whose first argument is the key.

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/system4us/graphAllTheThings/pkg/graph"

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

// defaultEmitPrefixes are lower-cased call-name prefixes that clearly send an
// event or message (emit, emitEvent, Publish, dispatchAction, queueJob, …).
// Extend with exact names via .gatt/dispatch.json {"emit_verbs": [...]}.
var defaultEmitPrefixes = []string{
	"emit", "dispatch", "publish", "trigger", "fire", "broadcast",
	"enqueue", "queue", "notify", "signal", "raise",
}

// dispatchAssoc is one discovered key→function association. Either key is a
// literal string, or keyConst is a constant/enum reference resolved in
// wireDispatch against dispatchConsts (or matched literally when both sides
// reference the same qualified enum like Events.PAID).
type dispatchAssoc struct {
	key          string
	keyConst     string
	identName    string
	targetFuncID string // optional direct function node ID (e.g. inline callback fallback)
	file         string // declaring file, for scoped constant resolution
	kind         string // "register", "table" or "switch"
	owner        string // switch: name of the function holding the switch
	group        int    // table: which literal the entry belongs to
}

// dispatchTrigger is one call site whose first argument is a string literal or
// a constant/enum reference — resolved against the registry built in wireDispatch.
type dispatchTrigger struct {
	callerFuncID string
	arg          string
	argConst     string
	file         string
	callName     string // the called name without receiver ("emit", "contentKind")
}

// dispatchConst is one string constant: its value, where it is declared,
// and whether it is visible outside its function (module/package level).
type dispatchConst struct {
	val      string
	file     string
	topLevel bool
}

// loadDispatchVerbs reads .gatt/dispatch.json once —
// {"registration_verbs": ["receivedMsj", ...], "emit_verbs": ["sendJob"]} —
// extending defaultDispatchVerbs with call names an in-house event/queue
// wrapper uses for registering and for sending.
func (c *Connector) loadDispatchVerbs() {
	c.dispatchVerbs = map[string]bool{}
	c.emitVerbs = map[string]bool{}
	for v := range defaultDispatchVerbs {
		c.dispatchVerbs[v] = true
	}
	data, err := os.ReadFile(filepath.Join(c.dir, ".gatt", "dispatch.json"))
	if err != nil {
		return
	}
	var payload struct {
		RegistrationVerbs []string `json:"registration_verbs"`
		EmitVerbs         []string `json:"emit_verbs"`
	}
	if json.Unmarshal(data, &payload) == nil {
		for _, v := range payload.RegistrationVerbs {
			if v != "" {
				c.dispatchVerbs[v] = true
			}
		}
		for _, v := range payload.EmitVerbs {
			if v != "" {
				c.emitVerbs[v] = true
			}
		}
	}
}

// isEmitVerb reports whether a call name clearly sends an event/message.
func (c *Connector) isEmitVerb(name string) bool {
	if c.emitVerbs[name] {
		return true
	}
	l := strings.ToLower(name)
	for _, p := range defaultEmitPrefixes {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// isDistinctiveKey reports whether a dispatch key is specific enough to link
// through any call that passes it ("order.paid", "createPdf", "user_created"),
// as opposed to a generic word ("error", "done", "text") that a log line, a
// map lookup or a test name passes just as often.
func isDistinctiveKey(k string) bool {
	if len(k) < 4 || strings.ContainsAny(k, " \t\n") {
		return false
	}
	if strings.ContainsAny(k, ".:/_-") {
		return true
	}
	prevLower := false
	for _, r := range k {
		if unicode.IsUpper(r) && prevLower {
			return true // camelCase transition
		}
		prevLower = unicode.IsLower(r)
	}
	return false
}

// isConstLikeName reports an ALL_CAPS constant name (EVT_PAID, ORDER_PAID2).
func isConstLikeName(name string) bool {
	letters := 0
	for _, r := range name {
		switch {
		case unicode.IsUpper(r):
			letters++
		case unicode.IsDigit(r) || r == '_':
		default:
			return false
		}
	}
	return letters >= 2
}

func (c *Connector) recordDispatchConst(name, val, file string, topLevel bool) {
	if name == "" || val == "" {
		return
	}
	if c.dispatchConsts == nil {
		c.dispatchConsts = map[string][]dispatchConst{}
	}
	c.dispatchConsts[name] = append(c.dispatchConsts[name], dispatchConst{val: val, file: file, topLevel: topLevel})
}

// enclosingFuncName returns the name of the function/method a node sits in
// ("" at module level), across Go, JS/TS and Python.
func enclosingFuncName(n *sitter.Node, data []byte) string {
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "function_declaration", "method_declaration", "method_definition",
			"function_definition", "generator_function_declaration":
			if nn := p.ChildByFieldName("name"); nn != nil {
				return nn.Content(data)
			}
			return ""
		case "arrow_function", "function_expression", "function":
			if vd := p.Parent(); vd != nil && vd.Type() == "variable_declarator" {
				if nn := vd.ChildByFieldName("name"); nn != nil {
					return nn.Content(data)
				}
			}
			return ""
		}
	}
	return ""
}

// goTopLevel reports whether a Go const_spec is declared at package level.
func goTopLevel(spec *sitter.Node) bool {
	decl := spec.Parent()
	return decl != nil && decl.Parent() != nil && decl.Parent().Type() == "source_file"
}

// pyTopLevel reports whether a Python assignment is a module-level statement.
func pyTopLevel(assign *sitter.Node) bool {
	st := assign.Parent()
	return st != nil && st.Type() == "expression_statement" && st.Parent() != nil && st.Parent().Type() == "module"
}

// goBasicTypes are Go value types that cannot hold a function value.
var goBasicTypes = map[string]bool{
	"string": true, "bool": true, "byte": true, "rune": true, "error": true, "any": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
}

// goMapHoldsFuncs reports whether a Go composite literal is a map whose value
// type can hold a function: func(...) or a named type (HandlerFunc,
// http.HandlerFunc). map[string]string/any/struct/slice literals are data.
func goMapHoldsFuncs(comp *sitter.Node, data []byte) bool {
	typ := comp.ChildByFieldName("type")
	if typ == nil || typ.Type() != "map_type" {
		return false
	}
	v := typ.ChildByFieldName("value")
	if v == nil {
		return false
	}
	switch v.Type() {
	case "function_type", "qualified_type":
		return true
	case "type_identifier":
		return !goBasicTypes[v.Content(data)]
	}
	return false
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
func (c *Connector) collectDispatchTable(objNode *sitter.Node, file string, data []byte) {
	if objNode == nil {
		return
	}
	c.dispatchGroup++
	group := c.dispatchGroup
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
					key: lit, keyConst: constRef, identName: ident, file: file, kind: "table", group: group,
				})
			}
			continue
		}
		if key := dispatchKeyText(keyNode, data); key != "" {
			c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, dispatchAssoc{
				key: key, identName: ident, file: file, kind: "table", group: group,
			})
		}
	}
}

// collectDispatchFile scans one parsed file for:
//   - string constants and enum/const objects (for resolving emit(EVT_PAID))
//   - Go map & Python dict dispatch tables
//   - switch/case string-literal event dispatch (JS/TS, Go)
func (c *Connector) collectDispatchFile(root *sitter.Node, ext, file string, data []byte) {
	if root == nil {
		return
	}
	isJS := ext == ".js" || ext == ".jsx" || ext == ".ts" || ext == ".tsx"
	isGo := ext == ".go"
	isPy := ext == ".py"
	if !isJS && !isGo && !isPy {
		return
	}
	goPkg := ""
	if isGo {
		for i := 0; i < int(root.NamedChildCount()); i++ {
			if pc := root.NamedChild(i); pc.Type() == "package_clause" && pc.NamedChildCount() > 0 {
				goPkg = pc.NamedChild(0).Content(data)
				break
			}
		}
	}
	switchCase := func(n, valNode *sitter.Node) {
		lit, constRef := extractDispatchKey(valNode, data)
		if lit == "" && constRef == "" {
			return
		}
		owner := enclosingFuncName(n, data)
		for _, fn := range collectBodyCallIdents(n, data, 3) {
			c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, dispatchAssoc{
				key: lit, keyConst: constRef, identName: fn, file: file, kind: "switch", owner: owner,
			})
		}
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
					top := isTopLevelDeclarator(n)
					if isStringLiteral(valNode) {
						c.recordDispatchConst(name, unquoteDispatchKey(valNode, data), file, top)
					} else if valNode.Type() == "object" && top {
						// Record string-valued properties like const Events = { PAID: "order.paid" }
						for i := 0; i < int(valNode.NamedChildCount()); i++ {
							p := valNode.NamedChild(i)
							if p.Type() != "pair" {
								continue
							}
							k := p.ChildByFieldName("key")
							v := p.ChildByFieldName("value")
							if k != nil && v != nil && isStringLiteral(v) {
								c.recordDispatchConst(name+"."+dispatchKeyText(k, data), unquoteDispatchKey(v, data), file, true)
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
								c.recordDispatchConst(enumName+"."+mn.Content(data), unquoteDispatchKey(mv, data), file, true)
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
						name, v, top := nameNode.Content(data), unquoteDispatchKey(val, data), goTopLevel(n)
						c.recordDispatchConst(name, v, file, top)
						if top && goPkg != "" {
							// Other packages reference it qualified: pkg.EvtPaid.
							c.recordDispatchConst(goPkg+"."+name, v, file, true)
						}
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
						c.recordDispatchConst(left.Content(data), unquoteDispatchKey(right, data), file, pyTopLevel(n))
					} else if right.Type() == "dictionary" {
						c.collectPyDispatchDict(right, file, data)
					}
				}
			}
		case "literal_value":
			// Go: map[string]func(...){ "paid": handlePaid }
			if isGo && n.Parent() != nil && n.Parent().Type() == "composite_literal" && goMapHoldsFuncs(n.Parent(), data) {
				c.collectGoDispatchMap(n, file, data)
			}
		case "switch_case":
			// JS/TS: case "order.paid": handlePaid()
			if isJS {
				switchCase(n, n.ChildByFieldName("value"))
			}
		case "expression_case":
			// Go: case "order.paid": handlePaid()
			if isGo {
				valNode := n.ChildByFieldName("value")
				if valNode != nil && valNode.Type() == "expression_list" && valNode.NamedChildCount() > 0 {
					valNode = valNode.NamedChild(0)
				}
				switchCase(n, valNode)
			}
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
}

func (c *Connector) collectGoDispatchMap(litVal *sitter.Node, file string, data []byte) {
	c.dispatchGroup++
	group := c.dispatchGroup
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
				key: lit, keyConst: constRef, identName: ident, file: file, kind: "table", group: group,
			})
		}
	}
}

func (c *Connector) collectPyDispatchDict(dictNode *sitter.Node, file string, data []byte) {
	c.dispatchGroup++
	group := c.dispatchGroup
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
				key: lit, keyConst: constRef, identName: ident, file: file, kind: "table", group: group,
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
func (c *Connector) detectDispatchCall(calleeNode *sitter.Node, callName, callerFuncID, file string, data []byte) {
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
	if i := strings.LastIndexByte(verb, '.'); i >= 0 {
		verb = verb[i+1:]
	}
	isReg := c.dispatchVerbs[verb]

	if !isReg && callerFuncID != "" {
		c.pendingDispatchTriggers = append(c.pendingDispatchTriggers, dispatchTrigger{
			callerFuncID: callerFuncID, arg: lit, argConst: constRef, file: file, callName: verb,
		})
	}
	if !isReg || args.NamedChildCount() < 2 {
		return
	}

	reg := func(a dispatchAssoc) {
		a.key, a.keyConst, a.file, a.kind = lit, constRef, file, "register"
		c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, a)
	}
	arg1 := unwrapArg(args.NamedChild(1))
	if ident := extractHandlerIdent(arg1, data); ident != "" {
		reg(dispatchAssoc{identName: ident})
		return
	}
	if isInlineCallback(arg1) {
		innerCalls := collectBodyCallIdents(arg1, data, 5)
		if len(innerCalls) > 0 {
			for _, fn := range innerCalls {
				reg(dispatchAssoc{identName: fn})
			}
		} else if callerFuncID != "" {
			reg(dispatchAssoc{targetFuncID: callerFuncID})
		}
	}
}

// resolveDispatchKey turns a key expression found in file into the string it
// stands for, by scope (see the precision rules at the top of this file).
// "" means unresolvable: no edge.
func (c *Connector) resolveDispatchKey(lit, constRef, file string) string {
	if lit != "" {
		return lit
	}
	if constRef == "" {
		return ""
	}
	cands := c.dispatchConsts[constRef]
	for _, k := range cands {
		if k.file == file {
			return k.val
		}
	}
	unique := func(match func(dispatchConst) bool) string {
		val := ""
		for _, k := range cands {
			if !k.topLevel || !match(k) {
				continue
			}
			if val != "" && val != k.val {
				return "" // ambiguous: two different values under one name
			}
			val = k.val
		}
		return val
	}
	if !strings.Contains(constRef, ".") {
		if strings.HasSuffix(file, ".go") {
			// Go: a bare name is visible package-wide, never beyond.
			dir := path.Dir(file)
			return unique(func(k dispatchConst) bool { return path.Dir(k.file) == dir })
		}
		// JS/TS/Python: an imported constant — only a distinctive ALL_CAPS
		// module-level name, so a local variable never borrows another
		// file's value.
		if isConstLikeName(constRef) {
			return unique(func(dispatchConst) bool { return true })
		}
		return ""
	}
	if v := unique(func(dispatchConst) bool { return true }); v != "" {
		return v
	}
	// Both sides naming the same enum member (Events.ORDER_PAID imported from
	// elsewhere) still match on the qualified token, without guessing a value.
	// Only enum/class-style refs: a variable's field (cfg.name, r.Method) is not
	// a key.
	if first := constRef[:strings.IndexByte(constRef, '.')]; first != "" && unicode.IsUpper(rune(first[0])) {
		return constRef
	}
	return ""
}

// langFamily groups file extensions that can call each other by name.
func langFamily(file string) string {
	ext := strings.ToLower(path.Ext(file))
	switch ext {
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".vue", ".svelte":
		return "js"
	case ".java", ".kt", ".kts", ".scala":
		return "jvm"
	}
	return ext
}

// sameLangFuncs keeps the function ids declared in the same language family
// as file: a JS handler named reject (a Promise callback) is never Go's
// reject method.
func sameLangFuncs(g *graph.Graph, ids []string, file string) []string {
	fam := langFamily(file)
	var out []string
	for _, id := range ids {
		if n := g.Nodes[id]; n != nil && langFamily(n.Attrs["file"]) == fam {
			out = append(out, id)
		}
	}
	return out
}

// wireDispatch resolves every queued dispatchAssoc against the project-wide
// function index (supporting 1-to-N fan-out), then wires heuristic CALLS edges
// (inferred=true, via=dispatch, event=<key>) from each queued trigger's
// enclosing function to every registered listener.
func (c *Connector) wireDispatch(g *graph.Graph, funcsByName map[string][]string) {
	type entry struct {
		target string
		kind   string
		owner  string
	}
	registry := map[string][]entry{}
	seenTarget := map[string]bool{}
	addTarget := func(key string, en entry) {
		if key == "" || en.target == "" {
			return
		}
		dedup := key + "\x00" + en.target + "\x00" + en.kind + "\x00" + en.owner
		if seenTarget[dedup] {
			return
		}
		seenTarget[dedup] = true
		registry[key] = append(registry[key], en)
	}

	// A table is dispatch only when its values are functions: at least half
	// of its entries must name exactly one known function.
	groupTotal, groupHits := map[int]int{}, map[int]int{}
	for _, a := range c.pendingDispatchAssocs {
		if a.kind == "table" {
			groupTotal[a.group]++
			if len(sameLangFuncs(g, funcsByName[a.identName], a.file)) == 1 {
				groupHits[a.group]++
			}
		}
	}

	for _, a := range c.pendingDispatchAssocs {
		if a.kind == "table" && groupHits[a.group]*2 < groupTotal[a.group] {
			continue
		}
		key := c.resolveDispatchKey(a.key, a.keyConst, a.file)
		if key == "" {
			continue
		}
		if a.targetFuncID != "" {
			addTarget(key, entry{a.targetFuncID, a.kind, a.owner})
			continue
		}
		if ids := sameLangFuncs(g, funcsByName[a.identName], a.file); len(ids) == 1 {
			addTarget(key, entry{ids[0], a.kind, a.owner})
		}
	}
	c.pendingDispatchAssocs = nil

	for _, t := range c.pendingDispatchTriggers {
		key := c.resolveDispatchKey(t.arg, t.argConst, t.file)
		if key == "" {
			continue
		}
		emits := c.isEmitVerb(t.callName)
		for _, en := range registry[key] {
			if en.target == t.callerFuncID {
				continue
			}
			switch {
			case en.kind == "switch" && en.owner != "" && t.callName == en.owner:
				// The trigger calls the very function holding the switch.
			case emits:
			case en.kind != "switch" && isDistinctiveKey(key):
			default:
				continue
			}
			g.AddEdge(t.callerFuncID, en.target, graph.EdgeCalls, map[string]string{
				"inferred": "true",
				"via":      "dispatch",
				"event":    key,
			})
		}
	}
	c.pendingDispatchTriggers = nil
}
