package codebase

import (
	"path"
	"strings"

	"github.com/system4us/graphAllTheThings/pkg/graph"

	sitter "github.com/smacker/go-tree-sitter"
)

// Go value-method fallback.
//
// The receiver index (receiver.go) follows a method call to its type when the
// receiver is a variable or field whose type it can read. It cannot follow a
// value produced by another call — sqlc's `q := s.q.WithTx(tx); q.InsertEvent()`,
// `cfg := s.cfg.Load(); cfg.ProviderDenied()` (atomic.Pointer[T]),
// `fl.engine(s).EvaluateOutput()` — so every query call of a sqlc codebase and
// many production callers were missing, and a signature change reported only
// the tests as affected.
//
// The fallback leans on what Go itself guarantees: a value's method can only
// be one declared in the caller's own package or in a package the caller
// file imports (a type from elsewhere would need its package imported to be
// named, and its methods are reached only through such values). When exactly
// one method of that name exists in that set, the call is wired, marked
// inferred=true, via=go-method.
//
// Guards: package-qualified calls (url.Parse, sitter.NewParser) never fall
// back; only methods are targets (a func-typed field shares no name space with
// functions); a _test.go method only for a test of the same package; and
// method names every stdlib type has (Close, Write, Load, …) are skipped,
// since a local type with a unique Close says nothing about os.File.Close.

// goGenericMethods are method names stdlib and common third-party types carry.
var goGenericMethods = map[string]bool{
	"Close": true, "Read": true, "Write": true, "WriteString": true, "WriteHeader": true,
	"Header": true, "String": true, "Error": true, "Unwrap": true, "Is": true, "As": true,
	"Len": true, "Less": true, "Swap": true, "Get": true, "Set": true, "Add": true, "Del": true,
	"Delete": true, "Run": true, "Start": true, "Stop": true, "Wait": true, "Lock": true,
	"Unlock": true, "RLock": true, "RUnlock": true, "Done": true, "Err": true, "Value": true,
	"Deadline": true, "Next": true, "Scan": true, "Load": true, "Store": true,
	"CompareAndSwap": true, "Parse": true, "Do": true, "Send": true, "Recv": true,
	"Reset": true, "Flush": true, "Sum": true, "Seek": true, "Format": true,
	"MarshalJSON": true, "UnmarshalJSON": true, "MarshalText": true, "UnmarshalText": true,
	"ServeHTTP": true, "Query": true, "QueryRow": true, "Exec": true, "QueryContext": true,
	"ExecContext": true, "QueryRowContext": true, "Begin": true, "BeginTx": true,
	"Commit": true, "Rollback": true, "Prepare": true, "Ping": true, "Contains": true,
	"Equal": true, "Copy": true, "Open": true, "Create": true, "Remove": true, "Stat": true,
	"Name": true, "Type": true, "Kind": true, "Size": true, "Bytes": true, "Text": true,
	"Int": true, "Bool": true, "Keys": true, "Values": true, "Clone": true, "Encode": true,
	"Decode": true, "Sync": true, "Fatal": true, "Fatalf": true, "Errorf": true,
	"Logf": true, "Log": true, "Helper": true, "Cleanup": true, "Skip": true,
	"Printf": true, "Println": true, "Print": true, "Info": true, "Warn": true,
	"Debug": true, "With": true, "WithContext": true, "Context": true, "Now": true,
	"Since": true, "Unix": true, "UnixMilli": true, "Sub": true, "Before": true,
	"After": true, "Match": true, "MatchString": true, "Call": true, "Handle": true,
	"Handler": true, "Use": true, "Listen": true, "Serve": true, "Shutdown": true,
	"Accept": true, "Dial": true, "SetDeadline": true, "SetReadDeadline": true,
	"SetWriteDeadline": true, "LocalAddr": true, "RemoteAddr": true, "Cancel": true,
	"Update": true, "Apply": true, "Validate": true, "Init": true,
}

