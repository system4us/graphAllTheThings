package codebase

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"graphallthethings/internal/graph"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

type Connector struct {
	dir string
	// only restricts parseFiles to this set of relative paths. nil = all.
	// Set internally by Update for incremental re-parsing.
	only map[string]bool
}

func New(dir string) *Connector {
	return &Connector{dir: dir}
}

// parseableExts are the file extensions the extractor understands.
var parseableExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".py": true, ".rs": true, ".md": true,
}

func (c *Connector) Name() string { return "codebase" }

// funcInfo holds all information extracted about a function/method before
// it is committed to the graph. Building in two passes lets us resolve
// call targets to local definitions after the whole codebase is scanned.
type funcInfo struct {
	id          string
	name        string
	file        string  // relative path
	lineStart   int
	lineEnd     int
	signature   string
	receiverDef string  // definition node id this is a method of (Go), or ""
	calls       []string // raw called names (resolved in second pass)
}

func (c *Connector) Extract(ctx context.Context) (*graph.Graph, error) {
	g := graph.New(fmt.Sprintf("codebase:%s", c.dir))
	g.ExtractedAt = time.Now().UTC()

	c.loadSemanticOverlay(g)
	c.detectProjects(g)

	funcs, funcsByName := c.parseFiles(ctx, g)
	resolveAndWire(g, funcs, funcsByName)

	return g, nil
}

// scanFiles walks the tree with the same skip rules as parseFiles and returns
// relPath → mtime (UnixNano) for every parseable file. Cheap: stat only.
func (c *Connector) scanFiles() map[string]string {
	out := map[string]string{}
	filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			isHidden := strings.HasPrefix(name, ".") && path != c.dir
			if name == ".git" || name == "node_modules" || name == "vendor" || name == ".gatt" || isHidden {
				return filepath.SkipDir
			}
			return nil
		}
		if !parseableExts[filepath.Ext(d.Name())] {
			return nil
		}
		rel, _ := filepath.Rel(c.dir, path)
		if info, err := d.Info(); err == nil {
			out[rel] = fmt.Sprint(info.ModTime().UnixNano())
		}
		return nil
	})
	return out
}

// HasDrift cheaply reports whether any parseable file changed, appeared, or
// disappeared relative to the recorded mtimes (relPath → mtime as returned by
// graph.FileMtimes / graph.LoadCodebaseState). Stat-walk only; nothing parsed.
func (c *Connector) HasDrift(prevM map[string]string) bool {
	cur := c.scanFiles()
	if len(cur) != len(prevM) {
		return true
	}
	for rel, mt := range cur {
		if prevM[rel] != mt {
			return true
		}
	}
	return false
}

