package codebase

// Cross-file resolution of calls that have a receiver: Helper.hello() (Java,
// C#, Kotlin), util.Helper() (a Go package), Foo.bar() with Foo imported (TS/JS),
// mod.func() (Python), svc.find() where svc is declared as a UserService.
//
// resolveCall only matched a selector call to a function in the SAME file, so
// every method call across files was missing from the call graph: `impact` on
// aspnetcore's UseRouting reported "no callers" while 166 files mention it.
//
// The rule is "only what can be demonstrated". A receiver name is turned into a
// set of candidate files using evidence that is in the graph already — the
// caller's own imports, its own directory (same package/namespace) and its own
// file — and an edge is added only when that set is a single file. Nothing is
// guessed from a bare method name: a repo-wide unique `add()` must not capture
// every `list.add()`.

import (
	"path"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"graphallthethings/internal/graph"
)

var (
	pyImportRe = regexp.MustCompile(`(?m)^[ \t]*import[ \t]+([^\n#]+)`)
	pyFromRe   = regexp.MustCompile(`(?m)^[ \t]*from[ \t]+[\w.]+[ \t]+import[ \t]+(\([^)]*\)|[^\n#]+)`)
)

// pythonImportBindings returns the names that import statements bind in a
// Python file: `import a.b` binds a, `import a.b as c` binds c, `from m import
// x, y as z` binds x and z. A receiver is only trusted to be a module/class when
// an import actually binds its name; otherwise `graph.add_node()` could be a local
// variable that merely shares a name with an imported module.
func pythonImportBindings(src []byte) map[string]bool {
	out := map[string]bool{}
	add := func(item string) {
		item = strings.TrimSpace(strings.Trim(strings.TrimSpace(item), "()\\"))
		if item == "" || item == "*" {
			return
		}
		if _, alias, ok := strings.Cut(item, " as "); ok {
			out[strings.TrimSpace(alias)] = true
			return
		}
		out[strings.SplitN(item, ".", 2)[0]] = true
	}
	for _, m := range pyImportRe.FindAllSubmatch(src, -1) {
		for _, it := range strings.Split(string(m[1]), ",") {
			add(it)
		}
	}
	for _, m := range pyFromRe.FindAllSubmatch(src, -1) {
		for _, it := range strings.Split(strings.NewReplacer("\n", " ", "(", " ", ")", " ").Replace(string(m[1])), ",") {
			add(it)
		}
	}
	return out
}

// maxReceiverTargets bounds how many same-named functions in the resolved file
// get an edge (overloads). More than this means the method name is too
// ambiguous inside that file to be informative.
const maxReceiverTargets = 6

// receiverToken is the raw-call token for a selector call whose receiver is a
// single plain identifier: "@Recv.method". Anything else (a chain, a call
// result, an index expression) cannot be resolved from names alone.
func receiverToken(obj, name *sitter.Node, data []byte) string {
	switch obj.Type() {
	case "identifier", "type_identifier", "simple_identifier", "name":
	default:
		return ""
	}
	recv, method := obj.Content(data), name.Content(data)
	if recv == "" || method == "" || strings.ContainsAny(recv, ". \t\n") {
		return ""
	}
	return "@" + recv + "." + method
}

// recvIndex is the lookup data for receiver resolution, built once per
// resolveAndWire from the finished graph.
type recvIndex struct {
	funcsInFile   map[string]map[string][]string // rel file -> function name -> ids
	funcsInDir    map[string]map[string][]string // rel dir -> name -> ids (package-level, not methods)
	defFiles      map[string][]string            // type name -> rel files that define it
	pkgName       map[string]string              // package node id -> name
	importedPkgs  map[string][]string            // caller rel file -> imported local package node ids
	importedFiles map[string][]string            // caller rel file -> imported local rel files
}