// recordGoImportName notes the name an import spec binds in file: its alias,
// else the package name guessed from the path (net/url → url,
// gopkg.in/yaml.v3 → yaml, github.com/x/go-tree-sitter → tree-sitter's
// "treesitter"/"sitter" family by stripping go-/-go and dashes, …/v2 → the
// segment before it). A guess that misses only costs the fallback for that
// one name.
func (c *Connector) recordGoImportName(file string, importNode *sitter.Node, importStr string, data []byte) {
	names := map[string]bool{}
	if sp := importNode.Parent(); sp != nil && sp.Type() == "import_spec" {
		if an := sp.ChildByFieldName("name"); an != nil {
			alias := an.Content(data)
			if alias == "_" || alias == "." {
				return
			}
			names[alias] = true
		}
	}
	if len(names) == 0 {
		segs := strings.Split(importStr, "/")
		last := segs[len(segs)-1]
		if len(segs) > 1 && len(last) >= 2 && last[0] == 'v' && strings.Trim(last[1:], "0123456789") == "" {
			last = segs[len(segs)-2]
		}
		if i := strings.Index(last, ".v"); i > 0 {
			last = last[:i]
		}
		names[last] = true
		trimmed := strings.TrimSuffix(strings.TrimPrefix(last, "go-"), "-go")
		names[trimmed] = true
		names[strings.ReplaceAll(trimmed, "-", "")] = true
		if i := strings.LastIndexAny(trimmed, "-."); i >= 0 {
			names[trimmed[i+1:]] = true // go-tree-sitter → sitter
		}
	}
	if c.goImportNames == nil {
		c.goImportNames = map[string]map[string]bool{}
	}
	if c.goImportNames[file] == nil {
		c.goImportNames[file] = map[string]bool{}
	}
	for n := range names {
		if n != "" {
			c.goImportNames[file][n] = true
		}
	}
}

// resolveGoValueMethods wires the Go selector calls the earlier passes left
// unresolved (methods: the ".Name" raw calls) — see the file comment.
func (c *Connector) resolveGoValueMethods(g *graph.Graph, callerID, callerFile string, calls, methods []string, funcsByName map[string][]string) {
	// Methods called as pkg.Method are package functions, not a value's.
	pkgQualified := map[string]bool{}
	for _, raw := range calls {
		if rest, ok := strings.CutPrefix(raw, "@"); ok {
			if dot := strings.LastIndexByte(rest, '.'); dot > 0 && c.goImportNames[callerFile][rest[:dot]] {
				pkgQualified[rest[dot+1:]] = true
			}
		}
	}
	wired := map[string]bool{}
	for _, e := range g.EdgesOf(callerID) {
		if e.Type == graph.EdgeCalls && e.From == callerID {
			if tn := g.Nodes[e.To]; tn != nil {
				wired[tn.Name] = true
			}
		}
	}
	callerDir := path.Dir(callerFile)
	dirs := map[string]bool{callerDir: true}
	for _, imp := range c.goPkgImports[callerFile] {
		dirs[strings.TrimPrefix(imp.pkgID, "pkg:")] = true
	}
	callerTest := strings.HasSuffix(callerFile, "_test.go")
	for _, m := range methods {
		if m == "" || wired[m] || pkgQualified[m] || goGenericMethods[m] {
			continue
		}
		var cands []string
		for _, id := range funcsByName[m] {
			tn := g.Nodes[id]
			if tn == nil || tn.Attrs["receiver_type"] == "" {
				continue
			}
			tf := tn.Attrs["file"]
			td := path.Dir(tf)
			if !strings.HasSuffix(tf, ".go") || !dirs[td] {
				continue
			}
			if strings.HasSuffix(tf, "_test.go") && !(callerTest && td == callerDir) {
				continue
			}
			cands = append(cands, id)
		}
		if len(cands) == 1 && cands[0] != callerID {
			g.AddEdge(callerID, cands[0], graph.EdgeCalls, map[string]string{"inferred": "true", "via": "go-method"})
			wired[m] = true
		}
	}
}
