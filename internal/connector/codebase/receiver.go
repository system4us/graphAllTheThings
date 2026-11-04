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
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"graphallthethings/internal/graph"
)

var (
	pyImportRe = regexp.MustCompile(`(?m)^[ \t]*import[ \t]+([^\n#]+)`)
	pyFromRe   = regexp.MustCompile(`(?m)^[ \t]*from[ \t]+([\w.]+)[ \t]+import[ \t]+(\([^)]*\)|[^\n#]+)`)
)

// pythonImportBindings returns, for each name the import statements of a Python
// file bind, the dotted module paths it may refer to, most specific first:
// `import a.b` binds a -> [a], `import a.b as c` binds c -> [a.b], and
// `from m import x as z` binds z -> [m.x, m] (x is a submodule, or an object
// defined in m). The path is what lets a receiver be tied to the SAME import:
// `html.escape()` after `import html` (the stdlib) must not be linked to some
// other file named html.py that a different statement imported.
func pythonImportBindings(src []byte) map[string][]string {
	out := map[string][]string{}
	add := func(prefix, item string) {
		item = strings.TrimSpace(strings.Trim(strings.TrimSpace(item), "()\\"))
		if item == "" || item == "*" {
			return
		}
		name, alias, hasAlias := strings.Cut(item, " as ")
		name = strings.TrimSpace(name)
		if prefix == "" { // import a.b[.c] [as d]
			if hasAlias {
				out[strings.TrimSpace(alias)] = []string{name}
			} else {
				first := strings.SplitN(name, ".", 2)[0]
				out[first] = []string{first}
			}
			return
		}
		bind := name
		if hasAlias {
			bind = strings.TrimSpace(alias)
		}
		sep := "."
		if strings.HasSuffix(prefix, ".") { // from . import x, from .. import x
			sep = ""
		}
		out[bind] = []string{prefix + sep + name, prefix}
	}
	for _, m := range pyImportRe.FindAllSubmatch(src, -1) {
		for _, it := range strings.Split(string(m[1]), ",") {
			add("", it)
		}
	}
	for _, m := range pyFromRe.FindAllSubmatch(src, -1) {
		prefix := string(m[1])
		for _, it := range strings.Split(strings.NewReplacer("\n", " ", "(", " ", ")", " ").Replace(string(m[2])), ",") {
			add(prefix, it)
		}
	}
	return out
}

// pyModuleFile resolves a Python module path ("a.b", ".c", "..d.e") to the
// relative path of its file (a.py or a/__init__.py) under the extraction root,
// or "". Unlike resolvePythonImport it creates no graph nodes.
func (c *Connector) pyModuleFile(fromAbs, spec string) string {
	try := func(base string, segs []string) string {
		target := filepath.Join(append([]string{base}, segs...)...)
		for _, cand := range []string{target + ".py", filepath.Join(target, "__init__.py")} {
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				if rel, err := filepath.Rel(c.dir, cand); err == nil && !strings.HasPrefix(rel, "..") {
					return filepath.ToSlash(rel)
				}
			}
		}
		return ""
	}
	if strings.HasPrefix(spec, ".") {
		dots := 0
		for dots < len(spec) && spec[dots] == '.' {
			dots++
		}
		base := filepath.Dir(fromAbs)
		for i := 1; i < dots; i++ {
			base = filepath.Dir(base)
		}
		var segs []string
		if rest := spec[dots:]; rest != "" {
			segs = strings.Split(rest, ".")
		}
		return try(base, segs)
	}
	for _, root := range c.pyRoots() {
		if f := try(root, strings.Split(spec, ".")); f != "" {
			return f
		}
	}
	return ""
}

var (
	csNamespaceRe = regexp.MustCompile(`(?m)^\s*namespace\s+([\w.]+)\s*[;{]`)
	csUsingRe     = regexp.MustCompile(`(?m)^\s*(?:global\s+)?using\s+([\w.]+)\s*;`)
)

// csNS is what C# visibility is made of: the namespaces a file declares and the
// namespaces it imports with `using`. (`using static`, aliases and `using var`
// do not match the pattern.)
type csNS struct{ declared, uses []string }