// Update incrementally refreshes prev against the current state of the tree:
// only changed/new files are re-parsed; entities of changed/deleted files are
// evicted first. Returns the updated graph and a summary — summary == "" means
// no drift (prev returned untouched).
//
// Limitation: calls from *unchanged* files to functions that are new in this
// update are wired only when the target existed before (by name+file relink);
// a full extract remains the ground truth.
func (c *Connector) Update(ctx context.Context, prev *graph.Graph) (*graph.Graph, string, error) {
	cur := c.scanFiles()
	prevM := graph.FileMtimes(prev) // relPath → mtime as recorded at extract time

	var changed, added, deleted []string
	for rel, mt := range cur {
		switch old, ok := prevM[rel]; {
		case !ok:
			added = append(added, rel)
		case old != mt:
			changed = append(changed, rel)
		}
	}
	for rel := range prevM {
		if _, ok := cur[rel]; !ok {
			deleted = append(deleted, rel)
		}
	}
	if len(changed)+len(added)+len(deleted) == 0 {
		return prev, "", nil
	}

	// Track mutations from here on: a SQLite-backed graph then saves only
	// the delta rows instead of rewriting everything.
	prev.StartJournal()

	dirty := map[string]bool{}
	for _, rel := range changed {
		dirty[rel] = true
	}
	for _, rel := range added {
		dirty[rel] = true
	}
	for _, rel := range deleted {
		dirty[rel] = true
	}

	// Remember CALLS edges from surviving callers into functions of dirty
	// files, so they can be re-attached to the re-parsed nodes (whose ids
	// change when line numbers shift).
	type relink struct{ from, name, file string }
	var relinks []relink
	for _, e := range prev.Edges {
		if e.Type != graph.EdgeCalls {
			continue
		}
		tn, fn := prev.Nodes[e.To], prev.Nodes[e.From]
		if tn != nil && fn != nil && dirty[tn.Attrs["file"]] && !dirty[fn.Attrs["file"]] {
			relinks = append(relinks, relink{e.From, tn.Name, tn.Attrs["file"]})
		}
	}

	// Evict everything owned by dirty files: functions/defs/docs (attrs.file)
	// and the file nodes themselves (Name == relPath).
	prev.RemoveNodesWhere(func(n *graph.Node) bool {
		if f := n.Attrs["file"]; f != "" && dirty[f] {
			return true
		}
		return n.Type == graph.NodeFile && dirty[n.Name]
	})

	// Re-parse only surviving dirty paths (deleted files stay gone).
	c.only = map[string]bool{}
	for rel := range dirty {
		if _, ok := cur[rel]; ok {
			c.only[rel] = true
		}
	}
	defer func() { c.only = nil }()

	newFuncs, _ := c.parseFiles(ctx, prev)

	// Global name index over the whole graph (survivors + new) so the new
	// functions resolve against everything, same as a full extract.
	funcsByName := map[string][]string{}
	for id, n := range prev.Nodes {
		if n.Type == graph.NodeFunction && n.Attrs["external"] != "true" && strings.HasPrefix(id, "func:") {
			funcsByName[n.Name] = append(funcsByName[n.Name], id)
		}
	}
	resolveAndWire(prev, newFuncs, funcsByName)

	// Re-attach surviving callers to the re-parsed targets.
	byFileName := map[string]string{}
	for _, fi := range newFuncs {
		key := fi.file + "\x00" + fi.name
		if _, ok := byFileName[key]; !ok {
			byFileName[key] = fi.id
		}
	}
	seenRelink := map[string]bool{}
	for _, r := range relinks {
		key := r.from + "\x00" + r.file + "\x00" + r.name
		if seenRelink[key] {
			continue
		}
		seenRelink[key] = true
		if target := byFileName[r.file+"\x00"+r.name]; target != "" {
			prev.AddEdge(r.from, target, graph.EdgeCalls, nil)
		}
	}

	prev.ExtractedAt = time.Now().UTC()
	summary := fmt.Sprintf("re-parsed %d changed + %d new file(s), evicted %d deleted",
		len(changed), len(added), len(deleted))
	return prev, summary, nil
}

// loadSemanticOverlay reads .gatt/definitions.json and .gatt/relations.json.
func (c *Connector) loadSemanticOverlay(g *graph.Graph) {
	gattDir := filepath.Join(c.dir, ".gatt")

	defPath := filepath.Join(gattDir, "definitions.json")
	if data, err := os.ReadFile(defPath); err == nil {
		var payload struct {
			Entities map[string]map[string]any `json:"entities"`
		}
		if json.Unmarshal(data, &payload) == nil {
			for name, attrs := range payload.Entities {
				nodeID := "sem_def:" + name
				strAttrs := map[string]string{}
				if desc, ok := attrs["description"].(string); ok {
					strAttrs["comment"] = desc
				}
				if cr, ok := attrs["critical_rules"]; ok {
					strAttrs["critical_rules"] = fmt.Sprint(cr)
				}
				g.AddNode(&graph.Node{
					ID:    nodeID,
					Type:  graph.NodeDefinition,
					Name:  name + " (Semantic)",
					Attrs: strAttrs,
				})
			}
		}
	}

	relPath := filepath.Join(gattDir, "relations.json")
	if data, err := os.ReadFile(relPath); err == nil {
		var payload struct {
			Features []map[string]any `json:"features"`
		}
		if json.Unmarshal(data, &payload) == nil {
			for _, feat := range payload.Features {
				name, _ := feat["name"].(string)
				if name == "" {
					continue
				}
				nodeID := "sem_feat:" + name
				strAttrs := map[string]string{}
				if ep, ok := feat["entry_point"].(string); ok {
					strAttrs["entry_point"] = ep
				}
				if cl, ok := feat["core_logic"].(string); ok {
					strAttrs["core_logic"] = cl
				}
				g.AddNode(&graph.Node{
					ID:    nodeID,
					Type:  graph.NodeFeature,
					Name:  name + " (Semantic)",
					Attrs: strAttrs,
				})
				if ep, ok := feat["entry_point"].(string); ok {
					physicalProjID := "proj:" + filepath.Join(c.dir, ep)
					g.AddEdge(physicalProjID, nodeID, graph.EdgeBelongsTo, nil)
				}
			}
		}
	}
}

