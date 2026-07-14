package codebase

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"graphallthethings/internal/graph"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

type Connector struct {
	dir string
	// only restricts parseFiles to this set of relative paths. nil = all.
	// Set internally by Update for incremental re-parsing.
	only map[string]bool
	// tsconfigs holds path-alias mappings (baseUrl + paths) of every
	// tsconfig.json in the tree, deepest directory first. Loaded lazily.
	tsconfigs []tsPathsConfig
	// pendingMentions collects code tokens found in each parsed markdown doc,
	// resolved against the finished graph by wireMentions.
	pendingMentions []docMentions
	// pendingAssocs collects ORM association calls found while parsing,
	// resolved against the finished model registry by wireModels.
	pendingAssocs []modelAssoc
	// modelBases is the inheritance-marker set identifying ORM model classes
	// (defaults + .gatt/models.json base_classes). Loaded lazily.
	modelBases map[string]bool
	// pendingClientCalls collects client-side HTTP call sites, wired to
	// their matching route nodes by wireClientCalls.
	pendingClientCalls []clientCall
	// pendingRoutes collects HTTP route registrations found while parsing,
	// resolved against the finished function index by wireRoutes.
	pendingRoutes []routeInfo
	// gitFiles caches the git-ls-files-based non-ignored file set (relative
	// paths, gatt's own .gatt/gatt-out always excluded regardless of the
	// target repo's own .gitignore) — see gitFileSet. Computed lazily, once
	// per Connector; gitFilesOK is false when c.dir isn't a git checkout (or
	// the command failed), meaning callers fall back to SkipDir-only walking.
	gitFiles      map[string]bool
	gitFilesOK    bool
	gitFilesKnown bool
}

// docMentions holds the candidate code references extracted from one doc.
type docMentions struct {
	docID  string
	tokens []string
}

// tsPathsConfig is one tsconfig.json's compilerOptions alias mapping.
type tsPathsConfig struct {
	dir     string // directory containing the tsconfig
	baseURL string
	paths   map[string][]string // "@modules/*" → ["src/modules/*"]
}

func New(dir string) *Connector {
	// Anchor to an absolute root: the graph's source then identifies the repo
	// unambiguously, so refresh/query work from any cwd instead of silently
	// re-pointing the graph at whatever tree the process happens to run in.
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return &Connector{dir: dir}
}

// parseableExts are the file extensions the extractor understands.
var parseableExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".py": true, ".rs": true, ".md": true,
}

// dataExts are data/config/style files indexed as plain file nodes (no
// parsing): they carry a content hash so blast-radius queries can walk
// IMPORTS/CO_CHANGED edges into them and flag identical/diverged copies.
// Stylesheets are here because git co-change is their only edge source.
var dataExts = map[string]bool{
	".json": true, ".yaml": true, ".yml": true, ".sql": true, ".toml": true,
	".css": true, ".scss": true, ".less": true,
}

func indexableExt(name string) bool {
	ext := filepath.Ext(name)
	return parseableExts[ext] || dataExts[ext]
}

// contentHash returns a short sha256 of the file, or "" for unreadable/huge files.
func contentHash(path string, size int64) string {
	if size > 2<<20 {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:8])
}

func (c *Connector) Name() string { return "codebase" }

// funcInfo holds all information extracted about a function/method before
// it is committed to the graph. Building in two passes lets us resolve
// call targets to local definitions after the whole codebase is scanned.
type funcInfo struct {
	id          string
	name        string
	file        string // relative path
	lineStart   int
	lineEnd     int
	signature   string
	receiverDef string   // definition node id this is a method of (Go), or ""
	calls       []string // raw called names (resolved in second pass)
}

func (c *Connector) Extract(ctx context.Context) (*graph.Graph, error) {
	g := graph.New(fmt.Sprintf("codebase:%s", c.dir))
	g.ExtractedAt = time.Now().UTC()

	c.loadSemanticOverlay(g)
	c.detectProjects(g)

	funcs, funcsByName := c.parseFiles(ctx, g)
	resolveAndWire(g, funcs, funcsByName)
	c.wireMentions(g)
	c.wireRoutes(g, funcsByName)
	c.wireModels(g)
	c.wireClientCalls(g, funcs)
	c.wireRouteModels(g)
	c.mineGitCoChanges(ctx, g)

	return g, nil
}