func buildRecvIndex(g *graph.Graph) *recvIndex {
	ri := &recvIndex{
		funcsInFile:   map[string]map[string][]string{},
		funcsInDir:    map[string]map[string][]string{},
		defFiles:      map[string][]string{},
		pkgName:       map[string]string{},
		importedPkgs:  map[string][]string{},
		importedFiles: map[string][]string{},
	}
	isMethod := map[string]bool{}
	for _, e := range g.Edges {
		if e.Type == graph.EdgeHasMethod {
			isMethod[e.To] = true
		}
	}
	for id, n := range g.Nodes {
		switch n.Type {
		case graph.NodeFunction:
			if n.Attrs["external"] == "true" {
				continue
			}
			f := n.Attrs["file"]
			if f == "" {
				continue
			}
			m := ri.funcsInFile[f]
			if m == nil {
				m = map[string][]string{}
				ri.funcsInFile[f] = m
			}
			m[n.Name] = append(m[n.Name], id)
			if !isMethod[id] {
				d := path.Dir(f)
				dm := ri.funcsInDir[d]
				if dm == nil {
					dm = map[string][]string{}
					ri.funcsInDir[d] = dm
				}
				dm[n.Name] = append(dm[n.Name], id)
			}
		case graph.NodeDefinition:
			if f := n.Attrs["file"]; f != "" {
				ri.defFiles[n.Name] = append(ri.defFiles[n.Name], f)
			}
		case graph.NodePackage:
			ri.pkgName[id] = n.Name
		}
	}
	for _, e := range g.Edges {
		if e.Type != graph.EdgeImports {
			continue
		}
		from, to := g.Nodes[e.From], g.Nodes[e.To]
		if from == nil || to == nil || from.Type != graph.NodeFile || to.Attrs["external"] == "true" {
			continue
		}
		switch to.Type {
		case graph.NodePackage:
			ri.importedPkgs[from.Name] = append(ri.importedPkgs[from.Name], e.To)
		case graph.NodeFile:
			ri.importedFiles[from.Name] = append(ri.importedFiles[from.Name], to.Name)
		}
	}
	return ri
}

// resolve returns the function ids that recv.method provably refers to, or nil.
// namedImports is the caller file's JS/TS import map (binding -> "file:<abs>").
func (ri *recvIndex) resolve(root, callerFile, recv, method string, namedImports map[string]string, pyBound map[string]bool) []string {
	// Go: recv is the name of an imported local package -> a package-level function.
	for _, pid := range ri.importedPkgs[callerFile] {
		if ri.pkgName[pid] != recv {
			continue
		}
		dir := strings.TrimPrefix(pid, "pkg:")
		if ids := ri.funcsInDir[dir][method]; len(ids) > 0 && len(ids) <= maxReceiverTargets {
			return ids
		}
	}

	cand := map[string]bool{}

	// Type name visible from the caller: same file, same directory (package /
	// namespace), or a directory / file the caller imports.
	callerDir := path.Dir(callerFile)
	impDirs := map[string]bool{}
	for _, pid := range ri.importedPkgs[callerFile] {
		impDirs[strings.TrimPrefix(pid, "pkg:")] = true
	}
	impFiles := map[string]bool{}
	for _, f := range ri.importedFiles[callerFile] {
		impFiles[f] = true
	}
	for _, f := range ri.defFiles[recv] {
		if strings.HasSuffix(callerFile, ".py") && f != callerFile && !pyBound[recv] {
			continue // Python: another file's class is only visible through an import binding
		}
		if f == callerFile || path.Dir(f) == callerDir || impDirs[path.Dir(f)] || impFiles[f] {
			cand[f] = true
		}
	}

	// JS/TS: `import { Foo } from './foo'` binds Foo to that exact file.
	if target, ok := namedImports[recv]; ok {
		if rel, ok := relFromFileID(root, target); ok {
			cand[rel] = true
		}
	}

	// Python: `import util` / `from pkg import util` then util.func().
	if strings.HasSuffix(callerFile, ".py") && pyBound[recv] {
		for _, f := range ri.importedFiles[callerFile] {
			base := strings.TrimSuffix(path.Base(f), ".py")
			if base == "__init__" {
				base = path.Base(path.Dir(f))
			}
			if base == recv {
				cand[f] = true
			}
		}
	}

	if len(cand) != 1 {
		return nil // nothing demonstrable, or ambiguous between files
	}
	for f := range cand {
		if ids := ri.funcsInFile[f][method]; len(ids) > 0 && len(ids) <= maxReceiverTargets {
			return ids
		}
	}
	return nil
}

// relFromFileID turns a "file:<abs path>" node id into a path relative to root.
func relFromFileID(root, id string) (string, bool) {
	abs, ok := strings.CutPrefix(id, "file:")
	if !ok {
		return "", false
	}
	rel, ok := strings.CutPrefix(abs, strings.TrimRight(root, "/")+"/")
	if !ok {
		return "", false
	}
	return rel, true
}

// resolveReceiverCall wires CALLS edges for one "@Recv.method" token.
func (c *Connector) resolveReceiverCall(g *graph.Graph, callerID, callerFile, recv, method string) {
	if c.recv == nil {
		return
	}
	for _, target := range c.recv.resolve(c.dir, callerFile, recv, method, c.namedImports[callerFile], c.pyBindings[callerFile]) {
		if target != callerID {
			g.AddEdge(callerID, target, graph.EdgeCalls, map[string]string{"inferred": "true", "via": "receiver", "receiver": recv})
		}
	}
}