// detectProjects walks the repo looking for project manifest files.
func (c *Connector) detectProjects(g *graph.Graph) {
	filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			isHidden := strings.HasPrefix(name, ".") && path != c.dir
			if name == ".git" || name == "node_modules" || name == "vendor" || name == ".gatt" || isHidden {
				return filepath.SkipDir
			}
			if _, e := os.Stat(filepath.Join(path, "go.mod")); e == nil {
				g.AddNode(&graph.Node{ID: "proj:" + path, Type: graph.NodeProject, Name: filepath.Base(path) + " (Go)"})
			} else if _, e := os.Stat(filepath.Join(path, "package.json")); e == nil {
				g.AddNode(&graph.Node{ID: "proj:" + path, Type: graph.NodeProject, Name: filepath.Base(path) + " (NPM)"})
			} else if _, e := os.Stat(filepath.Join(path, "pyproject.toml")); e == nil {
				g.AddNode(&graph.Node{ID: "proj:" + path, Type: graph.NodeProject, Name: filepath.Base(path) + " (Python)"})
			} else if _, e := os.Stat(filepath.Join(path, "Cargo.toml")); e == nil {
				g.AddNode(&graph.Node{ID: "proj:" + path, Type: graph.NodeProject, Name: filepath.Base(path) + " (Rust)"})
			}
		}
		return nil
	})
	if len(g.NodesByType(graph.NodeProject)) == 0 {
		g.AddNode(&graph.Node{ID: "proj:" + c.dir, Type: graph.NodeProject, Name: filepath.Base(c.dir)})
	}
}

type langConfig struct {
	lang     *sitter.Language
	// queryStr captures: def.name, func.name, call.func, import.path, method.receiver
	// Line ranges come from navigating cap.Node.Parent() to the declaration node.
	queryStr string
	query    *sitter.Query // compiled once in parseFiles, reused per file
}

func langFor(ext string) *langConfig {
	switch ext {
	case ".go":
		return &langConfig{
			lang: golang.GetLanguage(),
			// call.func = direct call (local resolution candidate)
			// call.sel  = selector field (pkg.Func or obj.Method — skip local resolution)
			queryStr: `
			(type_declaration (type_spec name: (type_identifier) @def.name))
			(function_declaration name: (identifier) @func.name)
			(method_declaration receiver: (parameter_list) @method.receiver name: (field_identifier) @func.name)
			(call_expression function: (identifier) @call.func)
			(call_expression function: (selector_expression field: (field_identifier) @call.sel))
			(import_spec path: (interpreted_string_literal) @import.path)
			`,
		}
	case ".ts", ".tsx":
		return &langConfig{
			lang: typescript.GetLanguage(),
			queryStr: `
			(class_declaration name: (type_identifier) @def.name)
			(function_declaration name: (identifier) @func.name)
			(method_definition name: (property_identifier) @func.name)
			(call_expression function: (identifier) @call.func)
			(call_expression function: (member_expression property: (property_identifier) @call.sel))
			(import_statement source: (string) @import.path)
			`,
		}
	case ".py":
		return &langConfig{
			lang: python.GetLanguage(),
			queryStr: `
			(class_definition name: (identifier) @def.name)
			(function_definition name: (identifier) @func.name)
			(call function: (identifier) @call.func)
			(call function: (attribute attribute: (identifier) @call.sel))
			(import_statement name: (dotted_name) @import.path)
			(import_from_statement module_name: (dotted_name) @import.path)
			`,
		}
	case ".rs":
		return &langConfig{
			lang: rust.GetLanguage(),
			queryStr: `
			(struct_item name: (type_identifier) @def.name)
			(function_item name: (identifier) @func.name)
			(call_expression function: (identifier) @call.func)
			(call_expression function: (field_expression field: (field_identifier) @call.sel))
			(use_declaration argument: (scoped_identifier) @import.path)
			`,
		}
	}
	return nil
}