// scanFiles walks the tree with the same skip rules as parseFiles and returns
// relPath → mtime (UnixNano) for every parseable file. Cheap: stat only.
// When c.dir is a git checkout, gitignored files/directories are additionally
// excluded via gitFileSet (SkipDir alone only knows a fixed list of common
// build-output names — dist, build, node_modules, ... — while a repo's own
// .gitignore is the authoritative, project-specific answer).
func (c *Connector) scanFiles() map[string]string {
	gitFiles, gitOK := c.gitFileSet()
	out := map[string]string{}
	filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if graph.SkipDir(d.Name(), path == c.dir) {
				return filepath.SkipDir
			}
			return nil
		}
		if !indexableExt(d.Name()) {
			return nil
		}
		rel, _ := filepath.Rel(c.dir, path)
		if gitOK && !gitFiles[rel] {
			return nil
		}
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

	// Root-mismatch guard: if most of the graph's recorded files don't exist
	// under this root, the connector is pointed at the wrong tree (legacy
	// graphs record a relative source, so a wrong cwd used to silently evict
	// the whole graph and re-extract the wrong repo). Refuse instead.
	if len(prevM) >= 20 {
		found := 0
		for rel := range prevM {
			if _, ok := cur[rel]; ok {
				found++
			}
		}
		if found*2 < len(prevM) {
			return prev, "", fmt.Errorf("refusing refresh: %d/%d files recorded in the graph exist under %s — graph root mismatch (wrong cwd for a relative-source graph, or the repo moved); run from the repo root or re-extract", found, len(prevM), c.dir)
		}
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
	type relink struct{ from, name, file, typ string }
	var relinks []relink
	// File↔file edges touching dirty file nodes: file ids are path-stable, so
	// these can be re-added verbatim once the node is re-created. IMPORTS from
	// dirty files are excluded (their re-parse re-emits them); CO_CHANGED has
	// no re-emitter until the next full extract, so keep any edge with a
	// surviving counterpart.
	var fileEdgeRelinks []graph.Edge
	for _, e := range prev.Edges {
		tn, fn := prev.Nodes[e.To], prev.Nodes[e.From]
		if tn == nil || fn == nil {
			continue
		}
		bothFiles := tn.Type == graph.NodeFile && fn.Type == graph.NodeFile
		switch e.Type {
		case graph.EdgeImports:
			if bothFiles && dirty[tn.Name] && !dirty[fn.Name] {
				fileEdgeRelinks = append(fileEdgeRelinks, e)
			}
		case graph.EdgeCoChanged:
			if bothFiles && (dirty[tn.Name] || dirty[fn.Name]) {
				fileEdgeRelinks = append(fileEdgeRelinks, e)
			}
		case graph.EdgeCalls:
			if dirty[tn.Attrs["file"]] && !dirty[fn.Attrs["file"]] {
				relinks = append(relinks, relink{e.From, tn.Name, tn.Attrs["file"], graph.EdgeCalls})
			}
		case graph.EdgeMentions:
			// Doc unchanged, target evicted: function ids shift with line
			// numbers → relink by name; def/file ids are path-stable → verbatim.
			if dirty[fn.Attrs["file"]] {
				continue // dirty doc re-parses and re-emits its mentions
			}
			if dirty[tn.Attrs["file"]] && strings.HasPrefix(e.To, "func:") {
				relinks = append(relinks, relink{e.From, tn.Name, tn.Attrs["file"], graph.EdgeMentions})
			} else if (strings.HasPrefix(e.To, "def:") && dirty[tn.Attrs["file"]]) ||
				(tn.Type == graph.NodeFile && dirty[tn.Name]) {
				fileEdgeRelinks = append(fileEdgeRelinks, e)
			}
		case graph.EdgeReferences:
			// Model association declared in a third file (setupAssociations
			// pattern): model ids are path-stable, so re-add verbatim when an
			// endpoint's file re-parses. A dirty declaring file re-emits its
			// own associations, so those are skipped here.
			if fn.Type == graph.NodeModel && tn.Type == graph.NodeModel &&
				!dirty[e.Attrs["declared_in"]] &&
				(dirty[fn.Attrs["file"]] || dirty[tn.Attrs["file"]]) {
				fileEdgeRelinks = append(fileEdgeRelinks, e)
			}
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
	c.wireMentions(prev)
	c.wireRoutes(prev, funcsByName)
	c.wireModels(prev)
	c.wireClientCalls(prev, newFuncs)
	c.wireRouteModels(prev)

	// Calls from *unchanged* files may now have a target that didn't exist at
	// their extract time: re-resolve their persisted raw call names against
	// the names newly defined in this update.
	newNames := map[string]bool{}
	for _, fi := range newFuncs {
		newNames[fi.name] = true
	}
	if len(newNames) > 0 {
		hasEdge := map[string]bool{} // caller id + target name, local targets only
		for _, e := range prev.Edges {
			if e.Type != graph.EdgeCalls {
				continue
			}
			if tn := prev.Nodes[e.To]; tn != nil && tn.Attrs["external"] != "true" {
				hasEdge[e.From+"\x00"+tn.Name] = true
			}
		}
		for id, n := range prev.Nodes {
			if n.Type != graph.NodeFunction || n.Attrs["calls_raw"] == "" || dirty[n.Attrs["file"]] {
				continue
			}
			for _, raw := range strings.Fields(n.Attrs["calls_raw"]) {
				bare := strings.TrimPrefix(raw, ".")
				if !newNames[bare] || hasEdge[id+"\x00"+bare] {
					continue
				}
				if target := resolveCall(prev, n.Attrs["file"], raw, funcsByName); target != "" {
					prev.AddEdge(id, target, graph.EdgeCalls, nil)
					hasEdge[id+"\x00"+bare] = true
				}
			}
		}
	}

	// Re-attach surviving callers to the re-parsed targets.
	byFileName := map[string]string{}
	for _, fi := range newFuncs {
		key := fi.file + "\x00" + fi.name
		if _, ok := byFileName[key]; !ok {
			byFileName[key] = fi.id
		}
	}
	for _, e := range fileEdgeRelinks {
		if prev.Nodes[e.From] != nil && prev.Nodes[e.To] != nil {
			prev.AddEdge(e.From, e.To, e.Type, e.Attrs)
		}
	}

	// GENERATES edges come from the overlay, not the parser: eviction dropped
	// those touching a dirty file node, so re-declare exactly that subset.
	for _, p := range c.generatesPairs() {
		fn, tn := prev.Nodes[p[0]], prev.Nodes[p[1]]
		if fn == nil || tn == nil {
			continue
		}
		if dirty[fn.Name] || dirty[tn.Name] {
			prev.AddEdge(p[0], p[1], graph.EdgeGenerates, nil)
		}
	}

	seenRelink := map[string]bool{}
	for _, r := range relinks {
		key := r.from + "\x00" + r.file + "\x00" + r.name + "\x00" + r.typ
		if seenRelink[key] {
			continue
		}
		seenRelink[key] = true
		if target := byFileName[r.file+"\x00"+r.name]; target != "" {
			prev.AddEdge(r.from, target, r.typ, nil)
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

	for _, p := range c.generatesPairs() {
		g.AddEdge(p[0], p[1], graph.EdgeGenerates, nil)
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

// generatesPairs reads the overlay's generates declarations —
// [{"from": "backend/scripts/gen.js", "to": "frontend/src/api/x.json"}] in
// .gatt/relations.json — as (fromID, toID) file-node pairs. These declare
// file → file generation pipelines the parser can't see.
func (c *Connector) generatesPairs() [][2]string {
	data, err := os.ReadFile(filepath.Join(c.dir, ".gatt", "relations.json"))
	if err != nil {
		return nil
	}
	var payload struct {
		Generates []map[string]string `json:"generates"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil
	}
	var out [][2]string
	for _, gen := range payload.Generates {
		from, to := gen["from"], gen["to"]
		if from == "" || to == "" {
			continue
		}
		out = append(out, [2]string{"file:" + filepath.Join(c.dir, from), "file:" + filepath.Join(c.dir, to)})
	}
	return out
}

// detectProjects walks the repo looking for project manifest files.
func (c *Connector) detectProjects(g *graph.Graph) {
	gitFiles, gitOK := c.gitFileSet()
	tracked := func(dir, manifest string) bool {
		if !gitOK {
			return true
		}
		rel, _ := filepath.Rel(c.dir, filepath.Join(dir, manifest))
		return gitFiles[rel]
	}
	filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if graph.SkipDir(d.Name(), path == c.dir) {
				return filepath.SkipDir
			}
			if _, e := os.Stat(filepath.Join(path, "go.mod")); e == nil && tracked(path, "go.mod") {
				g.AddNode(&graph.Node{ID: "proj:" + path, Type: graph.NodeProject, Name: filepath.Base(path) + " (Go)"})
			} else if _, e := os.Stat(filepath.Join(path, "package.json")); e == nil && tracked(path, "package.json") {
				g.AddNode(&graph.Node{ID: "proj:" + path, Type: graph.NodeProject, Name: filepath.Base(path) + " (NPM)"})
			} else if _, e := os.Stat(filepath.Join(path, "pyproject.toml")); e == nil && tracked(path, "pyproject.toml") {
				g.AddNode(&graph.Node{ID: "proj:" + path, Type: graph.NodeProject, Name: filepath.Base(path) + " (Python)"})
			} else if _, e := os.Stat(filepath.Join(path, "Cargo.toml")); e == nil && tracked(path, "Cargo.toml") {
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
	lang *sitter.Language
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
			(type_declaration (type_spec
			  name: (type_identifier) @gomodel.name
			  type: (struct_type) @gomodel.struct))
			(function_declaration name: (identifier) @func.name)
			(method_declaration receiver: (parameter_list) @method.receiver name: (field_identifier) @func.name)
			(call_expression function: (identifier) @call.func)
			(call_expression function: (selector_expression field: (field_identifier) @call.sel))
			(import_spec path: (interpreted_string_literal) @import.path)
			(comment) @loose.comment
			`,
		}
	case ".ts", ".tsx":
		return &langConfig{
			lang: typescript.GetLanguage(),
			queryStr: `
			(class_declaration name: (type_identifier) @def.name)
			(function_declaration name: (identifier) @func.name)
			(method_definition name: (property_identifier) @func.name)
			(variable_declarator name: (identifier) @func.name value: (arrow_function))
			(variable_declarator name: (identifier) @func.name value: (function_expression))
			(call_expression function: (identifier) @call.func)
			(call_expression function: (member_expression property: (property_identifier) @call.sel))
			(import_statement source: (string) @import.path)
			(comment) @loose.comment
			(call_expression
			  function: (member_expression
			    object: (identifier) @route.obj
			    property: (property_identifier) @route.method)
			  arguments: (arguments . [(string) (template_string)] @route.path)) @route.call
			(call_expression
			  function: (identifier) @fetch.fn
			  arguments: (arguments . [(string) (template_string)] @fetch.path)) @fetch.call
			(call_expression
			  function: (member_expression
			    object: (identifier) @model.obj
			    property: (property_identifier) @model.method)
			  arguments: (arguments . (object) @model.fields)) @model.call
			(call_expression
			  function: (member_expression
			    object: (identifier) @modeldef.obj
			    property: (property_identifier) @modeldef.method)
			  arguments: (arguments . (string) @modeldef.name (object) @modeldef.fields)) @modeldef.call
			(call_expression
			  function: (member_expression
			    object: (identifier) @assoc.obj
			    property: (property_identifier) @assoc.method)
			  arguments: (arguments . (identifier) @assoc.target)) @assoc.call
			(class_declaration
			  name: (_) @clsmodel.name
			  (class_heritage) @clsmodel.heritage) @clsmodel.class
			(import_statement
			  (import_clause
			    (named_imports
			      (import_specifier name: (identifier) @state.import_name)))
			  source: (string) @state.import_src)
			(variable_declarator
			  name: (identifier) @state.import_name
			  value: (call_expression
			    function: (identifier) @state.require_fn
			    arguments: (arguments (string) @state.import_src)))
			(assignment_expression
			  left: (member_expression
			    object: (identifier) @state.write_obj
			    property: (property_identifier) @state.write_prop)) @state.write
			(assignment_expression
			  left: (member_expression
			    object: (member_expression
			      object: (identifier) @state.write2_obj
			      property: (property_identifier) @state.write2_prop)
			    property: (property_identifier) @state.write2_sub)) @state.write2
			(member_expression
			  object: (identifier) @state.read_obj
			  property: (property_identifier) @state.read_prop) @state.read
			(variable_declarator name: (identifier) @const.name) @const.decl
			(call_expression
			  function: (member_expression
			    object: (identifier) @oa.obj
			    property: (property_identifier) @oa.prop)
			  arguments: (arguments
			    . (member_expression
			        object: (identifier) @state.oa_target_obj
			        property: (property_identifier) @state.oa_target_prop))) @oa.call
			`,
		}
	case ".js", ".jsx":
		return &langConfig{
			lang: javascript.GetLanguage(),
			queryStr: `
			(class_declaration name: (identifier) @def.name)
			(function_declaration name: (identifier) @func.name)
			(method_definition name: (property_identifier) @func.name)
			(variable_declarator name: (identifier) @func.name value: (arrow_function))
			(variable_declarator name: (identifier) @func.name value: (function_expression))
			(call_expression function: (identifier) @call.func)
			(call_expression function: (member_expression property: (property_identifier) @call.sel))
			(import_statement source: (string) @import.path)
			(comment) @loose.comment
			(call_expression
			  function: (member_expression
			    object: (identifier) @route.obj
			    property: (property_identifier) @route.method)
			  arguments: (arguments . [(string) (template_string)] @route.path)) @route.call
			(call_expression
			  function: (identifier) @fetch.fn
			  arguments: (arguments . [(string) (template_string)] @fetch.path)) @fetch.call
			(call_expression
			  function: (member_expression
			    object: (identifier) @model.obj
			    property: (property_identifier) @model.method)
			  arguments: (arguments . (object) @model.fields)) @model.call
			(call_expression
			  function: (member_expression
			    object: (identifier) @modeldef.obj
			    property: (property_identifier) @modeldef.method)
			  arguments: (arguments . (string) @modeldef.name (object) @modeldef.fields)) @modeldef.call
			(call_expression
			  function: (member_expression
			    object: (identifier) @assoc.obj
			    property: (property_identifier) @assoc.method)
			  arguments: (arguments . (identifier) @assoc.target)) @assoc.call
			(class_declaration
			  name: (_) @clsmodel.name
			  (class_heritage) @clsmodel.heritage) @clsmodel.class
			(import_statement
			  (import_clause
			    (named_imports
			      (import_specifier name: (identifier) @state.import_name)))
			  source: (string) @state.import_src)
			(variable_declarator
			  name: (identifier) @state.import_name
			  value: (call_expression
			    function: (identifier) @state.require_fn
			    arguments: (arguments (string) @state.import_src)))
			(assignment_expression
			  left: (member_expression
			    object: (identifier) @state.write_obj
			    property: (property_identifier) @state.write_prop)) @state.write
			(assignment_expression
			  left: (member_expression
			    object: (member_expression
			      object: (identifier) @state.write2_obj
			      property: (property_identifier) @state.write2_prop)
			    property: (property_identifier) @state.write2_sub)) @state.write2
			(member_expression
			  object: (identifier) @state.read_obj
			  property: (property_identifier) @state.read_prop) @state.read
			(variable_declarator name: (identifier) @const.name) @const.decl
			(call_expression
			  function: (member_expression
			    object: (identifier) @oa.obj
			    property: (property_identifier) @oa.prop)
			  arguments: (arguments
			    . (member_expression
			        object: (identifier) @state.oa_target_obj
			        property: (property_identifier) @state.oa_target_prop))) @oa.call
			`,
		}
	case ".py":
		return &langConfig{
			lang: python.GetLanguage(),
			queryStr: `
			(class_definition name: (identifier) @def.name)
			(class_definition
			  name: (identifier) @pymodel.name
			  superclasses: (argument_list) @pymodel.bases
			  body: (block) @pymodel.body) @pymodel.class
			(function_definition name: (identifier) @func.name)
			(call function: (identifier) @call.func)
			(call function: (attribute attribute: (identifier) @call.sel))
			(import_statement name: (dotted_name) @import.path)
			(import_from_statement module_name: (dotted_name) @import.path)
			(comment) @loose.comment
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
			(line_comment) @loose.comment
			(block_comment) @loose.comment
			`,
		}
	}
	return nil
}

// isTopLevelDeclarator reports whether a variable_declarator sits directly
// in a module-scope declaration — `const x = ...` (optionally exported) at
// the top of the file, not nested in any function/block. This is what keeps
// the const.decl capture (see langFor's TS/JS queryStr) from flooding the
// graph with every local variable in every function — only module-level
// bindings (config objects, lookup tables, queue/route definitions) become
// nodes.
func isTopLevelDeclarator(n *sitter.Node) bool {
	decl := n.Parent()
	if decl == nil {
		return false
	}
	switch decl.Type() {
	case "lexical_declaration", "variable_declaration":
	default:
		return false
	}
	p := decl.Parent()
	if p != nil && p.Type() == "export_statement" {
		p = p.Parent()
	}
	return p != nil && p.Type() == "program"
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
			"method_definition", "method",
			// const x = () => {…}: the declarator spans name + arrow body.
			"variable_declarator":
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
			t == "method_definition" || t == "method" ||
			t == "variable_declarator" {
			break
		}
		decl = decl.Parent()
	}
	if decl == nil {
		return name
	}
	// const x = () => {…}: parameters/return type live on the arrow value.
	if decl.Type() == "variable_declarator" {
		if v := decl.ChildByFieldName("value"); v != nil {
			decl = v
		}
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
// Every source line it consumes (the comment block itself; a Python
// docstring isn't a comment node so nothing to mark there) is recorded in
// consumed so the loose-comment pass doesn't re-emit it as a floating
// comment; consumed may be nil to skip tracking.
func docComment(nameNode *sitter.Node, src []byte, consumed map[int]bool) string {
	decl := nameNode.Parent()
	for decl != nil {
		switch decl.Type() {
		case "function_declaration", "method_declaration", "type_declaration",
			"function_definition", "fn_item", "method_definition", "method",
			"class_declaration", "class_definition", "struct_item",
			// const x = () => {…} / export const x = {…}: the doc comment
			// precedes the declarator, same as any other declaration.
			"variable_declarator":
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
			lines = append([]string{stripCommentMarkers(sib.Content(src))}, lines...)
			markConsumedLines(sib, consumed)
			below = sib
		}
	}

	doc := strings.TrimSpace(strings.Join(lines, " "))
	if len(doc) > 300 {
		doc = doc[:300]
	}
	return doc
}

// stripCommentMarkers removes the // or /* */ delimiters from a comment
// node's raw content, leaving the trimmed text.
func stripCommentMarkers(text string) string {
	text = strings.TrimPrefix(text, "//")
	text = strings.TrimPrefix(text, "/*")
	text = strings.TrimSuffix(text, "*/")
	return strings.TrimSpace(text)
}

// markConsumedLines records every source line (1-indexed, inclusive) node
// spans into consumed. Used to tell the loose-comment pass "this comment was
// already surfaced as a declaration's doc — don't emit it again". consumed
// may be nil, in which case this is a no-op.
func markConsumedLines(node *sitter.Node, consumed map[int]bool) {
	if consumed == nil {
		return
	}
	for row := int(node.StartPoint().Row) + 1; row <= int(node.EndPoint().Row)+1; row++ {
		consumed[row] = true
	}
}

// leadingFileDoc returns the file-level doc comment: a comment block at the
// very top of the file (Go package comment, JS/TS/Rust file header) or, for
// Python, the module docstring (a bare string as the file's first
// statement). Trimmed to 300 chars, same cap as docComment. Used to annotate
// `gatt tree` output when a file has no other summary to show. consumed
// tracks which lines it consumed, same contract as docComment.
func leadingFileDoc(root *sitter.Node, src []byte, consumed map[int]bool) string {
	if root == nil || root.NamedChildCount() == 0 {
		return ""
	}
	first := root.NamedChild(0)

	if first.Type() == "expression_statement" {
		if s := first.NamedChild(0); s != nil && s.Type() == "string" {
			doc := strings.Trim(s.Content(src), "\"' \n")
			if len(doc) > 300 {
				doc = doc[:300]
			}
			return doc
		}
	}

	if !strings.Contains(first.Type(), "comment") {
		return ""
	}
	var lines []string
	for node := first; node != nil && strings.Contains(node.Type(), "comment"); node = node.NextNamedSibling() {
		lines = append(lines, stripCommentMarkers(node.Content(src)))
		markConsumedLines(node, consumed)
	}
	doc := strings.TrimSpace(strings.Join(lines, " "))
	if len(doc) > 300 {
		doc = doc[:300]
	}
	return doc
}

// looseCommentMinChars is the minimum trimmed length a floating comment
// needs to be worth its own graph node — filters `// TODO` and similar
// one-word markers, keeping the substantive "why" blocks.
const looseCommentMinChars = 30

// emitLooseComments adds a NodeComment for every substantive comment in
// comments that docComment/leadingFileDoc didn't already consume as a
// declaration's or the file's doc. Contiguous single-line (`//`) comments —
// each its own AST node — are merged into one logical block first, the same
// way docComment merges a leading comment run; a block comment (`/* */`) is
// already a single node. Each comment is attributed to its enclosing
// function by line range (funcs, same technique as call-site attribution)
// when it falls inside one.
func emitLooseComments(g *graph.Graph, comments []*sitter.Node, consumed map[int]bool, funcs []funcInfo, relPath, fileID string, src []byte) {
	if len(comments) == 0 {
		return
	}
	sort.Slice(comments, func(i, j int) bool {
		return comments[i].StartPoint().Row < comments[j].StartPoint().Row
	})

	type block struct {
		startLine, endLine int
		text               string
	}
	var blocks []block
	for _, cn := range comments {
		startLine := int(cn.StartPoint().Row) + 1
		endLine := int(cn.EndPoint().Row) + 1
		alreadyConsumed := false
		for l := startLine; l <= endLine; l++ {
			if consumed[l] {
				alreadyConsumed = true
				break
			}
		}
		if alreadyConsumed {
			continue
		}
		text := stripCommentMarkers(cn.Content(src))
		if last := len(blocks) - 1; last >= 0 && startLine-blocks[last].endLine <= 1 {
			blocks[last].text += " " + text
			blocks[last].endLine = endLine
		} else {
			blocks = append(blocks, block{startLine, endLine, text})
		}
	}

	for _, bl := range blocks {
		text := strings.TrimSpace(bl.text)
		if len(text) < looseCommentMinChars {
			continue
		}
		if len(text) > 400 {
			text = text[:400]
		}
		name := text
		if len(name) > 60 {
			name = name[:60] + "…"
		}
		id := fmt.Sprintf("comment:%s:%d", relPath, bl.startLine)
		g.AddNode(&graph.Node{
			ID:   id,
			Type: graph.NodeComment,
			Name: name,
			Attrs: map[string]string{
				"file": relPath,
				"line": fmt.Sprint(bl.startLine),
				"text": text,
			},
		})
		g.AddEdge(id, fileID, graph.EdgeBelongsTo, nil)

		bestIdx := -1
		for i := range funcs {
			if funcs[i].file == relPath && funcs[i].lineStart <= bl.startLine {
				if bestIdx == -1 || funcs[i].lineStart > funcs[bestIdx].lineStart {
					bestIdx = i
				}
			}
		}
		if bestIdx >= 0 && funcs[bestIdx].lineEnd >= bl.startLine {
			g.AddEdge(id, funcs[bestIdx].id, graph.EdgeBelongsTo, nil)
		}
	}
}

// isGenerated reports whether the source carries a generated-code marker in
// its first lines (Go convention "Code generated ... DO NOT EDIT",
// "@generated") or a telltale filename. Generated entities stay in the graph
// and remain findable, but context packs skip them: a 1,700-method ANTLR
// parser must never win the ranking over hand-written code.
func isGenerated(relPath string, data []byte) bool {
	base := filepath.Base(relPath)
	for _, suf := range []string{".pb.go", "_gen.go", ".gen.go", ".min.js", ".min.css"} {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	head := data
	if len(head) > 2048 {
		head = head[:2048]
	}
	for _, line := range strings.SplitN(string(head), "\n", 20) {
		if strings.Contains(line, "DO NOT EDIT") || strings.Contains(line, "@generated") {
			return true
		}
	}
	return false
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

	if c.tsconfigs == nil {
		c.loadTSConfigs()
	}

	// Pre-compile queries for all supported extensions once.
	// NewQuery is not safe to call in a tight loop inside WalkDir callbacks.
	compiledLangs := map[string]*langConfig{}
	for _, ext := range []string{".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs"} {
		lc := langFor(ext)
		if lc == nil {
			continue
		}
		q, err := sitter.NewQuery([]byte(lc.queryStr), lc.lang)
		if err != nil {
			// A bad query silently produces an empty graph for this whole
			// language otherwise (every file of this ext is then skipped by
			// `lc := compiledLangs[ext]; if lc == nil { return nil }` below)
			// — surface it instead of failing quiet.
			fmt.Fprintf(os.Stderr, "gatt: %s query failed to compile, skipping: %v\n", ext, err)
			continue
		}
		lc.query = q
		compiledLangs[ext] = lc
	}

	gitFiles, gitOK := c.gitFileSet()

	filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && graph.SkipDir(d.Name(), path == c.dir) {
				return filepath.SkipDir
			}
			return nil
		}

		ext := filepath.Ext(d.Name())
		relPath, _ := filepath.Rel(c.dir, path)
		if c.only != nil && !c.only[relPath] {
			return nil
		}
		if gitOK && !gitFiles[relPath] {
			return nil
		}
		projID := findProject(g, path, c.dir)
		fileID := "file:" + path

		mtime := ""
		var size int64
		if info, err := d.Info(); err == nil {
			mtime = fmt.Sprint(info.ModTime().UnixNano())
			size = info.Size()
		}

		// Data/config files → bare file nodes with a content hash; no parsing.
		// The hash lets Blast flag identical vs diverged copies of the same file.
		if dataExts[ext] {
			attrs := map[string]string{"path": path, "mtime": mtime, "data": "true"}
			if h := contentHash(path, size); h != "" {
				attrs["hash"] = h
			}
			g.AddNode(&graph.Node{
				ID:    fileID,
				Type:  graph.NodeFile,
				Name:  relPath,
				Attrs: attrs,
			})
			if projID != "" {
				g.AddEdge(fileID, projID, graph.EdgeBelongsTo, nil)
			}
			return nil
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

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		gen := isGenerated(relPath, data)
		srcLines := strings.Split(string(data), "\n")

		fileAttrs := map[string]string{"path": path, "mtime": mtime}
		if gen {
			fileAttrs["generated"] = "true"
		}
		g.AddNode(&graph.Node{
			ID:    fileID,
			Type:  graph.NodeFile,
			Name:  relPath,
			Attrs: fileAttrs,
		})
		if projID != "" {
			g.AddEdge(fileID, projID, graph.EdgeBelongsTo, nil)
		}

		parser := sitter.NewParser()
		parser.SetLanguage(lc.lang)
		tree, err := parser.ParseCtx(ctx, nil, data)
		if err != nil || tree == nil {
			return nil
		}

		// consumed tracks which source lines were already surfaced as a
		// declaration's doc comment (or the file's leading doc), so the
		// loose-comment pass below doesn't re-emit them as floating comments.
		consumed := map[int]bool{}

		// fileAttrs is the same map the file node's Attrs already points at
		// (AddNode stores it by reference), so this mutation is visible on
		// the node already committed to the graph above.
		if doc := leadingFileDoc(tree.RootNode(), data, consumed); doc != "" {
			fileAttrs["file_doc"] = doc
		}

		qc := sitter.NewQueryCursor()
		qc.Exec(lc.query, tree.RootNode())

		// lastFuncLine detects re-entry into a function (unused currently, kept for future).
		lastFuncLine := -1

		// pendingReceiver: for Go, receiver arrives in the same match as func.name
		// via the method.receiver capture.
		pendingReceiver := ""

		// looseComments is deferred until the whole file has been walked: a
		// comment's own query match can be yielded before the declaration
		// match that consumes it (they're separate patterns in the same
		// query), so `consumed` isn't complete until the loop below ends.
		var looseComments []*sitter.Node

		// singletons maps a named-import binding to its resolved target file
		// id (JS/TS/JSX only — see state.* captures in langFor), populated as
		// import.state matches are seen; stateAccesses is deferred the same
		// way looseComments is, since an access may be matched before the
		// import statement that resolves its binding.
		singletons := map[string]string{}
		var stateAccesses []stateAccessRaw

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
				if doc := docComment(defNode, data, consumed); doc != "" {
					attrs["doc"] = doc
				}
				if gen {
					attrs["generated"] = "true"
				}
				g.AddNode(&graph.Node{
					ID:    nodeID,
					Type:  graph.NodeDefinition,
					Name:  name,
					Attrs: attrs,
				})
				g.AddEdge(nodeID, fileID, graph.EdgeBelongsTo, nil)
			}

			// ── Top-level const/exported binding (JS/TS/JSX) ───────────────────
			// A module-scope `const x = {...}`/`export const x = [...]` (config
			// objects, lookup tables, queue/route definitions) — anything whose
			// value isn't a function (those are already function nodes via
			// func.name) and that isn't nested in some function/block.
			if nameNode, ok := caps["const.name"]; ok {
				if declNode, ok := caps["const.decl"]; ok && isTopLevelDeclarator(declNode) {
					valType := ""
					if v := declNode.ChildByFieldName("value"); v != nil {
						valType = v.Type()
					}
					if valType != "arrow_function" && valType != "function_expression" {
						name := nameNode.Content(data)
						nodeID := "def:" + relPath + ":" + name
						if g.Nodes[nodeID] == nil {
							lineStart := int(declNode.StartPoint().Row) + 1
							attrs := map[string]string{
								"file":       relPath,
								"line_start": fmt.Sprint(lineStart),
							}
							if doc := docComment(nameNode, data, consumed); doc != "" {
								attrs["doc"] = doc
							}
							if gen {
								attrs["generated"] = "true"
							}
							g.AddNode(&graph.Node{
								ID:    nodeID,
								Type:  graph.NodeDefinition,
								Name:  name,
								Attrs: attrs,
							})
							g.AddEdge(nodeID, fileID, graph.EdgeBelongsTo, nil)
						}
					}
				}
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
				if doc := docComment(funcNameNode, data, consumed); doc != "" {
					attrs["doc"] = doc
				}
				if gen {
					attrs["generated"] = "true"
				}
				// Short functions carry their body: the context pack can then
				// answer without a follow-up file read.
				if n := lineEnd - lineStart + 1; n > 0 && n <= 15 && lineEnd <= len(srcLines) {
					body := strings.Join(srcLines[lineStart-1:lineEnd], "\n")
					if len(body) <= 600 {
						attrs["body"] = body
					}
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
				if local := c.resolveLocalImport(path, importStr); local != "" {
					// Relative import of a file we index (code or data):
					// wire file → file so Blast can walk importers.
					g.AddEdge(fileID, "file:"+local, graph.EdgeImports, nil)
				} else {
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

			// ── Loose comment ────────────────────────────────────────────────
			// Deferred, not emitted here: `consumed` isn't complete until every
			// def/func match in this file has been processed.
			if cmt, ok := caps["loose.comment"]; ok {
				looseComments = append(looseComments, cmt)
			}

			// ── HTTP route (Express-style) ─────────────────────────────────────
			if _, ok := caps["route.call"]; ok {
				c.detectRoute(g, caps, relPath, fileID, data, srcLines, gen, &funcs)
			}

			// ── ORM model (layered, language-agnostic) ─────────────────────────
			// Call-shape detectors (Sequelize init/define + associations),
			// inheritance detectors (TS/JS class heritage, Python bases),
			// struct-tag detector (Go). Unknown ORMs: declare base classes in
			// .gatt/models.json or tag entities via annotate_entity model_table.
			if _, ok := caps["model.call"]; ok {
				c.detectModelInit(g, caps, relPath, fileID, data)
			}
			if _, ok := caps["modeldef.call"]; ok {
				c.detectModelDefine(g, caps, relPath, fileID, data)
			}
			if _, ok := caps["assoc.call"]; ok {
				c.detectAssoc(caps, relPath, data)
			}
			if _, ok := caps["clsmodel.class"]; ok {
				c.detectClassModel(g, caps, relPath, fileID, data)
			}
			if _, ok := caps["pymodel.class"]; ok {
				c.detectPyModel(g, caps, relPath, fileID, data)
			}
			if _, ok := caps["gomodel.struct"]; ok {
				c.detectGoModel(g, caps, relPath, fileID, data)
			}
			if _, ok := caps["fetch.call"]; ok {
				c.detectClientFetch(caps, relPath, data)
			}

			// ── Shared-state singleton tracking (JS/TS/JSX) ─────────────────────
			// Two binding shapes: ES `import { x } from '...'` and CommonJS
			// `const x = require('...')` — the latter is how most real
			// Express codebases actually import a config/state module.
			if srcNode, ok := caps["state.import_src"]; ok {
				if nameNode, ok := caps["state.import_name"]; ok {
					isRequire := caps["state.require_fn"] != nil
					if !isRequire || caps["state.require_fn"].Content(data) == "require" {
						spec := strings.Trim(srcNode.Content(data), `"'`)
						if local := c.resolveLocalImport(path, spec); local != "" {
							singletons[nameNode.Content(data)] = "file:" + local
						}
					}
				}
			}
			if _, ok := caps["state.write2"]; ok {
				objN, propN, subN := caps["state.write2_obj"], caps["state.write2_prop"], caps["state.write2_sub"]
				if objN != nil && propN != nil && subN != nil {
					stateAccesses = append(stateAccesses, stateAccessRaw{
						kind: "write", obj: objN.Content(data),
						prop: propN.Content(data) + "." + subN.Content(data),
						line: int(objN.StartPoint().Row) + 1,
					})
				}
			} else if writeNode, ok := caps["state.write"]; ok {
				objN, propN := caps["state.write_obj"], caps["state.write_prop"]
				if objN != nil && propN != nil {
					stateAccesses = append(stateAccesses, stateAccessRaw{
						kind: "write", obj: objN.Content(data), prop: propN.Content(data),
						line: int(writeNode.StartPoint().Row) + 1,
					})
				}
			} else if callNode, ok := caps["oa.call"]; ok {
				// Object.assign(x.prop, {...}) mutates x.prop in place —
				// tracked as a write, the same as a direct assignment.
				if oaObj, oaProp := caps["oa.obj"], caps["oa.prop"]; oaObj != nil && oaProp != nil &&
					oaObj.Content(data) == "Object" && oaProp.Content(data) == "assign" {
					objN, propN := caps["state.oa_target_obj"], caps["state.oa_target_prop"]
					if objN != nil && propN != nil {
						stateAccesses = append(stateAccesses, stateAccessRaw{
							kind: "write", obj: objN.Content(data), prop: propN.Content(data),
							line: int(callNode.StartPoint().Row) + 1,
						})
					}
				}
			} else if readNode, ok := caps["state.read"]; ok {
				objN, propN := caps["state.read_obj"], caps["state.read_prop"]
				if objN != nil && propN != nil && !isStateWriteLHS(readNode) && !isCallCallee(readNode) && !isObjectAssignTarget(readNode, data) {
					stateAccesses = append(stateAccesses, stateAccessRaw{
						kind: "read", obj: objN.Content(data), prop: propN.Content(data),
						line: int(readNode.StartPoint().Row) + 1,
					})
				}
			}
		}

		emitLooseComments(g, looseComments, consumed, funcs, relPath, fileID, data)
		emitStateAccess(g, stateAccesses, singletons, funcs, relPath)

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
		ID:    fileID,
		Type:  graph.NodeFile,
		Name:  relPath,
		Attrs: map[string]string{"path": path, "doc": "true", "mtime": mtime},
	})
	if projID != "" {
		g.AddEdge(fileID, projID, graph.EdgeBelongsTo, nil)
	}

	title := relPath
	var bodyLines []string
	var tokens []string
	inFence := false
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
		// Mine inline-code references (`funcName`, `path/to/file.ts`) for
		// MENTIONS edges. Fenced blocks are skipped: whole code samples
		// mention everything and mean nothing.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence || len(tokens) >= 100 {
			continue
		}
		for _, m := range inlineCodeRe.FindAllStringSubmatch(line, -1) {
			tok := strings.TrimSuffix(strings.TrimSpace(m[1]), "()")
			if len(tok) >= 3 && identLikeRe.MatchString(tok) && !isLikelyNonSymbolToken(tok) {
				tokens = append(tokens, tok)
			}
		}
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
	if len(tokens) > 0 {
		c.pendingMentions = append(c.pendingMentions, docMentions{docID: docID, tokens: tokens})
	}
}

var (
	inlineCodeRe = regexp.MustCompile("`([^`\n]{1,80})`")
	identLikeRe  = regexp.MustCompile(`^[A-Za-z_][\w./-]*$`)
)

// httpVerbTokens are backtick-quoted HTTP methods in prose ("call `PUT
// /users/:id`"), never a code symbol — excluded from mention mining.
var httpVerbTokens = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true,
	"PATCH": true, "HEAD": true, "OPTIONS": true,
}

// isLikelyNonSymbolToken reports whether an identifier-shaped backtick token
// is prose that was never meant to resolve to a graph entity — a directory
// path ("webApp/"), an HTTP verb, or a SCREAMING_SNAKE_CASE env var/constant
// literal — so mining doesn't manufacture "broken reference" noise out of
// it. This can't be perfect (a table name like `settings_app` or an external
// package like `otplib` looks identical to a real code symbol syntactically)
// — it only catches the mechanically-detectable cases.
func isLikelyNonSymbolToken(tok string) bool {
	if strings.HasSuffix(tok, "/") {
		return true
	}
	if httpVerbTokens[tok] {
		return true
	}
	if strings.Contains(tok, "_") && tok == strings.ToUpper(tok) {
		return true
	}
	return false
}

// wireMentions resolves the code tokens mined from markdown docs against the
// finished graph: paths match file nodes, names match definitions then
// functions. Each hit becomes a MENTIONS edge, and the doc node records the
// resolved set in mentions_resolved so doc-drift can later re-check whether
// those references still exist. Ambiguous names (>3 targets) are dropped.
func (c *Connector) wireMentions(g *graph.Graph) {
	if len(c.pendingMentions) == 0 {
		return
	}
	funcIDs := map[string][]string{}
	defIDs := map[string][]string{}
	fileByName := map[string]string{}
	for id, n := range g.Nodes {
		switch {
		case n.Type == graph.NodeFunction && n.Attrs["external"] != "true" && strings.HasPrefix(id, "func:"):
			funcIDs[n.Name] = append(funcIDs[n.Name], id)
		case n.Type == graph.NodeDefinition:
			defIDs[n.Name] = append(defIDs[n.Name], id)
		case n.Type == graph.NodeFile && !strings.HasPrefix(id, "doc:"):
			fileByName[n.Name] = id
		}
	}
	// methodOf indexes HAS_METHOD edges (defID + method name -> funcID) so a
	// "Class.method" doc token resolves to the actual method instead of
	// failing to match as either a file path or a flat name.
	methodOf := map[string]string{}
	for _, e := range g.Edges {
		if e.Type != graph.EdgeHasMethod {
			continue
		}
		if fn := g.Nodes[e.To]; fn != nil {
			methodOf[e.From+"\x00"+fn.Name] = e.To
		}
	}
	for _, dm := range c.pendingMentions {
		doc := g.Nodes[dm.docID]
		if doc == nil {
			continue
		}
		var resolved []string
		seen := map[string]bool{}
		for _, tok := range dm.tokens {
			if seen[tok] {
				continue
			}
			seen[tok] = true
			ids := resolveMention(tok, fileByName, defIDs, funcIDs, methodOf)
			if len(ids) == 0 || len(ids) > 3 {
				continue
			}
			for _, id := range ids {
				g.AddEdge(dm.docID, id, graph.EdgeMentions, nil)
			}
			resolved = append(resolved, tok)
		}
		if len(resolved) > 0 {
			sort.Strings(resolved)
			doc.Attrs["mentions_resolved"] = strings.Join(resolved, " ")
		}
	}
	c.pendingMentions = nil
}

// mineGitCoChanges adds CO_CHANGED edges between file nodes that carry no
// IMPORTS edge between them yet frequently change together in the same
// commit — the "you'll also have to touch these" signal for stylesheets,
// docs, e2e tests, and i18n bundles that no static edge would ever catch.
// Pure git history, so it only runs at full extract (see HasDrift/Update):
// there's no incremental delta to mine between refreshes.
func (c *Connector) mineGitCoChanges(ctx context.Context, g *graph.Graph) {
	if _, err := os.Stat(filepath.Join(c.dir, ".git")); err != nil {
		return
	}

	cmd := exec.CommandContext(ctx, "git", "-C", c.dir, "log", "--no-renames", "--name-only", "--pretty=format:%x00", "--max-count=2000")
	out, err := cmd.Output()
	if err != nil {
		return
	}

	// Pairs already linked by a static IMPORTS edge don't need a co-change
	// callout; the graph already explains why they move together.
	imported := map[[2]string]bool{}
	for _, e := range g.Edges {
		if e.Type != graph.EdgeImports {
			continue
		}
		from, to := g.Nodes[e.From], g.Nodes[e.To]
		if from == nil || to == nil || from.Type != graph.NodeFile || to.Type != graph.NodeFile {
			continue
		}
		imported[pairKey(e.From, e.To)] = true
	}

	pairCounts := map[[2]string]int{}
	fileCounts := map[string]int{}
	for _, commit := range strings.Split(string(out), "\x00") {
		seen := map[string]bool{}
		var files []string
		for _, l := range strings.Split(commit, "\n") {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			id := "file:" + filepath.Join(c.dir, l)
			if g.Nodes[id] == nil || seen[id] {
				continue
			}
			seen[id] = true
			files = append(files, id)
		}
		// Skip commits touching too few (no pair) or too many files (mass
		// reformats, vendor bumps): huge commits co-touch everything and
		// would swamp real signal with noise.
		if len(files) < 2 || len(files) > 20 {
			continue
		}
		sort.Strings(files)
		for _, f := range files {
			fileCounts[f]++
		}
		for i := 0; i < len(files); i++ {
			for j := i + 1; j < len(files); j++ {
				pairCounts[pairKey(files[i], files[j])]++
			}
		}
	}

	const minCount = 3
	for pair, cnt := range pairCounts {
		if cnt < minCount || imported[pair] {
			continue
		}
		a, b := pair[0], pair[1]
		total := fileCounts[a]
		if fileCounts[b] < total {
			total = fileCounts[b]
		}
		ratio := float64(cnt) / float64(total)
		confidence := "low"
		switch {
		case cnt >= 5 && ratio >= 0.7:
			confidence = "high"
		case ratio >= 0.4:
			confidence = "medium"
		}
		g.AddEdge(a, b, graph.EdgeCoChanged, map[string]string{
			"count":      fmt.Sprint(cnt),
			"confidence": confidence,
		})
	}
}

// pairKey returns a stable, order-independent key for a pair of node ids.
func pairKey(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

// resolveMention maps one doc token to graph node ids: path-looking tokens
// try file nodes (exact then suffix), a "Class.method" shape (single dot, no
// slash) tries the method via methodOf, bare names try definitions then
// functions. Shared by wireMentions and the doc-drift re-check.
func resolveMention(tok string, fileByName map[string]string, defIDs, funcIDs map[string][]string, methodOf map[string]string) []string {
	if strings.ContainsAny(tok, "/.") {
		if id := fileByName[tok]; id != "" {
			return []string{id}
		}
		var ids []string
		for name, id := range fileByName {
			if strings.HasSuffix(name, "/"+tok) {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			sort.Strings(ids)
			return ids
		}
		if !strings.Contains(tok, "/") {
			if left, right, ok := strings.Cut(tok, "."); ok && right != "" && !strings.Contains(right, ".") {
				if defs := defIDs[left]; len(defs) == 1 {
					if fid := methodOf[defs[0]+"\x00"+right]; fid != "" {
						return []string{fid}
					}
				}
			}
		}
	}
	if ids := defIDs[tok]; len(ids) > 0 {
		return ids
	}
	return funcIDs[tok]
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

// resolveCall applies the call-resolution rules to one raw call name (a "."
// prefix marks a selector call) and returns the local target node id, or ""
// when the call is external or ambiguous:
//   - selector calls resolve to same-file locals only (receiver methods live
//     next to their type; filepath.Join must never match a local Join)
//   - direct calls resolve to the unique global match, same-file tiebreak
func resolveCall(g *graph.Graph, fromFile, raw string, funcsByName map[string][]string) string {
	isSelector := strings.HasPrefix(raw, ".")
	bare := strings.TrimPrefix(raw, ".")
	targets := funcsByName[bare]
	sameFile := ""
	for _, tid := range targets {
		if tn := g.Nodes[tid]; tn != nil && tn.Attrs["file"] == fromFile {
			sameFile = tid
			break
		}
	}
	switch {
	case sameFile != "":
		return sameFile
	case isSelector:
		return ""
	case len(targets) == 1:
		return targets[0]
	default:
		return "" // ambiguous cross-file or unresolved
	}
}

// resolveAndWire does the second pass:
//  1. Resolves each call target to a local function node when possible.
//  2. Falls back to an external stub when not found locally.
//  3. Wires HAS_METHOD edges for cases missed during the first pass.
//
// It also persists each function's raw call names (calls_raw) so a later
// incremental Update can resolve calls from unchanged files against functions
// that did not exist yet at extract time.
// probeIndexable resolves an extension-less or extension-full import target to
// an existing indexable file, following TS/JS resolution (bare, .ts/.tsx,
// /index, .js → .ts). Returns "" when nothing matches.
func probeIndexable(base string) string {
	cands := []string{
		base, base + ".ts", base + ".tsx", base + ".js", base + ".jsx",
		filepath.Join(base, "index.ts"), filepath.Join(base, "index.tsx"),
		filepath.Join(base, "index.js"), filepath.Join(base, "index.jsx"),
	}
	if strings.HasSuffix(base, ".js") {
		cands = append(cands, strings.TrimSuffix(base, ".js")+".ts")
	}
	for _, p := range cands {
		if !indexableExt(p) {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// resolveLocalImport maps an import specifier from fromPath to the walked path
// of an existing indexable file, or "". Handles relative specifiers (./x,
// ../y/z.json) and tsconfig path aliases (@modules/x → src/modules/x).
func (c *Connector) resolveLocalImport(fromPath, spec string) string {
	if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
		return probeIndexable(filepath.Join(filepath.Dir(fromPath), spec))
	}
	for _, tc := range c.tsconfigs { // deepest dir first: innermost tsconfig wins
		if !strings.HasPrefix(fromPath, tc.dir+string(filepath.Separator)) {
			continue
		}
		for pat, targets := range tc.paths {
			var rest string
			if star := strings.IndexByte(pat, '*'); star >= 0 {
				prefix, suffix := pat[:star], pat[star+1:]
				if !strings.HasPrefix(spec, prefix) || !strings.HasSuffix(spec, suffix) {
					continue
				}
				rest = spec[len(prefix) : len(spec)-len(suffix)]
			} else if spec != pat {
				continue
			}
			for _, t := range targets {
				cand := strings.Replace(t, "*", rest, 1)
				if p := probeIndexable(filepath.Join(tc.dir, tc.baseURL, cand)); p != "" {
					return p
				}
			}
		}
	}
	return ""
}

// loadTSConfigs scans the tree for tsconfig.json files and records their
// baseUrl+paths alias mappings, deepest directory first. tsconfig is JSONC:
// line comments and trailing commas are stripped before parsing.
func (c *Connector) loadTSConfigs() {
	gitFiles, gitOK := c.gitFileSet()
	c.tsconfigs = []tsPathsConfig{}
	filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if graph.SkipDir(d.Name(), path == c.dir) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "tsconfig.json" {
			return nil
		}
		if rel, _ := filepath.Rel(c.dir, path); gitOK && !gitFiles[rel] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var raw struct {
			CompilerOptions struct {
				BaseURL string              `json:"baseUrl"`
				Paths   map[string][]string `json:"paths"`
			} `json:"compilerOptions"`
		}
		if json.Unmarshal(stripJSONC(data), &raw) != nil || len(raw.CompilerOptions.Paths) == 0 {
			return nil
		}
		c.tsconfigs = append(c.tsconfigs, tsPathsConfig{
			dir:     filepath.Dir(path),
			baseURL: raw.CompilerOptions.BaseURL,
			paths:   raw.CompilerOptions.Paths,
		})
		return nil
	})
	sort.Slice(c.tsconfigs, func(i, j int) bool {
		return len(c.tsconfigs[i].dir) > len(c.tsconfigs[j].dir)
	})
}

// stripJSONC removes // line comments and trailing commas so tsconfig-style
// JSONC parses with encoding/json. String-aware for the comment pass.
func stripJSONC(data []byte) []byte {
	var out []byte
	inStr, esc := false, false
	for i := 0; i < len(data); i++ {
		ch := data[i]
		if inStr {
			out = append(out, ch)
			if esc {
				esc = false
			} else if ch == '\\' {
				esc = true
			} else if ch == '"' {
				inStr = false
			}
			continue
		}
		if ch == '"' {
			inStr = true
			out = append(out, ch)
			continue
		}
		if ch == '/' && i+1 < len(data) && data[i+1] == '/' {
			for i < len(data) && data[i] != '\n' {
				i++
			}
			out = append(out, '\n')
			continue
		}
		if ch == ',' {
			j := i + 1
			for j < len(data) && (data[j] == ' ' || data[j] == '\t' || data[j] == '\n' || data[j] == '\r') {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue // trailing comma: drop
			}
		}
		out = append(out, ch)
	}
	return out
}

func resolveAndWire(g *graph.Graph, funcs []funcInfo, funcsByName map[string][]string) {
	for i := range funcs {
		fi := &funcs[i]

		if len(fi.calls) > 0 {
			if n := g.Nodes[fi.id]; n != nil {
				n.Attrs["calls_raw"] = strings.Join(fi.calls, " ")
			}
		}

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

			if target := resolveCall(g, fi.file, calledName, funcsByName); target != "" {
				g.AddEdge(fi.id, target, graph.EdgeCalls, nil)
				continue
			}
			bareName := strings.TrimPrefix(calledName, ".")
			if strings.HasPrefix(calledName, ".") || builtinNames[bareName] {
				// External selector (stdlib/method) or language builtin:
				// stubs for these are pure noise in the graph.
				continue
			}
			if len(funcsByName[bareName]) > 1 {
				// Ambiguous cross-file name (e.g. dozens of "append" defs in a
				// monorepo): wiring to all targets creates quadratic false
				// edges. Skip — better no edge than thousands of wrong ones.
				continue
			}
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
