package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Node types. Built-in constants cover SQL databases, OpenAPI specs, codebases,
// and document collections; external consumers may also use arbitrary custom
// node types (e.g. "customer", "contract", "issue").
const (
	NodeDatabase = "database"
	NodeTable    = "table"
	NodeColumn   = "column"
	NodeView     = "view"
	NodeIndex    = "index"

	NodeAPI      = "api"      // root of an API spec (analogous to database)
	NodeSchema   = "schema"   // a component schema / model (analogous to table)
	NodeProperty = "property" // a schema property (analogous to column)
	NodeEndpoint = "endpoint" // an HTTP operation, e.g. "GET /users/{id}"

	NodeDefinition = "definition"
	NodeFeature    = "feature"
	NodeComponent  = "component"
	NodeProject    = "project"
	NodeFile       = "file"
	NodeFunction   = "function"

	NodeRoute   = "route"   // an HTTP route registration found in code (e.g. Express router.get)
	NodeComment = "comment" // a floating comment not attached to any declaration's doc
	NodeState   = "state"   // a tracked property slot on a shared cross-file singleton
	NodeModel   = "model"   // an ORM model found in code (e.g. Sequelize Model.init / sequelize.define)
	NodePackage = "package" // a resolved local package/namespace directory (Go, Python, Java, Kotlin, C#)

	NodeDocument = "document" // a standalone document file (PDF, DOCX, Markdown, TXT)
	NodeSection  = "section"  // a heading/page section inside a document
	NodeChunk    = "chunk"    // a retrieval-sized text chunk inside a section
)

// Edge types. External consumers may also use arbitrary custom edge types.
const (
	EdgeDependsOn     = "DEPENDS_ON"
	EdgeBelongsTo     = "BELONGS_TO"
	EdgeCalls         = "CALLS"
	EdgeImports       = "IMPORTS"
	EdgeHasMethod     = "HAS_METHOD"     // definition -> function (type owns method)
	EdgeGenerates     = "GENERATES"      // file -> file (source regenerates target; overlay-declared)
	EdgeCoChanged     = "CO_CHANGED"     // file -> file (frequently change in the same git commit)
	EdgeMentions      = "MENTIONS"       // doc -> function/definition/file it references
	EdgeCallsEndpoint = "CALLS_ENDPOINT" // function (client call site) -> route it hits over HTTP
	EdgeUsesModel     = "USES_MODEL"     // route -> ORM model its handler chain touches
	EdgeUsesStyle     = "USES_STYLE"     // file (template/JSX) -> css file whose classes it uses

	EdgeHandledBy      = "HANDLED_BY"      // route -> function (the terminal handler)
	EdgeUsesMiddleware = "USES_MIDDLEWARE" // route -> function (middleware in chain order)
	EdgeReadsState     = "READS"           // function -> state
	EdgeWritesState    = "WRITES"          // function -> state

	EdgeHasTable   = "HAS_TABLE"
	EdgeHasColumn  = "HAS_COLUMN"
	EdgeHasIndex   = "HAS_INDEX"
	EdgeIndexes    = "INDEXES"
	EdgeForeignKey = "FOREIGN_KEY" // column -> column
	EdgeReferences = "REFERENCES"  // table -> table / schema -> schema (derived), or view -> table

	EdgeHasSchema    = "HAS_SCHEMA"    // api -> schema
	EdgeHasProperty  = "HAS_PROPERTY"  // schema -> property
	EdgeHasEndpoint  = "HAS_ENDPOINT"  // api -> endpoint
	EdgeRefersTo     = "REFERS_TO"     // property -> schema (a $ref, analogous to FOREIGN_KEY)
	EdgeAccepts      = "ACCEPTS"       // endpoint -> schema (request body)
	EdgeRespondsWith = "RESPONDS_WITH" // endpoint -> schema (response)

	EdgeHasSection = "HAS_SECTION" // document -> section
	EdgeHasChunk   = "HAS_CHUNK"   // section -> chunk
	EdgeNext       = "NEXT"        // section -> section or chunk -> chunk (reading order)
)