// declarationRange returns the (lineStart, lineEnd) of the declaration that
// contains a captured name node, by walking up to the declaration node.
// Works for function_declaration, method_declaration, type_declaration, etc.
func declarationRange(nameNode *sitter.Node) (int, int) {
	n := nameNode.Parent()
	for n != nil {
		t := n.Type()
		switch t {
		case "function_declaration", "method_declaration",
			"function_definition", "fn_item",
			"method_definition", "method":
			return int(n.StartPoint().Row) + 1, int(n.EndPoint().Row) + 1
		}
		n = n.Parent()
	}
	// Fall back to the name node's own line.
	return int(nameNode.StartPoint().Row) + 1, int(nameNode.EndPoint().Row) + 1
}

// buildSignature constructs a human-readable signature for a function node
// by examining the parameters and result fields of the parent declaration.
func buildSignature(name string, nameNode *sitter.Node, src []byte) string {
	decl := nameNode.Parent()
	for decl != nil {
		t := decl.Type()
		if t == "function_declaration" || t == "method_declaration" ||
			t == "function_definition" || t == "fn_item" ||
			t == "method_definition" || t == "method" {
			break
		}
		decl = decl.Parent()
	}
	if decl == nil {
		return name
	}

	sig := name
	if params := decl.ChildByFieldName("parameters"); params != nil {
		sig += params.Content(src)
	}
	if result := decl.ChildByFieldName("result"); result != nil {
		rt := strings.TrimSpace(result.Content(src))
		if rt != "" {
			sig += " " + rt
		}
	}
	if retType := decl.ChildByFieldName("return_type"); retType != nil {
		sig += " " + retType.Content(src)
	}
	return sig
}

// docComment returns the comment block immediately preceding the declaration
// that contains nameNode (Go/TS/Rust style), or the leading docstring for
// Python function bodies. Trimmed to 300 chars — used for semantic embedding.
func docComment(nameNode *sitter.Node, src []byte) string {
	decl := nameNode.Parent()
	for decl != nil {
		switch decl.Type() {
		case "function_declaration", "method_declaration", "type_declaration",
			"function_definition", "fn_item", "method_definition", "method",
			"class_declaration", "class_definition", "struct_item":
		default:
			decl = decl.Parent()
			continue
		}
		break
	}
	if decl == nil {
		return ""
	}

	var lines []string

	// Python docstring: first statement of the body block.
	if body := decl.ChildByFieldName("body"); body != nil && body.Type() == "block" {
		if first := body.NamedChild(0); first != nil && first.Type() == "expression_statement" {
			if s := first.NamedChild(0); s != nil && s.Type() == "string" {
				lines = append(lines, strings.Trim(s.Content(src), "\"' \n"))
			}
		}
	}

	// Preceding // or /* */ comment block: each comment must be directly
	// adjacent (no blank line) to the node below it, ending at the decl.
	if len(lines) == 0 {
		below := decl
		for sib := decl.PrevNamedSibling(); sib != nil && strings.Contains(sib.Type(), "comment"); sib = sib.PrevNamedSibling() {
			if below.StartPoint().Row-sib.EndPoint().Row > 1 {
				break
			}
			text := sib.Content(src)
			text = strings.TrimPrefix(text, "//")
			text = strings.TrimPrefix(text, "/*")
			text = strings.TrimSuffix(text, "*/")
			lines = append([]string{strings.TrimSpace(text)}, lines...)
			below = sib
		}
	}

	doc := strings.TrimSpace(strings.Join(lines, " "))
	if len(doc) > 300 {
		doc = doc[:300]
	}
	return doc
}

// receiverTypeName extracts the type name from a Go receiver node like "(e *Engine)" → "Engine".
func receiverTypeName(recvNode *sitter.Node, src []byte) string {
	text := strings.Trim(recvNode.Content(src), "()")
	parts := strings.Fields(text)
	if len(parts) >= 2 {
		return strings.TrimPrefix(parts[len(parts)-1], "*")
	}
	return ""
}