func parseCSharpNamespaces(src []byte) csNS {
	var out csNS
	for _, m := range csNamespaceRe.FindAllSubmatch(src, -1) {
		out.declared = append(out.declared, string(m[1]))
	}
	for _, m := range csUsingRe.FindAllSubmatch(src, -1) {
		out.uses = append(out.uses, string(m[1]))
	}
	return out
}

var csExtensionRe = regexp.MustCompile(`^\w+\s*(?:<[^>(]*>)?\(\s*this\s+([\w.]+)`)

// csharpExtensionReceiver returns T for a C# extension method signature
// "M(this T x, ...)" (T without namespace or generic arguments), else "".
func csharpExtensionReceiver(signature string) string {
	m := csExtensionRe.FindStringSubmatch(signature)
	if m == nil {
		return ""
	}
	t := m[1]
	if i := strings.LastIndexByte(t, '.'); i >= 0 {
		t = t[i+1:]
	}
	return t
}

// maxReceiverTargets bounds how many same-named functions in the resolved file
// get an edge (overloads). More than this means the method name is too
// ambiguous inside that file to be informative.
const maxReceiverTargets = 6

// receiverToken is the raw-call token for a selector call whose receiver is a
// single plain identifier, or `this.field` / `self.field` (which can only name
// that field): "@Recv.method". Anything else (a longer chain, a call result, an
// index expression) cannot be resolved from names alone.
func receiverToken(obj, name *sitter.Node, data []byte) string {
	recv := ""
	switch obj.Type() {
	case "identifier", "type_identifier", "simple_identifier", "name":
		recv = obj.Content(data)
	case "field_access", "member_expression", "member_access_expression", "navigation_expression":
		recv = thisFieldName(obj, data)
	}
	method := name.Content(data)
	if recv == "" || method == "" || strings.ContainsAny(recv, ". \t\n") {
		return ""
	}
	return "@" + recv + "." + method
}

// thisFieldName returns "f" for the expression this.f / self.f, else "".
func thisFieldName(n *sitter.Node, data []byte) string {
	isSelf := func(c *sitter.Node) bool {
		switch c.Type() {
		case "this", "this_expression", "self_expression", "self":
			return true
		}
		return false
	}
	var object, field *sitter.Node
	switch n.Type() {
	case "field_access": // Java
		object, field = n.ChildByFieldName("object"), n.ChildByFieldName("field")
	case "member_expression": // TS / JS
		object, field = n.ChildByFieldName("object"), n.ChildByFieldName("property")
	case "member_access_expression": // C#
		object, field = n.ChildByFieldName("expression"), n.ChildByFieldName("name")
	case "navigation_expression": // Kotlin / Swift: <target> <navigation_suffix>
		if n.NamedChildCount() == 2 {
			object = n.NamedChild(0)
			if suf := n.NamedChild(1); suf != nil && suf.Type() == "navigation_suffix" {
				field = suf.NamedChild(0)
				if suf.NamedChildCount() == 0 {
					field = nil
				}
			}
		}
	}
	if object == nil || field == nil || !isSelf(object) {
		return ""
	}
	switch field.Type() {
	case "identifier", "property_identifier", "simple_identifier", "field_identifier":
		return field.Content(data)
	}
	return ""
}

// defRef is a type definition and the file that holds it.
type defRef struct{ id, file string }

// recvIndex is the lookup data for receiver resolution, built once per
// resolveAndWire from the finished graph.
type recvIndex struct {
	funcsInFile   map[string]map[string][]string // rel file -> function name -> ids
	funcsInDir    map[string]map[string][]string // rel dir -> name -> ids (package-level, not methods)
	defs          map[string][]defRef            // type name -> definitions
	methodsOf     map[string]map[string][]string // definition id -> method name -> function ids (Go: HAS_METHOD)
	extByType     map[string]map[string][]string // C# extension methods: receiver type -> method name -> function ids
	funcFile      map[string]string              // function id -> file (C# extension methods only)
	csNS          map[string]csNS                // C# file -> declared / imported namespaces
	pkgName       map[string]string              // package node id -> name
	importedPkgs  map[string][]string            // caller rel file -> imported local package node ids
	importedFiles map[string][]string            // caller rel file -> imported local rel files
}

