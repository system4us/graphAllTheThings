// Package graph defines the semantic metadata graph: typed nodes and edges
// extracted from a source (database schema, OpenAPI spec, ...), with
// traversal helpers used by the MCP tools.
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
	"time"
)

// Node types. The first group models a SQL source; the second models an API
// spec (OpenAPI/FastAPI). Both live in the same graph shape so the engine's
// traversal, search and join machinery is shared.
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
)

// Edge types.
const (
	EdgeDependsOn  = "DEPENDS_ON"
	EdgeBelongsTo  = "BELONGS_TO"
	EdgeCalls      = "CALLS"
	EdgeImports    = "IMPORTS"
	EdgeHasMethod  = "HAS_METHOD" // definition -> function (type owns method)

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
)

type Node struct {
	ID    string            `json:"id"`
	Type  string            `json:"type"`
	Name  string            `json:"name"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

type Edge struct {
	From  string            `json:"from"`
	To    string            `json:"to"`
	Type  string            `json:"type"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

type Graph struct {
	Source string `json:"source"` // e.g. "sqlite:/path/to/db"
	// ExtractedAt records when the metadata was pulled from the source, so
	// agents can judge how stale the answer is. Zero when unknown (older
	// graphs, hand-built graphs).
	ExtractedAt time.Time        `json:"extracted_at,omitzero"`
	Nodes       map[string]*Node `json:"nodes"`
	Edges       []Edge           `json:"edges"`

	adj map[string][]int // node id -> edge indexes (both directions), built lazily

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
	return ids
}

func New(source string) *Graph {
	return &Graph{Source: source, Nodes: map[string]*Node{}}
}

// Dialect returns the SQL dialect implied by the source, so agents write
// dialect-correct SQL instead of inferring it from column types. Source is
// "<kind>:<detail>" (e.g. "postgres:app", "sqlite:/path"); the kind maps to
// its canonical dialect name, or "" when unknown.
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

// IsAPI reports whether this graph describes an API spec (OpenAPI/FastAPI)
// rather than a database. The engine uses it to switch its container node type
// (schema vs table), rendering, and join semantics ($ref chain vs FK JOIN).
func (g *Graph) IsAPI() bool { return strings.HasPrefix(g.Source, "openapi:") }

// Freshness returns a one-line provenance string for agent output, e.g.
// "postgres:app, extracted 3h ago (2026-07-13)". Empty when no extraction
// time was recorded. Agents use it to decide whether to trust the graph or
// re-verify against the live source.
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

func (g *Graph) AddNode(n *Node) {
	if n.Attrs == nil {
		n.Attrs = map[string]string{}
	}
	g.Nodes[n.ID] = n
	g.adj = nil
	if g.journal != nil {
		g.journal.addedNodes[n.ID] = true
	}
}

func (g *Graph) AddEdge(from, to, typ string, attrs map[string]string) {
	e := Edge{From: from, To: to, Type: typ, Attrs: attrs}
	g.Edges = append(g.Edges, e)
	g.adj = nil
	if g.journal != nil {
		g.journal.addedEdges = append(g.journal.addedEdges, e)
	}
}

// RemoveNodesWhere deletes every node matching pred plus all edges touching a
// removed node, returning how many nodes were removed. Used by incremental
// refresh to evict entities of changed/deleted files before re-parsing.
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
	kept := g.Edges[:0]
	for _, e := range g.Edges {
		if !removed[e.From] && !removed[e.To] {
			kept = append(kept, e)
		}
	}
	g.Edges = kept
	g.adj = nil
	return len(removed)
}

func (g *Graph) buildAdj() {
	if g.adj != nil {
		return
	}
	g.adj = map[string][]int{}
	for i, e := range g.Edges {
		g.adj[e.From] = append(g.adj[e.From], i)
		// A self-loop (self-referential FK, or a schema whose property $refs
		// itself) must be recorded once, not twice, or EdgesOf returns it twice.
		if e.To != e.From {
			g.adj[e.To] = append(g.adj[e.To], i)
		}
	}
}

// EdgesOf returns all edges touching the node, in both directions.
func (g *Graph) EdgesOf(id string) []Edge {
	g.buildAdj()
	var out []Edge
	for _, i := range g.adj[id] {
		out = append(out, g.Edges[i])
	}
	return out
}