// Node is one typed entity in the graph.
type Node struct {
	ID    string            `json:"id"`
	Type  string            `json:"type"`
	Name  string            `json:"name"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Edge is a directed typed relationship from From to To.
type Edge struct {
	From  string            `json:"from"`
	To    string            `json:"to"`
	Type  string            `json:"type"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Graph holds nodes, edges, provenance metadata, and an optional lazy adjacency
// index and mutation journal.
type Graph struct {
	Source string `json:"source"` // e.g. "sqlite:/path/to/db", "docs:/path", "enterprise:acme"
	// ExtractedAt records when the metadata was pulled from the source, so
	// callers can judge how stale the answer is. Zero when unknown.
	ExtractedAt time.Time        `json:"extracted_at,omitzero"`
	Nodes       map[string]*Node `json:"nodes"`
	Edges       []Edge           `json:"edges"`

	// NodeTextFunc optionally overrides Graph.NodeText for custom domains.
	// When nil, NodeText uses the built-in renderer (which also indexes custom
	// attributes and edges deterministically for non-built-in node types).
	NodeTextFunc func(g *Graph, n *Node) string `json:"-"`

	adjMu sync.RWMutex
	adj   map[string][]int // node id -> edge indexes (both directions), built lazily

	journal *journal // non-nil while tracking mutations for delta saves
}

// journal records mutations since StartJournal so a SQLite save can write
// only the delta instead of rewriting every row. JSON saves ignore it.
type journal struct {
	removedNodes map[string]bool
	addedNodes   map[string]bool
	addedEdges   []Edge
}

// StartJournal begins mutation tracking. Call it right after loading a graph
// that will be incrementally updated and saved to SQLite.
func (g *Graph) StartJournal() {
	g.journal = &journal{removedNodes: map[string]bool{}, addedNodes: map[string]bool{}}
}

// JournalAddedNodeIDs returns the node ids added since StartJournal — the set
// an incremental re-embed targets. Read it before Save (a SQLite save resets
// the journal). Nil when no journal is active.
func (g *Graph) JournalAddedNodeIDs() []string {
	if g.journal == nil {
		return nil
	}
	ids := make([]string, 0, len(g.journal.addedNodes))
	for id := range g.journal.addedNodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// New creates an empty graph tagged with source.
func New(source string) *Graph {
	return &Graph{Source: source, Nodes: map[string]*Node{}}
}

// Dialect returns the SQL dialect implied by the source ("<kind>:<detail>").
func (g *Graph) Dialect() string {
	kind := g.Source
	if i := strings.IndexByte(kind, ':'); i >= 0 {
		kind = kind[:i]
	}
	switch kind {
	case "postgres", "postgresql":
		return "postgresql"
	case "sqlite":
		return "sqlite"
	default:
		return kind
	}
}

// IsAPI reports whether this graph describes an API spec (OpenAPI/FastAPI).
func (g *Graph) IsAPI() bool { return strings.HasPrefix(g.Source, "openapi:") }

// IsDocs reports whether this graph describes a document collection.
func (g *Graph) IsDocs() bool { return strings.HasPrefix(g.Source, "docs:") }

// Freshness returns a one-line provenance string, e.g.
// "postgres:app, extracted 3h ago (2026-07-13)". Empty when ExtractedAt is zero.
func (g *Graph) Freshness(now time.Time) string {
	if g.ExtractedAt.IsZero() {
		return ""
	}
	d := now.Sub(g.ExtractedAt)
	var age string
	switch {
	case d < time.Minute:
		age = "just now"
	case d < time.Hour:
		age = fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		age = fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		age = fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return fmt.Sprintf("%s, extracted %s (%s)", g.Source, age, g.ExtractedAt.Format("2006-01-02"))
}

// AddNode inserts or replaces a node by ID.
func (g *Graph) AddNode(n *Node) {
	if n.Attrs == nil {
		n.Attrs = map[string]string{}
	}
	g.Nodes[n.ID] = n // adj indexes edges only, so a new node never invalidates it
	if g.journal != nil {
		g.journal.addedNodes[n.ID] = true
	}
}

// AddEdge appends a directed edge from -> to with type typ and optional attrs.
func (g *Graph) AddEdge(from, to, typ string, attrs map[string]string) {
	e := Edge{From: from, To: to, Type: typ, Attrs: attrs}
	g.adjMu.Lock()
	g.Edges = append(g.Edges, e)
	// Keep a built adjacency index in sync instead of dropping it: callers that
	// interleave EdgesOf with AddEdge would otherwise rebuild the whole index
	// per call, which is quadratic on large graphs.
	if g.adj != nil {
		i := len(g.Edges) - 1
		g.adj[from] = append(g.adj[from], i)
		if to != from { // self-loop recorded once, as in buildAdj
			g.adj[to] = append(g.adj[to], i)
		}
	}
	g.adjMu.Unlock()
	if g.journal != nil {
		g.journal.addedEdges = append(g.journal.addedEdges, e)
	}
}

// RemoveNodesWhere deletes every node matching pred plus all edges touching a
// removed node, returning how many nodes were removed.
func (g *Graph) RemoveNodesWhere(pred func(*Node) bool) int {
	removed := map[string]bool{}
	for id, n := range g.Nodes {
		if pred(n) {
			removed[id] = true
			delete(g.Nodes, id)
		}
	}
	if len(removed) == 0 {
		return 0
	}
	if g.journal != nil {
		for id := range removed {
			g.journal.removedNodes[id] = true
			delete(g.journal.addedNodes, id)
		}
	}
	g.adjMu.Lock()
	kept := g.Edges[:0]
	for _, e := range g.Edges {
		if !removed[e.From] && !removed[e.To] {
			kept = append(kept, e)
		}
	}
	g.Edges = kept
	g.adj = nil
	g.adjMu.Unlock()
	return len(removed)
}

func (g *Graph) buildAdj() map[string][]int {
	g.adjMu.RLock()
	adj := g.adj
	g.adjMu.RUnlock()
	if adj != nil {
		return adj
	}
	g.adjMu.Lock()
	defer g.adjMu.Unlock()
	if g.adj != nil {
		return g.adj
	}
	adj = make(map[string][]int, len(g.Nodes))
	for i, e := range g.Edges {
		adj[e.From] = append(adj[e.From], i)
		// A self-loop must be recorded once, not twice, or EdgesOf returns it twice.
		if e.To != e.From {
			adj[e.To] = append(adj[e.To], i)
		}
	}
	g.adj = adj
	return adj
}

// EdgesOf returns all edges touching the node, in both directions.
func (g *Graph) EdgesOf(id string) []Edge {
	adj := g.buildAdj()
	g.adjMu.RLock()
	idxs := adj[id]
	out := make([]Edge, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, g.Edges[i])
	}
	g.adjMu.RUnlock()
	return out
}

// OutEdges returns all directed edges leaving id (e.From == id).
func (g *Graph) OutEdges(id string) []Edge {
	adj := g.buildAdj()
	g.adjMu.RLock()
	var out []Edge
	for _, i := range adj[id] {
		if e := g.Edges[i]; e.From == id {
			out = append(out, e)
		}
	}
	g.adjMu.RUnlock()
	return out
}

// InEdges returns all directed edges entering id (e.To == id).
func (g *Graph) InEdges(id string) []Edge {
	adj := g.buildAdj()
	g.adjMu.RLock()
	var out []Edge
	for _, i := range adj[id] {
		if e := g.Edges[i]; e.To == id {
			out = append(out, e)
		}
	}
	g.adjMu.RUnlock()
	return out
}

// ShortestPath finds the shortest undirected path between two nodes using
// only the given edge types (all types if none given). Returns the edges
// along the path, or nil if unreachable.
func (g *Graph) ShortestPath(from, to string, edgeTypes ...string) []Edge {
	return g.FindPath(from, to, TraverseOptions{
		EdgeTypes: edgeTypes,
		Direction: DirectionBoth,
	})
}

// CheapestPath is Dijkstra over undirected edges of the given types, where
// passing *through* an intermediate node costs nodeCost(id) on top of the
// hop. Endpoints are free. Used to route join paths around hub nodes
// (e.g. tenant_id tables or company-wide departments) which BFS would cut through.
func (g *Graph) CheapestPath(from, to string, nodeCost func(id string) float64, edgeTypes ...string) []Edge {
	var costFn func(*Node) float64
	if nodeCost != nil {
		costFn = func(n *Node) float64 { return nodeCost(n.ID) }
	}
	return g.FindPath(from, to, TraverseOptions{
		EdgeTypes: edgeTypes,
		Direction: DirectionBoth,
		NodeCost:  costFn,
	})
}

// NodesByType returns nodes of the given type sorted by name (then ID).
func (g *Graph) NodesByType(typ string) []*Node {
	var out []*Node
	for _, n := range g.Nodes {
		if n.Type == typ {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

var builtinNodeTypes = map[string]bool{
	NodeDatabase: true, NodeTable: true, NodeColumn: true, NodeView: true, NodeIndex: true,
	NodeAPI: true, NodeSchema: true, NodeProperty: true, NodeEndpoint: true,
	NodeDefinition: true, NodeFeature: true, NodeComponent: true, NodeProject: true,
	NodeFile: true, NodeFunction: true, NodeRoute: true, NodeComment: true,
	NodeState: true, NodeModel: true, NodePackage: true,
	NodeDocument: true, NodeSection: true, NodeChunk: true,
}

var builtinEdgeTypes = map[string]bool{
	EdgeDependsOn: true, EdgeBelongsTo: true, EdgeCalls: true, EdgeImports: true,
	EdgeHasMethod: true, EdgeGenerates: true, EdgeCoChanged: true, EdgeMentions: true,
	EdgeCallsEndpoint: true, EdgeUsesModel: true, EdgeUsesStyle: true,
	EdgeHandledBy: true, EdgeUsesMiddleware: true, EdgeReadsState: true, EdgeWritesState: true,
	EdgeHasTable: true, EdgeHasColumn: true, EdgeHasIndex: true, EdgeIndexes: true,
	EdgeForeignKey: true, EdgeReferences: true,
	EdgeHasSchema: true, EdgeHasProperty: true, EdgeHasEndpoint: true,
	EdgeRefersTo: true, EdgeAccepts: true, EdgeRespondsWith: true,
	EdgeHasSection: true, EdgeHasChunk: true, EdgeNext: true,
}

// NodeText builds the natural-language description of a node used for
// embedding and FTS5 keyword search: name, type, attributes and immediate
// relationships. Set g.NodeTextFunc to override this behavior completely.
func (g *Graph) NodeText(id string) string {
	n := g.Nodes[id]
	if n == nil {
		return ""
	}
	if g.NodeTextFunc != nil {
		return g.NodeTextFunc(g, n)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s.", n.Type, n.Name)
	if c := n.Attrs["comment"]; c != "" {
		fmt.Fprintf(&b, " %s.", c)
	}
	if en := n.Attrs["entity_note"]; en != "" {
		fmt.Fprintf(&b, " %s.", en)
	}
	if ev := n.Attrs["enum_values"]; ev != "" {
		fmt.Fprintf(&b, " Allowed values: %s.", ev)
	}
	if dt := n.Attrs["data_type"]; dt != "" {
		fmt.Fprintf(&b, " Type %s.", dt)
	}
	// Codebase-specific enrichment: signature, file location, doc content.
	if sig := n.Attrs["signature"]; sig != "" {
		fmt.Fprintf(&b, " Signature: %s.", sig)
	}
	if f := n.Attrs["file"]; f != "" {
		fmt.Fprintf(&b, " In file %s.", f)
	}
	if doc := n.Attrs["doc"]; doc != "" {
		fmt.Fprintf(&b, " %s", doc)
	}
	if body := n.Attrs["doc_body"]; body != "" {
		// Include up to 300 chars of markdown body for semantic indexing.
		if len(body) > 300 {
			body = body[:300]
		}
		fmt.Fprintf(&b, " %s", body)
	}
	if n.Type == NodeRoute {
		if m, p := n.Attrs["method"], n.Attrs["path"]; m != "" || p != "" {
			fmt.Fprintf(&b, " %s %s.", m, p)
		}
	}
	if n.Type == NodeComment {
		if t := n.Attrs["text"]; t != "" {
			fmt.Fprintf(&b, " %s", t)
		}
	}
	if n.Type == NodeModel {
		if t := n.Attrs["table"]; t != "" {
			fmt.Fprintf(&b, " table %s.", t)
		}
		if f := n.Attrs["fields"]; f != "" {
			fmt.Fprintf(&b, " Fields: %s.", f)
		}
	}
	if n.Type == NodeDocument || n.Type == NodeSection || n.Type == NodeChunk {
		if title := n.Attrs["title"]; title != "" && title != n.Name {
			fmt.Fprintf(&b, " Title: %s.", title)
		}
		if sec := n.Attrs["section"]; sec != "" && sec != n.Name {
			fmt.Fprintf(&b, " Section: %s.", sec)
		}
		// Enterprise/context metadata on documents & chunks (e.g. folder, summary, tags, year, department).
		for _, k := range []string{"folder", "department", "year", "summary", "tags", "author"} {
			if v := n.Attrs[k]; v != "" {
				fmt.Fprintf(&b, " %s: %s.", k, v)
			}
		}
		if t := n.Attrs["text"]; t != "" {
			fmt.Fprintf(&b, " %s", t)
		}
	}
	// Generic attribute rendering for custom domain node types (e.g. customer,
	// contract, employee, issue, department) so FTS5 and embeddings index them
	// automatically without requiring a custom NodeTextFunc.
	if !builtinNodeTypes[n.Type] && len(n.Attrs) > 0 {
		handled := map[string]bool{
			"comment": true, "entity_note": true, "enum_values": true,
			"data_type": true, "signature": true, "file": true,
			"doc": true, "doc_body": true,
			"mtime": true, "sha256": true,
		}
		var keys []string
		for k, v := range n.Attrs {
			if v == "" || handled[k] {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, " %s: %s.", k, n.Attrs[k])
		}
	}

	var cols, refs, refBy, accepts, returns, usedBy, methods, calledBy []string
	var handledBy, middleware, writesState, readsState, writtenBy, readBy []string
	var callsEndpoints, usesModels, usedByRoutes []string
	customOut := map[string][]string{}
	customIn := map[string][]string{}

	for _, e := range g.EdgesOf(id) {
		other := e.To
		if other == id {
			other = e.From
		}
		on := g.Nodes[other]
		if on == nil {
			continue
		}
		switch {
		case (e.Type == EdgeHasColumn || e.Type == EdgeHasProperty) && e.From == id:
			cols = append(cols, on.Name)
		case e.Type == EdgeReferences && e.From == id:
			refs = append(refs, on.Name)
		case e.Type == EdgeReferences && e.To == id:
			refBy = append(refBy, on.Name)
		case e.Type == EdgeAccepts && e.From == id:
			accepts = append(accepts, on.Name)
		case e.Type == EdgeRespondsWith && e.From == id:
			returns = append(returns, on.Name)
		case (e.Type == EdgeAccepts || e.Type == EdgeRespondsWith) && e.To == id:
			usedBy = append(usedBy, on.Name)
		case e.Type == EdgeHasMethod && e.From == id:
			methods = append(methods, on.Name)
		case e.Type == EdgeCalls && e.To == id:
			calledBy = append(calledBy, on.Name)
		case e.Type == EdgeHandledBy && e.From == id:
			handledBy = append(handledBy, on.Name)
		case e.Type == EdgeUsesMiddleware && e.From == id:
			middleware = append(middleware, on.Name)
		case e.Type == EdgeWritesState && e.From == id:
			writesState = append(writesState, on.Name)
		case e.Type == EdgeReadsState && e.From == id:
			readsState = append(readsState, on.Name)
		case e.Type == EdgeWritesState && e.To == id:
			writtenBy = append(writtenBy, on.Name)
		case e.Type == EdgeReadsState && e.To == id:
			readBy = append(readBy, on.Name)
		case e.Type == EdgeCallsEndpoint && e.From == id:
			callsEndpoints = append(callsEndpoints, on.Name)
		case e.Type == EdgeUsesModel && e.From == id:
			usesModels = append(usesModels, on.Name)
		case e.Type == EdgeUsesModel && e.To == id:
			usedByRoutes = append(usedByRoutes, on.Name)
		default:
			if !builtinEdgeTypes[e.Type] || !builtinNodeTypes[n.Type] {
				if e.From == id {
					customOut[e.Type] = append(customOut[e.Type], on.Name)
				} else {
					customIn[e.Type] = append(customIn[e.Type], on.Name)
				}
			}
		}
	}
	// Sort set-like lists so identical content produces identical text regardless
	// of edge insertion order (preserves embedding cache hashes).
	for _, l := range [][]string{refs, refBy, usedBy, methods, calledBy, handledBy, writesState, readsState, writtenBy, readBy, callsEndpoints, usesModels, usedByRoutes} {
		sort.Strings(l)
	}
	if len(cols) > 0 {
		label := "Columns"
		if n.Type == NodeSchema {
			label = "Properties"
		}
		fmt.Fprintf(&b, " %s: %s.", label, strings.Join(cols, ", "))
	}
	if len(methods) > 0 {
		fmt.Fprintf(&b, " Methods: %s.", strings.Join(methods, ", "))
	}
	if len(calledBy) > 0 {
		fmt.Fprintf(&b, " Called by: %s.", strings.Join(calledBy, ", "))
	}
	if len(callsEndpoints) > 0 {
		fmt.Fprintf(&b, " Calls backend routes: %s.", strings.Join(callsEndpoints, ", "))
	}
	if len(usesModels) > 0 {
		fmt.Fprintf(&b, " Touches models: %s.", strings.Join(usesModels, ", "))
	}
	if len(usedByRoutes) > 0 {
		fmt.Fprintf(&b, " Used by routes: %s.", strings.Join(usedByRoutes, ", "))
	}
	if len(handledBy) > 0 {
		fmt.Fprintf(&b, " Handled by: %s.", strings.Join(handledBy, ", "))
	}
	if len(middleware) > 0 {
		fmt.Fprintf(&b, " Middleware: %s.", strings.Join(middleware, ", "))
	}
	if len(writesState) > 0 {
		fmt.Fprintf(&b, " Writes: %s.", strings.Join(writesState, ", "))
	}
	if len(readsState) > 0 {
		fmt.Fprintf(&b, " Reads: %s.", strings.Join(readsState, ", "))
	}
	if len(writtenBy) > 0 {
		fmt.Fprintf(&b, " Written by: %s.", strings.Join(writtenBy, ", "))
	}
	if len(readBy) > 0 {
		fmt.Fprintf(&b, " Read by: %s.", strings.Join(readBy, ", "))
	}
	if len(accepts) > 0 {
		fmt.Fprintf(&b, " Accepts: %s.", strings.Join(accepts, ", "))
	}
	if len(returns) > 0 {
		fmt.Fprintf(&b, " Returns: %s.", strings.Join(returns, ", "))
	}
	if len(refs) > 0 {
		fmt.Fprintf(&b, " References: %s.", strings.Join(refs, ", "))
	}
	if len(refBy) > 0 {
		fmt.Fprintf(&b, " Referenced by: %s.", strings.Join(refBy, ", "))
	}
	if len(usedBy) > 0 {
		fmt.Fprintf(&b, " Used by: %s.", strings.Join(usedBy, ", "))
	}
	if len(customOut) > 0 {
		types := make([]string, 0, len(customOut))
		for t := range customOut {
			types = append(types, t)
		}
		sort.Strings(types)
		for _, t := range types {
			names := customOut[t]
			sort.Strings(names)
			fmt.Fprintf(&b, " %s: %s.", t, strings.Join(names, ", "))
		}
	}
	if len(customIn) > 0 {
		types := make([]string, 0, len(customIn))
		for t := range customIn {
			types = append(types, t)
		}
		sort.Strings(types)
		for _, t := range types {
			names := customIn[t]
			sort.Strings(names)
			fmt.Fprintf(&b, " %s from: %s.", t, strings.Join(names, ", "))
		}
	}
	return b.String()
}

// Save persists the graph to path. Extensions .db, .sqlite, and .sqlite3 write
// SQLite (with FTS5 and optional journaled delta); all other extensions write JSON.
func (g *Graph) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if IsSQLitePath(path) {
		return g.saveSQLite(path)
	}
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Load reads a graph from path (SQLite or JSON) and merges the sibling
// annotations.json sidecar if present.
func Load(path string) (*Graph, error) {
	g, err := LoadRaw(path)
	if err != nil {
		return nil, err
	}
	if err := g.applyAnnotations(filepath.Join(filepath.Dir(path), AnnotationsFile)); err != nil {
		return nil, err
	}
	return g, nil
}

// LoadRaw loads the graph exactly as stored, without merging the curated
// annotations sidecar. Diffing two extractions must compare raw graphs, or
// annotated attributes would show up as spurious source changes.
func LoadRaw(path string) (*Graph, error) {
	if IsSQLitePath(path) {
		return loadSQLite(path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var g Graph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, err
	}
	if g.Nodes == nil {
		g.Nodes = map[string]*Node{}
	}
	return &g, nil
}

// AnnotationsFile is the sidecar, relative to the graph file, holding curated
// per-node attribute overrides: {"<node-id>": {"<attr>": "<value>", ...}}.
const AnnotationsFile = "annotations.json"

// LoadAnnotations reads the curated overrides map from path, returning an
// empty map (not an error) when the file does not exist.
func LoadAnnotations(path string) (map[string]map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	ann := map[string]map[string]string{}
	if err := json.Unmarshal(data, &ann); err != nil {
		return nil, fmt.Errorf("annotations %s: %w", path, err)
	}
	return ann, nil
}

// SetAnnotation updates the annotations sidecar at annPath for one node id.
// With clear, the node's annotations are removed entirely; otherwise each
// key in set is applied (an empty value deletes that single key).
func SetAnnotation(annPath, id string, set map[string]string, clear bool) (map[string]string, error) {
	ann, err := LoadAnnotations(annPath)
	if err != nil {
		return nil, err
	}
	if clear {
		delete(ann, id)
	} else {
		if ann[id] == nil {
			ann[id] = map[string]string{}
		}
		for k, v := range set {
			if v == "" {
				delete(ann[id], k)
			} else {
				ann[id][k] = v
			}
		}
		if len(ann[id]) == 0 {
			delete(ann, id)
		}
	}
	result := ann[id]
	data, err := json.MarshalIndent(ann, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(annPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(annPath, data, 0o644); err != nil {
		return nil, err
	}
	return result, nil
}

func (g *Graph) applyAnnotations(path string) error {
	ann, err := LoadAnnotations(path)
	if err != nil {
		return err
	}
	for id, attrs := range ann {
		n := g.Nodes[id]
		if n == nil {
			n = g.ResolveRelinkedFunc(id)
		}
		if n == nil {
			continue
		}
		if n.Attrs == nil {
			n.Attrs = map[string]string{}
		}
		maps.Copy(n.Attrs, attrs)
	}
	return nil
}

// ResolveRelinkedFunc re-resolves a stale function-node annotation id whose
// embedded line number shifted ("func:<file>:<name>:<line>") by looking up the
// unique live function with the same file and name.
func (g *Graph) ResolveRelinkedFunc(id string) *Node {
	parts := strings.Split(id, ":")
	if len(parts) != 4 || parts[0] != "func" {
		return nil
	}
	file, name := parts[1], parts[2]
	var match *Node
	for _, n := range g.Nodes {
		if n.Type != NodeFunction || n.Attrs["external"] == "true" {
			continue
		}
		if n.Attrs["file"] == file && n.Name == name {
			if match != nil {
				return nil
			}
			match = n
		}
	}
	return match
}