func buildRecvIndex(g *graph.Graph) *recvIndex {
	ri := &recvIndex{
		funcsInFile:   map[string]map[string][]string{},
		funcsInDir:    map[string]map[string][]string{},
		defs:          map[string][]defRef{},
		methodsOf:     map[string]map[string][]string{},
		extByType:     map[string]map[string][]string{},
		funcFile:      map[string]string{},
		csNS:          map[string]csNS{},
		pkgName:       map[string]string{},
		importedPkgs:  map[string][]string{},
		importedFiles: map[string][]string{},
	}
	isMethod := map[string]bool{}
	for _, e := range g.Edges {
		if e.Type == graph.EdgeHasMethod {
			isMethod[e.To] = true
			if fn := g.Nodes[e.To]; fn != nil {
				m := ri.methodsOf[e.From]
				if m == nil {
					m = map[string][]string{}
					ri.methodsOf[e.From] = m
				}
				m[fn.Name] = append(m[fn.Name], e.To)
			}
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
			if strings.HasSuffix(f, ".cs") {
				if t := csharpExtensionReceiver(n.Attrs["signature"]); t != "" {
					em := ri.extByType[t]
					if em == nil {
						em = map[string][]string{}
						ri.extByType[t] = em
					}
					em[n.Name] = append(em[n.Name], id)
					ri.funcFile[id] = f
				}
			}
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
				ri.defs[n.Name] = append(ri.defs[n.Name], defRef{id: id, file: f})
			}
		case graph.NodeFile:
			if d, u := n.Attrs["cs_declared"], n.Attrs["cs_uses"]; d != "" || u != "" {
				ri.csNS[n.Name] = csNS{declared: strings.Fields(d), uses: strings.Fields(u)}
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
// namedImports is the caller file's JS/TS import map (binding -> "file:<abs>"),
// pyMod the file each of its Python import bindings resolves to, types its declared variable types.
func (ri *recvIndex) resolve(root, callerFile, recv, method string, namedImports map[string]string, pyMod map[string]string, types map[string]string) []string {
	// Go: recv is the name of an imported local package -> a package-level function.
	// Only Go qualifies a call with a package name (Java/C#/Kotlin cannot: there a
	// directory named like a variable would capture it), and a variable declared in
	// this file with the same name shadows the package.
	if strings.HasSuffix(callerFile, ".go") {
		if _, shadowed := types[recv]; !shadowed {
			if ids := ri.packageFunc(callerFile, recv, method); len(ids) > 0 {
				return ids
			}
		}
	}

	// recv is itself a type (static call), a JS/TS import binding or a Python module.
	defs := ri.visibleDefs(callerFile, recv, pyMod)
	files := map[string]bool{}
	for _, d := range defs {
		files[d.file] = true
	}
	if target, ok := namedImports[recv]; ok { // `import { Foo } from './foo'` binds Foo to that exact file
		if rel, ok := relFromFileID(root, target); ok {
			files[rel] = true
		}
	}
	if f := pyMod[recv]; f != "" && strings.HasSuffix(callerFile, ".py") { // `import util` then util.func()
		files[f] = true
	}
	if len(files) == 1 {
		return ri.methodsIn(defs, files, method)
	}
	if len(files) > 1 {
		return nil // ambiguous between files
	}

	// recv is a variable whose declared type is in this file: svc.find() with
	// `UserService svc`. "" marks a name declared with conflicting types.
	typ := types[recv]
	if typ == "" {
		return nil
	}
	if pkg, name, ok := strings.Cut(typ, "."); ok { // Go: util.Repo
		return ri.qualifiedTypeMethods(callerFile, pkg, name, method)
	}
	tdefs := ri.visibleDefs(callerFile, typ, pyMod)
	tfiles := map[string]bool{}
	for _, d := range tdefs {
		tfiles[d.file] = true
	}
	if len(tfiles) != 1 {
		return nil
	}
	if ids := ri.methodsIn(tdefs, tfiles, method); len(ids) > 0 {
		return ids // an instance method always wins over an extension
	}
	// C#: the type has no such method, so `x.M()` binds to an extension method
	// M(this T x) if there is one. Only for types defined in this repo: an
	// extension on a library type could be shadowed by the library's own method.
	if strings.HasSuffix(callerFile, ".cs") {
		var ids []string
		for _, id := range ri.extByType[typ][method] {
			if f := ri.funcFile[id]; f == callerFile || ri.csVisible(callerFile, f) {
				ids = append(ids, id) // the extension's namespace must be in scope
			}
		}
		if len(ids) > 0 && len(ids) <= maxReceiverTargets {
			return ids
		}
	}
	return nil
}

// packageFunc: recv names a local package the caller imports (Go).
func (ri *recvIndex) packageFunc(callerFile, recv, method string) []string {
	for _, pid := range ri.importedPkgs[callerFile] {
		if ri.pkgName[pid] != recv {
			continue
		}
		if ids := ri.funcsInDir[strings.TrimPrefix(pid, "pkg:")][method]; len(ids) > 0 && len(ids) <= maxReceiverTargets {
			return ids
		}
	}
	return nil
}

// visibleDefs returns the definitions named name that the caller can see: in
// its own file, in its own directory (same package / namespace), or in a
// directory or file it imports.
func (ri *recvIndex) visibleDefs(callerFile, name string, pyMod map[string]string) []defRef {
	callerDir := path.Dir(callerFile)
	impDirs := map[string]bool{}
	for _, pid := range ri.importedPkgs[callerFile] {
		impDirs[strings.TrimPrefix(pid, "pkg:")] = true
	}
	impFiles := map[string]bool{}
	for _, f := range ri.importedFiles[callerFile] {
		impFiles[f] = true
	}
	var out []defRef
	for _, d := range ri.defs[name] {
		if strings.HasSuffix(callerFile, ".cs") && strings.HasSuffix(d.file, ".cs") {
			// C# scoping: same file, or the type's namespace is imported with `using`,
			// is the caller's namespace, or encloses it. Directories mean nothing here.
			if d.file == callerFile || ri.csVisible(callerFile, d.file) {
				out = append(out, d)
			}
			continue
		}
		if strings.HasSuffix(callerFile, ".py") && d.file != callerFile {
			if _, bound := pyMod[name]; !bound {
				continue // Python: another file's class is only visible through an import binding
			}
		}
		if d.file == callerFile || path.Dir(d.file) == callerDir || impDirs[path.Dir(d.file)] || impFiles[d.file] {
			out = append(out, d)
		}
	}
	return out
}

// csVisible reports whether a type declared in targetFile is in scope in callerFile.
func (ri *recvIndex) csVisible(callerFile, targetFile string) bool {
	caller, target := ri.csNS[callerFile], ri.csNS[targetFile]
	for _, t := range target.declared {
		for _, u := range caller.uses {
			if u == t {
				return true
			}
		}
		for _, c := range caller.declared {
			if c == t || strings.HasPrefix(c, t+".") {
				return true
			}
		}
	}
	return false
}

// methodsIn finds functions called method that belong to the given definitions.
// Where methods are linked to their type (Go: HAS_METHOD) only those count;
// elsewhere every function of that name in the defining file does (overloads).
func (ri *recvIndex) methodsIn(defs []defRef, files map[string]bool, method string) []string {
	var out []string
	linked := false
	for _, d := range defs {
		if len(ri.methodsOf[d.id]) > 0 {
			linked = true
			out = append(out, ri.methodsOf[d.id][method]...)
		}
	}
	if !linked {
		for f := range files {
			out = append(out, ri.funcsInFile[f][method]...)
		}
	}
	if len(out) == 0 || len(out) > maxReceiverTargets {
		return nil
	}
	return out
}

// qualifiedTypeMethods: a Go variable declared as pkg.Type, where pkg is an
// imported local package.
func (ri *recvIndex) qualifiedTypeMethods(callerFile, pkg, name, method string) []string {
	for _, pid := range ri.importedPkgs[callerFile] {
		if ri.pkgName[pid] != pkg {
			continue
		}
		dir := strings.TrimPrefix(pid, "pkg:")
		var defs []defRef
		files := map[string]bool{}
		for _, d := range ri.defs[name] {
			if path.Dir(d.file) == dir {
				defs = append(defs, d)
				files[d.file] = true
			}
		}
		if len(defs) == 1 {
			return ri.methodsIn(defs, files, method)
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
	for _, target := range c.recv.resolve(c.dir, callerFile, recv, method, c.namedImports[callerFile], c.pyBindings[callerFile], c.fileTypes[callerFile]) {
		if target != callerID {
			g.AddEdge(callerID, target, graph.EdgeCalls, map[string]string{"inferred": "true", "via": "receiver", "receiver": recv})
		}
	}
}