// parseFiles walks the repo, creates file/def/func/doc nodes, returns
// the collected funcInfo slice and a map from function name to its node ids
// (for local call resolution).
func (c *Connector) parseFiles(ctx context.Context, g *graph.Graph) ([]funcInfo, map[string][]string) {
	var funcs []funcInfo
	funcsByName := map[string][]string{}

	// Pre-compile queries for all supported extensions once.
	// NewQuery is not safe to call in a tight loop inside WalkDir callbacks.
	compiledLangs := map[string]*langConfig{}
	for _, ext := range []string{".go", ".ts", ".tsx", ".py", ".rs"} {
		lc := langFor(ext)
		if lc == nil {
			continue
		}
		q, err := sitter.NewQuery([]byte(lc.queryStr), lc.lang)
		if err != nil {
			// Log and skip this language.
			continue
		}
		lc.query = q
		compiledLangs[ext] = lc
	}

	filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() {
				name := d.Name()
				// Skip hidden dirs, but NOT the root dir itself (which may be "." when
				// invoked as `gatt extract codebase .`).
				isHidden := strings.HasPrefix(name, ".") && path != c.dir
				if name == ".git" || name == "node_modules" || name == "vendor" || name == ".gatt" || isHidden {
					return filepath.SkipDir
				}
			}
			return nil
		}

		ext := filepath.Ext(d.Name())
		relPath, _ := filepath.Rel(c.dir, path)
		if c.only != nil && !c.only[relPath] {
			return nil
		}
		projID := findProject(g, path, c.dir)
		fileID := "file:" + path

		mtime := ""
		if info, err := d.Info(); err == nil {
			mtime = fmt.Sprint(info.ModTime().UnixNano())
		}

		// Markdown files → doc nodes (semantic search oriented).
		if ext == ".md" {
			c.parseMarkdown(g, path, relPath, fileID, projID, mtime)
			return nil
		}

		lc := compiledLangs[ext]
		if lc == nil {
			return nil
		}

		g.AddNode(&graph.Node{
			ID:   fileID,
			Type: graph.NodeFile,
			Name: relPath,
			Attrs: map[string]string{"path": path, "mtime": mtime},
		})
		if projID != "" {
			g.AddEdge(fileID, projID, graph.EdgeBelongsTo, nil)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		parser := sitter.NewParser()
		parser.SetLanguage(lc.lang)
		tree, err := parser.ParseCtx(ctx, nil, data)
		if err != nil || tree == nil {
			return nil
		}

		qc := sitter.NewQueryCursor()
		qc.Exec(lc.query, tree.RootNode())

		// lastFuncLine detects re-entry into a function (unused currently, kept for future).
		lastFuncLine := -1

		// pendingReceiver: for Go, receiver arrives in the same match as func.name
		// via the method.receiver capture.
		pendingReceiver := ""

		for {
			m, ok := qc.NextMatch()
			if !ok {
				break
			}
			// Collect all captures in this match by name.
			caps := map[string]*sitter.Node{}
			for _, cap := range m.Captures {
				caps[lc.query.CaptureNameForId(cap.Index)] = cap.Node
			}

			// ── Type/class definition ──────────────────────────────────────────
			if defNode, ok := caps["def.name"]; ok {
				name := defNode.Content(data)
				nodeID := "def:" + relPath + ":" + name
				lineStart := int(defNode.StartPoint().Row) + 1
				// Walk up to find the type_declaration for a more accurate range.
				if p := defNode.Parent(); p != nil && (p.Type() == "type_spec") {
					if pp := p.Parent(); pp != nil {
						lineStart = int(pp.StartPoint().Row) + 1
					}
				}
				attrs := map[string]string{
					"file":       relPath,
					"line_start": fmt.Sprint(lineStart),
				}
				if doc := docComment(defNode, data); doc != "" {
					attrs["doc"] = doc
				}
				g.AddNode(&graph.Node{
					ID:    nodeID,
					Type:  graph.NodeDefinition,
					Name:  name,
					Attrs: attrs,
				})
				g.AddEdge(nodeID, fileID, graph.EdgeBelongsTo, nil)
			}

			// ── Method receiver (Go) ───────────────────────────────────────────
			if recvNode, ok := caps["method.receiver"]; ok {
				pendingReceiver = receiverTypeName(recvNode, data)
			} else {
				pendingReceiver = ""
			}

			// ── Function / method ──────────────────────────────────────────────
			if funcNameNode, ok := caps["func.name"]; ok {
				name := funcNameNode.Content(data)
				lineStart, lineEnd := declarationRange(funcNameNode)

				// Reset current function when we enter a new function scope.
				if lineStart != lastFuncLine {
					lastFuncLine = lineStart
				}

				nodeID := "func:" + relPath + ":" + name + ":" + fmt.Sprint(lineStart)
				sig := buildSignature(name, funcNameNode, data)

				// Determine receiver type → def node id.
				receiverDef := ""
				if pendingReceiver != "" {
					receiverDef = "def:" + relPath + ":" + pendingReceiver
				}

				fi := funcInfo{
					id:          nodeID,
					name:        name,
					file:        relPath,
					lineStart:   lineStart,
					lineEnd:     lineEnd,
					signature:   sig,
					receiverDef: receiverDef,
				}
				funcs = append(funcs, fi)
				funcsByName[name] = append(funcsByName[name], nodeID)

				attrs := map[string]string{
					"file":       relPath,
					"line_start": fmt.Sprint(lineStart),
					"line_end":   fmt.Sprint(lineEnd),
					"signature":  sig,
				}
				if doc := docComment(funcNameNode, data); doc != "" {
					attrs["doc"] = doc
				}
				g.AddNode(&graph.Node{
					ID:    nodeID,
					Type:  graph.NodeFunction,
					Name:  name,
					Attrs: attrs,
				})
				g.AddEdge(nodeID, fileID, graph.EdgeBelongsTo, nil)

				// Wire HAS_METHOD immediately if receiver def exists.
				if receiverDef != "" && g.Nodes[receiverDef] != nil {
					g.AddEdge(receiverDef, nodeID, graph.EdgeHasMethod, nil)
				}

				pendingReceiver = ""
			}

			// ── Call expression ────────────────────────────────────────────────
			// call.func = direct call (resolve locally); call.sel = selector field
			// (pkg.Func / obj.Method — stored with "." prefix, resolved only to
			// same-file locals in resolveAndWire to avoid cross-package false hits).
			var callNode *sitter.Node
			callName := ""
			if cn, ok := caps["call.func"]; ok {
				callNode, callName = cn, cn.Content(data)
			} else if cn, ok := caps["call.sel"]; ok {
				callNode, callName = cn, "."+cn.Content(data)
			}
			if callName != "" {
				// Attribute the call to the innermost function whose line range
				// contains the call site.
				callLine := int(callNode.StartPoint().Row) + 1
				bestIdx := -1
				for i := range funcs {
					if funcs[i].file == relPath && funcs[i].lineStart <= callLine {
						if bestIdx == -1 || funcs[i].lineStart > funcs[bestIdx].lineStart {
							bestIdx = i
						}
					}
				}
				if bestIdx >= 0 && funcs[bestIdx].lineEnd >= callLine {
					funcs[bestIdx].calls = append(funcs[bestIdx].calls, callName)
				}
			}

			// ── Import ────────────────────────────────────────────────────────
			if importNode, ok := caps["import.path"]; ok {
				importStr := strings.Trim(importNode.Content(data), `"'`)
				importID := "import:" + importStr
				if g.Nodes[importID] == nil {
					g.AddNode(&graph.Node{
						ID:    importID,
						Type:  graph.NodeComponent,
						Name:  importStr,
						Attrs: map[string]string{"external": "true"},
					})
				}
				g.AddEdge(fileID, importID, graph.EdgeImports, nil)
			}
		}

		return nil
	})

	return funcs, funcsByName
}

