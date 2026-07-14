package codebase

import (
	"fmt"

	sitter "github.com/smacker/go-tree-sitter"

	"graphallthethings/internal/graph"
)

// stateAccessRaw is one property access on a tracked singleton binding,
// collected during a file's parse pass and resolved against `singletons`
// after the whole file has been walked (emitStateAccess) — deferred the
// same way looseComments is, since an access can be matched before the
// import statement that resolves its binding.
//
// Scope (v1, JS/TS/JSX only): only property access on identifiers bound by
// a named import (`import { config } from './config'`) or a CommonJS
// `require(...)` is tracked — i.e. cross-file singletons, not local
// variables/params. `Object.assign(x.prop, {...})` is tracked as a write to
// `x.prop` (one level of nesting only); Go package-level vars and Python
// module globals are explicitly out of scope. This is a heuristic data-flow
// signal, not an exact one like CALLS.
type stateAccessRaw struct {
	kind string // "read" or "write"
	obj  string
	prop string
	line int
}

// isStateWriteLHS reports whether n (a `object.prop` member_expression) is
// the left-hand side of an assignment, directly (`obj.prop = ...`) or one
// level nested (`obj.prop.sub = ...`, where n is the inner `obj.prop`) — in
// both cases it's already recorded as a write by the state.write/state.write2
// captures, so the broader state.read pattern must not also record it as a
// read. Compares byte ranges since go-tree-sitter has no direct node-identity
// check.
func isStateWriteLHS(n *sitter.Node) bool {
	sameNode := func(a, b *sitter.Node) bool {
		return a != nil && b != nil && a.StartByte() == b.StartByte() && a.EndByte() == b.EndByte()
	}
	p := n.Parent()
	if p == nil {
		return false
	}
	if p.Type() == "assignment_expression" && sameNode(p.ChildByFieldName("left"), n) {
		return true
	}
	if p.Type() == "member_expression" {
		if gp := p.Parent(); gp != nil && gp.Type() == "assignment_expression" {
			return sameNode(gp.ChildByFieldName("left"), p)
		}
	}
	return false
}

// isCallCallee reports whether n is the function being called in a
// call_expression (`obj.method()`) — a method call, not a data read, so the
// state.read pattern must exclude it.
func isCallCallee(n *sitter.Node) bool {
	p := n.Parent()
	if p == nil || p.Type() != "call_expression" {
		return false
	}
	fn := p.ChildByFieldName("function")
	return fn != nil && fn.StartByte() == n.StartByte() && fn.EndByte() == n.EndByte()
}

// isObjectAssignTarget reports whether n (a `obj.prop` member_expression) is
// the first argument of `Object.assign(obj.prop, ...)` — a mutation of
// obj.prop, not a read, so the state.oa_call capture records it as a write
// (see wireObjectAssign) and the broader state.read pattern must exclude it.
func isObjectAssignTarget(n *sitter.Node, src []byte) bool {
	args := n.Parent()
	if args == nil || args.Type() != "arguments" || args.NamedChildCount() == 0 {
		return false
	}
	first := args.NamedChild(0)
	if first.StartByte() != n.StartByte() || first.EndByte() != n.EndByte() {
		return false
	}
	call := args.Parent()
	if call == nil || call.Type() != "call_expression" {
		return false
	}
	fn := call.ChildByFieldName("function")
	if fn == nil || fn.Type() != "member_expression" {
		return false
	}
	obj, prop := fn.ChildByFieldName("object"), fn.ChildByFieldName("property")
	return obj != nil && prop != nil && obj.Content(src) == "Object" && prop.Content(src) == "assign"
}

// emitStateAccess resolves each collected access against singletons (a
// binding name resolves only if it came from a local named import — see the
// state.import_src/name captures) into a NodeState node and a READS/WRITES
// edge, attributed to the enclosing function by line range (funcs, the same
// technique CALLS attribution uses). An access outside any known function
// is dropped rather than attributed to a synthetic "module scope" node.
func emitStateAccess(g *graph.Graph, accesses []stateAccessRaw, singletons map[string]string, funcs []funcInfo, relPath string) {
	for _, a := range accesses {
		targetFile, ok := singletons[a.obj]
		if !ok {
			continue
		}
		bestIdx := -1
		for i := range funcs {
			if funcs[i].file == relPath && funcs[i].lineStart <= a.line {
				if bestIdx == -1 || funcs[i].lineStart > funcs[bestIdx].lineStart {
					bestIdx = i
				}
			}
		}
		if bestIdx < 0 || funcs[bestIdx].lineEnd < a.line {
			continue
		}

		stateID := fmt.Sprintf("state:%s:%s", targetFile, a.prop)
		if g.Nodes[stateID] == nil {
			g.AddNode(&graph.Node{
				ID:    stateID,
				Type:  graph.NodeState,
				Name:  a.obj + "." + a.prop,
				Attrs: map[string]string{"property": a.prop},
			})
		}
		edgeType := graph.EdgeReadsState
		if a.kind == "write" {
			edgeType = graph.EdgeWritesState
		}
		g.AddEdge(funcs[bestIdx].id, stateID, edgeType, nil)
	}
}
