// Package enrich links an API graph to the source code that implements it.
// For a swaggo-generated spec the link is derivable cheaply and reliably: the
// `@Router <path> [<method>]` annotation sits in the doc comment right above
// the handler function, and each schema is a Go struct. This pass parses the
// repo's Go files (in parallel) and stamps a `source: file:line` attribute on
// the matching endpoint and schema nodes, so an agent can jump from the HTTP
// contract straight to the code — no exploratory grep.
//
// The `source` attribute is deliberately kept out of graph.NodeText, so a
// handler moving (line changes, no contract change) never forces re-embedding.
package enrich

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"graphallthethings/internal/graph"
)

// Result reports how many nodes the pass linked.
type Result struct {
	Endpoints int // endpoint nodes stamped with a source location
	Schemas   int // schema nodes stamped with a source location
}

var routerRe = regexp.MustCompile(`@Router\s+(\S+)\s+\[(\w+)\]`)

// endpointHit is a handler annotation; schemaHit is a struct declaration.
type endpointHit struct{ method, path, loc string }
type schemaHit struct{ name, loc, key string } // key: swaggo-style underscored import path + ".Type"

// Code parses the Go sources under root and stamps `source: file:line` onto the
// endpoint and schema nodes it can match. Endpoints match by (method, path) —
// exact and reliable; schemas match by type name, disambiguated by package for
// the rare colliding names.
func Code(g *graph.Graph, root string) (Result, error) {
	info, err := os.Stat(root)
	if err != nil {
		return Result{}, fmt.Errorf("code root: %w", err)
	}
	if !info.IsDir() {
		return Result{}, fmt.Errorf("code root %q is not a directory", root)
	}
	modRoot, modPath := moduleInfo(root)

	var files []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			// Skip hidden dirs (.git, and crucially .claude/worktrees which holds
			// duplicate checkouts that would make every type name collide),
			// vendored/generated code, and test fixtures.
			if name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			switch name {
			case "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})

	// Parse files in a bounded worker pool — CPU-bound and embarrassingly
	// parallel, one of the safe places to spend goroutines.
	var mu sync.Mutex
	var endpoints []endpointHit
	var schemas []schemaHit
	sem := make(chan struct{}, max(runtime.NumCPU(), 1))
	var wg sync.WaitGroup
	for _, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(f string) {
			defer wg.Done()
			defer func() { <-sem }()
			eps, sch := parseFile(f, root, modRoot, modPath)
			if len(eps)+len(sch) == 0 {
				return
			}
			mu.Lock()
			endpoints = append(endpoints, eps...)
			schemas = append(schemas, sch...)
			mu.Unlock()
		}(f)
	}
	wg.Wait()

	var res Result
	for _, e := range endpoints {
		if n := g.Nodes["endpoint:"+e.method+" "+e.path]; n != nil {
			setSource(n, e.loc)
			res.Endpoints++
		}
	}

	byName := map[string][]schemaHit{}
	for _, s := range schemas {
		byName[s.name] = append(byName[s.name], s)
	}
	for _, n := range g.NodesByType(graph.NodeSchema) {
		cands := byName[lastSegment(n.Name)]
		var pick *schemaHit
		switch {
		case len(cands) == 1:
			pick = &cands[0]
		case len(cands) > 1:
			// Colliding type name: the schema node kept its full swaggo key
			// (pkg-qualified). Match by package — swaggo often drops a leading
			// stretch of the import path, so accept a suffix match too.
			for i := range cands {
				if k := cands[i].key; k != "" && (k == n.Name || strings.HasSuffix(k, "_"+n.Name)) {
					pick = &cands[i]
					break
				}
			}
		}
		if pick != nil {
			setSource(n, pick.loc)
			res.Schemas++
		}
	}
	return res, nil
}

func setSource(n *graph.Node, loc string) {
	if n.Attrs == nil {
		n.Attrs = map[string]string{}
	}
	n.Attrs["source"] = loc
}

func parseFile(file, root, modRoot, modPath string) ([]endpointHit, []schemaHit) {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		return nil, nil // unparseable file: skip, don't fail the whole pass
	}
	rel := relSlash(root, file)
	pkgKey := underscoreImport(modRoot, modPath, filepath.Dir(file))

	var eps []endpointHit
	var sch []schemaHit
	for _, decl := range af.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Doc == nil {
				continue
			}
			for _, c := range d.Doc.List {
				if m := routerRe.FindStringSubmatch(c.Text); m != nil {
					loc := fmt.Sprintf("%s:%d", rel, fset.Position(d.Pos()).Line)
					eps = append(eps, endpointHit{method: strings.ToUpper(m[2]), path: m[1], loc: loc})
				}
			}
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				// Any named type can back a schema: structs for object models,
				// `type X string` for enums.
				loc := fmt.Sprintf("%s:%d", rel, fset.Position(ts.Pos()).Line)
				key := ""
				if pkgKey != "" {
					key = pkgKey + "." + ts.Name.Name
				}
				sch = append(sch, schemaHit{name: ts.Name.Name, loc: loc, key: key})
			}
		}
	}
	return eps, sch
}

// moduleInfo finds the enclosing Go module: its root directory and module path.
// Returns empty strings when no go.mod is found (schema disambiguation by
// package is then skipped; endpoints still match).
func moduleInfo(start string) (root, modPath string) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", ""
	}
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			for line := range strings.SplitSeq(string(data), "\n") {
				if s := strings.TrimSpace(line); strings.HasPrefix(s, "module ") {
					return dir, strings.TrimSpace(strings.TrimPrefix(s, "module "))
				}
			}
			return dir, ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

// underscoreImport builds the swaggo-style schema key prefix for a package
// directory: its import path with '/', '.', '-' replaced by '_'.
func underscoreImport(modRoot, modPath, dir string) string {
	if modRoot == "" || modPath == "" {
		return ""
	}
	rel, err := filepath.Rel(modRoot, dir)
	if err != nil {
		return ""
	}
	imp := modPath
	if rel != "." {
		imp = path.Join(modPath, filepath.ToSlash(rel))
	}
	return strings.NewReplacer("/", "_", ".", "_", "-", "_").Replace(imp)
}

func relSlash(root, file string) string {
	if rel, err := filepath.Rel(root, file); err == nil {
		return filepath.ToSlash(rel)
	}
	return file
}

func lastSegment(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}