// parseMarkdown adds a doc node for an .md file. The first heading becomes
// the name; up to 500 chars of body are stored for semantic search.
func (c *Connector) parseMarkdown(g *graph.Graph, path, relPath, fileID, projID, mtime string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	g.AddNode(&graph.Node{
		ID:   fileID,
		Type: graph.NodeFile,
		Name: relPath,
		Attrs: map[string]string{"path": path, "doc": "true", "mtime": mtime},
	})
	if projID != "" {
		g.AddEdge(fileID, projID, graph.EdgeBelongsTo, nil)
	}

	title := relPath
	var bodyLines []string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	headingFound := false
	for scanner.Scan() {
		line := scanner.Text()
		if !headingFound && strings.HasPrefix(line, "#") {
			title = strings.TrimSpace(strings.TrimLeft(line, "#"))
			headingFound = true
			continue
		}
		bodyLines = append(bodyLines, line)
	}
	body := strings.Join(bodyLines, " ")
	if len(body) > 500 {
		body = body[:500]
	}

	docID := "doc:" + relPath
	g.AddNode(&graph.Node{
		ID:   docID,
		Type: graph.NodeFile,
		Name: title + " (doc)",
		Attrs: map[string]string{
			"file":     relPath,
			"doc_body": body,
		},
	})
	g.AddEdge(docID, fileID, graph.EdgeBelongsTo, nil)
}