// ShortestPath finds the shortest undirected path between two nodes using
// only the given edge types (all types if none given). Returns the edges
// along the path, or nil if unreachable.
func (g *Graph) ShortestPath(from, to string, edgeTypes ...string) []Edge {
	g.buildAdj()
	if g.Nodes[from] == nil || g.Nodes[to] == nil {
		return nil
	}
	allowed := map[string]bool{}
	for _, t := range edgeTypes {
		allowed[t] = true
	}
	type step struct {
		node string
		edge int // edge index used to arrive, -1 for start
		prev int // index into visitedOrder
	}
	order := []step{{from, -1, -1}}
	seen := map[string]bool{from: true}
	for qi := 0; qi < len(order); qi++ {
		cur := order[qi]
		if cur.node == to {
			var path []Edge
			for s := cur; s.edge >= 0; s = order[s.prev] {
				path = append([]Edge{g.Edges[s.edge]}, path...)
			}
			return path
		}
		for _, ei := range g.adj[cur.node] {
			e := g.Edges[ei]
			if len(allowed) > 0 && !allowed[e.Type] {
				continue
			}
			next := e.To
			if next == cur.node {
				next = e.From
			}
			if !seen[next] {
				seen[next] = true
				order = append(order, step{next, ei, qi})
			}
		}
	}
	return nil
}

// CheapestPath is Dijkstra over undirected edges of the given types, where
// passing *through* an intermediate node costs nodeCost(id) on top of the
// hop. Endpoints are free. Used to route join paths around hub tables
// (tenant_id-style FKs that every table carries) which BFS would happily
// cut through.
func (g *Graph) CheapestPath(from, to string, nodeCost func(id string) float64, edgeTypes ...string) []Edge {
	g.buildAdj()
	if g.Nodes[from] == nil || g.Nodes[to] == nil {
		return nil
	}
	allowed := map[string]bool{}
	for _, t := range edgeTypes {
		allowed[t] = true
	}
	dist := map[string]float64{from: 0}
	prevEdge := map[string]int{}
	prevNode := map[string]string{}
	done := map[string]bool{}
	for {
		cur, best := "", 0.0
		for id, d := range dist {
			if !done[id] && (cur == "" || d < best) {
				cur, best = id, d
			}
		}
		if cur == "" || cur == to {
			break
		}
		done[cur] = true
		for _, ei := range g.adj[cur] {
			e := g.Edges[ei]
			if len(allowed) > 0 && !allowed[e.Type] {
				continue
			}
			next := e.To
			if next == cur {
				next = e.From
			}
			cost := best + 1
			if next != to {
				cost += nodeCost(next)
			}
			if d, ok := dist[next]; !ok || cost < d {
				dist[next] = cost
				prevEdge[next] = ei
				prevNode[next] = cur
			}
		}
	}
	if _, ok := dist[to]; !ok {
		return nil
	}
	var path []Edge
	for at := to; at != from; at = prevNode[at] {
		path = append([]Edge{g.Edges[prevEdge[at]]}, path...)
	}
	return path
}

// NodesByType returns nodes of the given type sorted by name.
func (g *Graph) NodesByType(typ string) []*Node {
	var out []*Node
	for _, n := range g.Nodes {
		if n.Type == typ {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NodeText builds the natural-language description of a node used for
// embedding and keyword search: name, type, attributes and immediate
// relationships.
func (g *Graph) NodeText(id string) string {
	n := g.Nodes[id]
	if n == nil {
		return ""
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
	var cols, refs, refBy, accepts, returns, usedBy, methods, calledBy []string
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
		}
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
	return b.String()
}

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

func Load(path string) (*Graph, error) {
	g, err := LoadRaw(path)
	if err != nil {
		return nil, err
	}
	// Curated annotations live in a sibling file so hand-written business
	// knowledge (entity_note, default_filter, ...) survives re-extraction and
	// is merged over the auto-extracted graph on every load.
	if err := g.applyAnnotations(filepath.Join(filepath.Dir(path), AnnotationsFile)); err != nil {
		return nil, err
	}
	return g, nil
}

// LoadRaw loads the graph exactly as extracted, without merging the curated
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
// key in set is applied (an empty value deletes that single key). Returns the
// node's resulting annotation map. Shared by `gatt annotate` and the MCP
// annotate_entity tool so curated business knowledge is written the same way.
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
			continue // annotation for a node that no longer exists; ignore
		}
		if n.Attrs == nil {
			n.Attrs = map[string]string{}
		}
		maps.Copy(n.Attrs, attrs)
	}
	return nil
}
