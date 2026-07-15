package codebase

// String-keyed dispatch resolution: connects a call site passing a literal
// string key (queueJob("createPdf"), io.emit("connected")) to the function
// that string names elsewhere in the codebase — either a module-scope
// lookup table ({createPdf: createPdfHandler}) or a call that registers a
// handler under that name (io.on("connected", onConnected)). CALLS
// resolution alone can't see this: the call site never mentions the
// handler function by name, only by a string the handler is associated
// with somewhere else.
//
// Two source shapes populate the registry, both requiring a function
// *identifier* as evidence — not just a matching string — to keep false
// positives down:
//  1. object-literal dispatch tables: {key: funcIdentifier, ...}, captured
//     alongside the existing top-level const/lookup-table handling in
//     codebase.go.
//  2. calls to a known "registration verb" (on, addEventListener,
//     addListener, once, subscribe — extend via .gatt/dispatch.json) whose
//     first argument is the string key and second is a function
//     identifier.
//
// Once the registry is built (after the whole tree is parsed — a
// declaration can be seen after its first use in source order), every call
// site with a string-literal first argument is checked against it and, on
// a match, gets a heuristic CALLS edge (inferred=true) from its enclosing
// function to the registered one. This is deliberately unfiltered by the
// *trigger* call's own name — queueJob("createPdf"), io.emit("connected"),
// and a custom io.receivedMsj("connected") are all covered without
// per-framework wiring, since it's the registry entry (not the trigger
// callee) that's gated by the two shapes above.
//
// JS/TS/JSX only — the object-literal lookup-table shape is itself JS/TS/
// JSX-only (same scope as the existing queueDefinitions-style const
// handling), and the surrounding call/arguments AST shape this file
// navigates (call_expression / arguments) isn't uniform across every
// supported language.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"graphallthethings/internal/graph"

	sitter "github.com/smacker/go-tree-sitter"
)

// defaultDispatchVerbs are the built-in registration-call names recognized
// without configuration — the common EventEmitter/DOM-listener surface.
var defaultDispatchVerbs = map[string]bool{
	"on": true, "addEventListener": true, "addListener": true,
	"once": true, "subscribe": true,
}

// dispatchAssoc is one discovered string→function association. identName is
// resolved against the project-wide function index (funcsByName) in
// wireDispatch, once the whole tree — including files parsed after this one
// — has been walked.
type dispatchAssoc struct {
	key       string
	identName string
}

// dispatchTrigger is one call site whose first argument is a string
// literal — a candidate dispatch/emit call, resolved against the registry
// built from pendingDispatchAssocs in wireDispatch.
type dispatchTrigger struct {
	callerFuncID string
	arg          string
}

// loadDispatchVerbs reads .gatt/dispatch.json once —
// {"registration_verbs": ["receivedMsj", ...]} — extending
// defaultDispatchVerbs with call names an in-house event/queue wrapper uses
// that the built-in set can't guess. A missing or malformed file leaves
// just the defaults.
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

// dispatchKeyText extracts a pair/argument's key as plain text: an
// unquoted string, or a bare identifier's own name.
func dispatchKeyText(n *sitter.Node, data []byte) string {
	if n.Type() == "string" {
		return strings.Trim(n.Content(data), "\"'`")
	}
	return n.Content(data)
}

// collectDispatchTable walks a module-scope object literal's pairs
// (key: identifier) and queues one dispatchAssoc per entry whose value is a
// plain identifier — the {key: funcIdentifier} table shape. Non-identifier
// values (inline functions, spreads, computed keys) are skipped: wiring
// those would need locating or synthesizing an anonymous function node,
// out of scope for this heuristic.
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
		if keyNode == nil || valNode == nil || valNode.Type() != "identifier" {
			continue
		}
		if key := dispatchKeyText(keyNode, data); key != "" {
			c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, dispatchAssoc{
				key: key, identName: valNode.Content(data),
			})
		}
	}
}

// detectDispatchCall inspects one call.func/call.sel capture for the two
// dispatch shapes: any call with a string-literal first argument queues a
// dispatchTrigger — checked against the registry, once built, regardless
// of the callee's own name — and, additionally, a call to a known
// registration verb with a string + identifier argument pair queues a
// dispatchAssoc.
func (c *Connector) detectDispatchCall(calleeNode *sitter.Node, callName, callerFuncID string, data []byte) {
	if c.dispatchVerbs == nil {
		c.loadDispatchVerbs()
	}
	// call.func's parent is the call_expression directly; call.sel's is a
	// member_expression one level further in — one extra hop covers both.
	callExpr := calleeNode.Parent()
	if callExpr != nil && callExpr.Type() != "call_expression" {
		callExpr = callExpr.Parent()
	}
	if callExpr == nil || callExpr.Type() != "call_expression" {
		return
	}
	args := callExpr.ChildByFieldName("arguments")
	if args == nil || args.NamedChildCount() == 0 {
		return
	}
	arg0 := args.NamedChild(0)
	if arg0.Type() != "string" {
		return
	}
	key := dispatchKeyText(arg0, data)
	if key == "" {
		return
	}
	if callerFuncID != "" {
		c.pendingDispatchTriggers = append(c.pendingDispatchTriggers, dispatchTrigger{
			callerFuncID: callerFuncID, arg: key,
		})
	}
	if !c.dispatchVerbs[strings.TrimPrefix(callName, ".")] || args.NamedChildCount() < 2 {
		return
	}
	if arg1 := args.NamedChild(1); arg1.Type() == "identifier" {
		c.pendingDispatchAssocs = append(c.pendingDispatchAssocs, dispatchAssoc{
			key: key, identName: arg1.Content(data),
		})
	}
}

// wireDispatch resolves every queued dispatchAssoc against the project-wide
// function index, then wires a heuristic CALLS edge (inferred=true) from
// each queued trigger's enclosing function to the matching one. Ambiguous
// identifiers — the same name resolved in more than one file — are
// skipped rather than guessed, same as every other name-based heuristic in
// this connector.
func (c *Connector) wireDispatch(g *graph.Graph, funcsByName map[string][]string) {
	registry := map[string]string{}
	for _, a := range c.pendingDispatchAssocs {
		if ids := funcsByName[a.identName]; len(ids) == 1 {
			if _, exists := registry[a.key]; !exists {
				registry[a.key] = ids[0]
			}
		}
	}
	c.pendingDispatchAssocs = nil
	for _, t := range c.pendingDispatchTriggers {
		if target, ok := registry[t.arg]; ok && target != t.callerFuncID {
			g.AddEdge(t.callerFuncID, target, graph.EdgeCalls, map[string]string{"inferred": "true", "via": "dispatch"})
		}
	}
	c.pendingDispatchTriggers = nil
}