// builtinNames are language builtins that never deserve a call stub node.
var builtinNames = map[string]bool{
	// Go
	"append": true, "cap": true, "clear": true, "close": true, "copy": true,
	"delete": true, "len": true, "make": true, "max": true, "min": true,
	"new": true, "panic": true, "print": true, "println": true, "recover": true,
	"bool": true, "byte": true, "rune": true, "string": true, "error": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
	// Python / JS
	"isinstance": true, "range": true, "enumerate": true, "sorted": true,
	"repr": true, "getattr": true, "setattr": true, "hasattr": true,
	"require": true, "parseInt": true, "parseFloat": true, "Boolean": true,
	"Number": true, "String": true, "Array": true, "Object": true, "Symbol": true,
}

// resolveAndWire does the second pass:
//  1. Resolves each call target to a local function node when possible.
//  2. Falls back to an external stub when not found locally.
//  3. Wires HAS_METHOD edges for cases missed during the first pass.
func resolveAndWire(g *graph.Graph, funcs []funcInfo, funcsByName map[string][]string) {
	for i := range funcs {
		fi := &funcs[i]

		// Wire HAS_METHOD when receiver type was defined after the method.
		if fi.receiverDef != "" && g.Nodes[fi.receiverDef] != nil {
			already := false
			for _, ed := range g.EdgesOf(fi.receiverDef) {
				if ed.Type == graph.EdgeHasMethod && ed.To == fi.id {
					already = true
					break
				}
			}
			if !already {
				g.AddEdge(fi.receiverDef, fi.id, graph.EdgeHasMethod, nil)
			}
		}

		// Resolve CALLS edges.
		seen := map[string]bool{}
		for _, calledName := range fi.calls {
			if seen[calledName] {
				continue
			}
			seen[calledName] = true

			// Selector call ("." prefix): pkg.Func or obj.Method. Only resolve
			// against same-file locals (receiver methods live next to their
			// type); anything else is external — never match cross-package by
			// bare name (filepath.Join must not resolve to a local Join).
			isSelector := strings.HasPrefix(calledName, ".")
			bareName := strings.TrimPrefix(calledName, ".")

			targets := funcsByName[bareName]
			sameFile := ""
			for _, tid := range targets {
				if tn := g.Nodes[tid]; tn != nil && tn.Attrs["file"] == fi.file {
					sameFile = tid
					break
				}
			}

			switch {
			case isSelector && sameFile != "":
				g.AddEdge(fi.id, sameFile, graph.EdgeCalls, nil)
			case isSelector:
				// External selector call: skip. Stubs for stdlib/methods
				// (Join, Sprintf, Close…) are pure noise in the graph.
			case len(targets) == 1:
				g.AddEdge(fi.id, targets[0], graph.EdgeCalls, nil)
			case len(targets) > 1 && sameFile != "":
				g.AddEdge(fi.id, sameFile, graph.EdgeCalls, nil)
			case len(targets) > 1:
				// Ambiguous cross-file name (e.g. dozens of "append" defs in a
				// monorepo): wiring to all targets creates quadratic false
				// edges. Skip — better no edge than thousands of wrong ones.
			case builtinNames[bareName]:
				// Language builtins (append, len, print…) — no stub, pure noise.
			default:
				stubID := "call:" + bareName
				if g.Nodes[stubID] == nil {
					g.AddNode(&graph.Node{
						ID:    stubID,
						Type:  graph.NodeFunction,
						Name:  bareName,
						Attrs: map[string]string{"external": "true"},
					})
				}
				g.AddEdge(fi.id, stubID, graph.EdgeCalls, nil)
			}
		}
	}
}

func findProject(g *graph.Graph, path string, root string) string {
	dir := filepath.Dir(path)
	for {
		id := "proj:" + dir
		if g.Nodes[id] != nil {
			return id
		}
		if dir == root || dir == "." || dir == "/" {
			break
		}
		dir = filepath.Dir(dir)
	}
	id := "proj:" + root
	if g.Nodes[id] != nil {
		return id
	}
	return ""
}
