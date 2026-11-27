// Package httpapi serves the JSON API consumed by the gatt web UI.
package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/system4us/graphAllTheThings/internal/engine"
	"github.com/system4us/graphAllTheThings/pkg/graph"
)

// RepoInfo describes a repository known to the server.
type RepoInfo struct {
	Root    string `json:"root"`
	Key     string `json:"key"`
	Head    string `json:"head,omitempty"`
	Running bool   `json:"running"`
	Queued  bool   `json:"queued"`
}

// RepoStatus reports live background sync / embedding progress for a repo.
type RepoStatus struct {
	Running      bool   `json:"running"`
	Queued       bool   `json:"queued"`
	ProgressDone int64  `json:"progress_done,omitempty"`
	ProgressTot  int64  `json:"progress_total,omitempty"`
	LastAt       string `json:"last_at,omitempty"`
	LastDuration string `json:"last_duration,omitempty"`
	LastSummary  string `json:"last_summary,omitempty"`
	LastEmbedded int    `json:"last_embedded,omitempty"`
	LastNote     string `json:"last_note,omitempty"`
	LastError    string `json:"last_error,omitempty"`
}

// Provider bridges the HTTP API to either single-repo or multi-repo server state.
type Provider interface {
	Mode() string // "single" or "multi"
	ListRepos() []RepoInfo
	EngineFor(ctx context.Context, repo string) (*engine.Engine, string, error)
	TriggerRefresh(ctx context.Context, repo string) (string, error)
	GraphPathFor(ctx context.Context, repo string) (string, error)
	ReloadRepo(ctx context.Context, repo string) error
	StatusFor(ctx context.Context, repo string) (RepoStatus, error)
}

// Handler mounts all /api/* routes on a ServeMux.
type Handler struct {
	p   Provider
	mux *http.ServeMux
}

// New constructs a Handler backed by p.
func New(p Provider) *Handler {
	h := &Handler{p: p, mux: http.NewServeMux()}
	h.mux.HandleFunc("/api/info", h.handleInfo)
	h.mux.HandleFunc("/api/status", h.handleStatus)
	h.mux.HandleFunc("/api/overview", h.handleOverview)
	h.mux.HandleFunc("/api/graph", h.handleGraph)
	h.mux.HandleFunc("/api/node", h.handleNode)
	h.mux.HandleFunc("/api/search", h.handleSearch)
	h.mux.HandleFunc("/api/tree", h.handleTree)
	h.mux.HandleFunc("/api/refresh", h.handleRefresh)
	h.mux.HandleFunc("/api/grep", h.handleGrep)
	h.mux.HandleFunc("/api/diff", h.handleDiff)
	h.mux.HandleFunc("/api/doc-drift", h.handleDocDrift)
	h.mux.HandleFunc("/api/annotate", h.handleAnnotate)
	h.mux.HandleFunc("/api/join", h.handleJoin)
	h.mux.HandleFunc("/api/context", h.handleContext)
	h.mux.HandleFunc("/api/tests", h.handleTests)
	h.mux.HandleFunc("/api/hotspots", h.handleHotspots)
	h.mux.HandleFunc("/api/node-diff", h.handleNodeDiff)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) resolveEngine(w http.ResponseWriter, r *http.Request) (*engine.Engine, string, bool) {
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repo == "" {
		repo = strings.TrimSpace(r.Header.Get("X-Gatt-Repo"))
	}
	e, resolved, err := h.p.EngineFor(r.Context(), repo)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return nil, "", false
	}
	if e == nil || e.G == nil {
		writeErr(w, http.StatusServiceUnavailable, "graph not loaded yet")
		return nil, "", false
	}
	return e, resolved, true
}

func (h *Handler) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	repos := h.p.ListRepos()
	if repos == nil {
		repos = []RepoInfo{}
	}
	defaultRepo := ""
	if len(repos) > 0 {
		defaultRepo = repos[0].Root
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":         h.p.Mode(),
		"repos":        repos,
		"default_repo": defaultRepo,
	})
}

func (h *Handler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	state, err := h.p.TriggerRefresh(r.Context(), repo)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"state": state})
}

type packageStat struct {
	Path      string `json:"path"`
	Files     int    `json:"files"`
	Functions int    `json:"functions"`
}

func (h *Handler) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	e, resolved, ok := h.resolveEngine(w, r)
	if !ok {
		return
	}
	g := e.G
	nodeCounts := map[string]int{}
	edgeCounts := map[string]int{}
	pkgMap := map[string]*packageStat{}

	for _, n := range g.Nodes {
		if n.Attrs["external"] == "true" {
			continue
		}
		nodeCounts[n.Type]++
		if f := n.Attrs["file"]; f != "" {
			dir := filepath.ToSlash(filepath.Dir(f))
			if dir == "." {
				dir = "(root)"
			}
			ps := pkgMap[dir]
			if ps == nil {
				ps = &packageStat{Path: dir}
				pkgMap[dir] = ps
			}
			switch n.Type {
			case graph.NodeFile:
				ps.Files++
			case graph.NodeFunction:
				ps.Functions++
			}
		} else if n.Type == graph.NodeFile {
			dir := filepath.ToSlash(filepath.Dir(n.Name))
			if dir == "." {
				dir = "(root)"
			}
			ps := pkgMap[dir]
			if ps == nil {
				ps = &packageStat{Path: dir}
				pkgMap[dir] = ps
			}
			ps.Files++
		}
	}
	for _, ed := range g.Edges {
		edgeCounts[ed.Type]++
	}

	pkgs := make([]packageStat, 0, len(pkgMap))
	for _, ps := range pkgMap {
		pkgs = append(pkgs, *ps)
	}
	sort.Slice(pkgs, func(i, j int) bool {
		if pkgs[i].Functions != pkgs[j].Functions {
			return pkgs[i].Functions > pkgs[j].Functions
		}
		if pkgs[i].Files != pkgs[j].Files {
			return pkgs[i].Files > pkgs[j].Files
		}
		return pkgs[i].Path < pkgs[j].Path
	})
	if len(pkgs) > 40 {
		pkgs = pkgs[:40]
	}

	var extractedAt string
	if !g.ExtractedAt.IsZero() {
		extractedAt = g.ExtractedAt.UTC().Format(time.RFC3339)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"repo":         resolved,
		"source":       g.Source,
		"extracted_at": extractedAt,
		"is_codebase":  e.IsCodebase(),
		"is_api":       g.IsAPI(),
		"total_nodes":  len(g.Nodes),
		"total_edges":  len(g.Edges),
		"node_counts":  nodeCounts,
		"edge_counts":  edgeCounts,
		"top_packages": pkgs,
		"routes_count": nodeCounts[graph.NodeRoute],
		"models_count": nodeCounts[graph.NodeModel] + nodeCounts[graph.NodeTable] + nodeCounts[graph.NodeSchema],
	})
}

// GraphNode is the compact node representation sent to the canvas renderer.
type GraphNode struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Name      string            `json:"name"`
	Label     string            `json:"label"`
	File      string            `json:"file,omitempty"`
	Package   string            `json:"package,omitempty"`
	LineStart int               `json:"line_start,omitempty"`
	LineEnd   int               `json:"line_end,omitempty"`
	Degree     int               `json:"degree"`
	Distance   *int              `json:"distance,omitempty"`
	DiffStatus string            `json:"diff_status,omitempty"`
	External   bool              `json:"external,omitempty"`
	Attrs      map[string]string `json:"attrs,omitempty"`
}

// GraphEdge is the compact edge representation sent to the canvas renderer.
type GraphEdge struct {
	ID    string            `json:"id"`
	From  string            `json:"from"`
	To    string            `json:"to"`
	Type  string            `json:"type"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

func parseCSVSet(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out[p] = true
		}
	}
	return out
}

func parseIntParam(r *http.Request, name string, def, minVal, maxVal int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if v < minVal {
		return minVal
	}
	if v > maxVal {
		return maxVal
	}
	return v
}

func nodeFile(n *graph.Node) string {
	if f := n.Attrs["file"]; f != "" {
		return f
	}
	if n.Type == graph.NodeFile {
		return n.Name
	}
	return ""
}

func nodePackage(n *graph.Node) string {
	if p := n.Attrs["package"]; p != "" {
		return p
	}
	if n.Type == graph.NodePackage {
		return n.Name
	}
	if f := nodeFile(n); f != "" {
		d := filepath.ToSlash(filepath.Dir(f))
		if d == "." {
			return "(root)"
		}
		return d
	}
	return ""
}

func shortLabel(n *graph.Node) string {
	switch n.Type {
	case graph.NodeFile:
		return filepath.Base(n.Name)
	case graph.NodeRoute:
		if m, p := n.Attrs["method"], n.Attrs["path"]; p != "" {
			if m == "" {
				m = "ANY"
			}
			return m + " " + p
		}
	}
	return n.Name
}

func compactAttrs(n *graph.Node) map[string]string {
	if len(n.Attrs) == 0 {
		return nil
	}
	keep := []string{"lang", "kind", "signature", "method", "path", "table", "field_count", "row_count", "exported", "summary", "comment"}
	out := map[string]string{}
	for _, k := range keep {
		if v := n.Attrs[k]; v != "" {
			if len(v) > 160 {
				v = v[:157] + "..."
			}
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (h *Handler) handleGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	e, resolved, ok := h.resolveEngine(w, r)
	if !ok {
		return
	}
	g := e.G

	view := strings.TrimSpace(r.URL.Query().Get("view"))
	if view == "" {
		if e.IsCodebase() {
			view = "architecture"
		} else {
			view = "models"
		}
	}
	focus := strings.TrimSpace(r.URL.Query().Get("focus"))
	pkgFilter := strings.TrimSpace(r.URL.Query().Get("package"))
	depth := parseIntParam(r, "depth", 2, 1, 5)
	limit := parseIntParam(r, "limit", 140, 10, 500)
	includeExternal := r.URL.Query().Get("include_external") == "true"
	typeFilter := parseCSVSet(r.URL.Query().Get("types"))
	edgeFilter := parseCSVSet(r.URL.Query().Get("edge_types"))

	selected := map[string]*graph.Node{}
	distances := map[string]int{}
	allowedEdges := map[string]bool{}
	totalMatching := 0

	addNode := func(n *graph.Node) bool {
		if n == nil {
			return false
		}
		if !includeExternal && n.Attrs["external"] == "true" {
			return false
		}
		if len(typeFilter) > 0 && !typeFilter[n.Type] {
			return false
		}
		selected[n.ID] = n
		return true
	}

	switch view {
	case "impact":
		if focus == "" || g.Nodes[focus] == nil {
			writeErr(w, http.StatusBadRequest, "impact view requires a valid focus node id")
			return
		}
		impactEdgeTypes := map[string]bool{
			graph.EdgeCalls:          true,
			graph.EdgeImports:        true,
			graph.EdgeDependsOn:      true,
			graph.EdgeHandledBy:      true,
			graph.EdgeUsesMiddleware: true,
			graph.EdgeUsesModel:      true,
			graph.EdgeCallsEndpoint:  true,
			graph.EdgeReferences:     true,
			graph.EdgeForeignKey:     true,
			graph.EdgeCoChanged:      true,
			graph.EdgeHasMethod:      true,
		}
		allowedEdges = impactEdgeTypes
		if len(edgeFilter) > 0 {
			allowedEdges = edgeFilter
		}
		type qItem struct {
			id   string
			dist int
		}
		queue := []qItem{{id: focus, dist: 0}}
		distances[focus] = 0
		selected[focus] = g.Nodes[focus]

		for len(queue) > 0 && len(selected) < limit {
			cur := queue[0]
			queue = queue[1:]
			if cur.dist >= depth {
				continue
			}
			for _, ed := range g.EdgesOf(cur.id) {
				if !allowedEdges[ed.Type] {
					continue
				}
				var nextID string
				if ed.Type == graph.EdgeCoChanged {
					if ed.From == cur.id {
						nextID = ed.To
					} else {
						nextID = ed.From
					}
				} else if ed.To == cur.id {
					// Reverse dependency: whoever calls/imports/depends on cur is impacted.
					nextID = ed.From
				} else {
					continue
				}
				if _, seen := distances[nextID]; seen {
					continue
				}
				nn := g.Nodes[nextID]
				if nn == nil || (!includeExternal && nn.Attrs["external"] == "true") {
					continue
				}
				distances[nextID] = cur.dist + 1
				selected[nextID] = nn
				if len(selected) >= limit {
					break
				}
				queue = append(queue, qItem{id: nextID, dist: cur.dist + 1})
			}
		}
		totalMatching = len(selected)

	case "neighbors":
		if focus == "" || g.Nodes[focus] == nil {
			writeErr(w, http.StatusBadRequest, "neighbors view requires a valid focus node id")
			return
		}
		type qItem struct {
			id   string
			dist int
		}
		queue := []qItem{{id: focus, dist: 0}}
		distances[focus] = 0
		selected[focus] = g.Nodes[focus]

		for len(queue) > 0 && len(selected) < limit {
			cur := queue[0]
			queue = queue[1:]
			if cur.dist >= depth {
				continue
			}
			for _, ed := range g.EdgesOf(cur.id) {
				if len(edgeFilter) > 0 && !edgeFilter[ed.Type] {
					continue
				}
				other := ed.To
				if other == cur.id {
					other = ed.From
				}
				if _, seen := distances[other]; seen {
					continue
				}
				nn := g.Nodes[other]
				if nn == nil || (!includeExternal && nn.Attrs["external"] == "true") {
					continue
				}
				if len(typeFilter) > 0 && !typeFilter[nn.Type] && other != focus {
					continue
				}
				distances[other] = cur.dist + 1
				selected[other] = nn
				if len(selected) >= limit {
					break
				}
				queue = append(queue, qItem{id: other, dist: cur.dist + 1})
			}
		}
		totalMatching = len(selected)

	case "routes":
		allowedEdges = map[string]bool{
			graph.EdgeHandledBy:      true,
			graph.EdgeUsesMiddleware: true,
			graph.EdgeUsesModel:      true,
			graph.EdgeCallsEndpoint:  true,
			graph.EdgeAccepts:        true,
			graph.EdgeRespondsWith:   true,
			graph.EdgeHasEndpoint:    true,
		}
		if len(edgeFilter) > 0 {
			allowedEdges = edgeFilter
		}
		var seeds []*graph.Node
		for _, n := range g.Nodes {
			if n.Type == graph.NodeRoute || n.Type == graph.NodeEndpoint || n.Attrs["route_method"] != "" {
				if pkgFilter != "" && !strings.Contains(nodeFile(n), pkgFilter) {
					continue
				}
				seeds = append(seeds, n)
			}
		}
		sort.Slice(seeds, func(i, j int) bool { return seeds[i].Name < seeds[j].Name })
		totalMatching = len(seeds)
		for _, s := range seeds {
			if len(selected) >= limit {
				break
			}
			addNode(s)
			for _, ed := range g.EdgesOf(s.ID) {
				if !allowedEdges[ed.Type] {
					continue
				}
				other := ed.To
				if other == s.ID {
					other = ed.From
				}
				if len(selected) >= limit {
					break
				}
				addNode(g.Nodes[other])
			}
		}

	case "models":
		allowedEdges = map[string]bool{
			graph.EdgeReferences:   true,
			graph.EdgeForeignKey:   true,
			graph.EdgeRefersTo:     true,
			graph.EdgeUsesModel:    true,
			graph.EdgeAccepts:      true,
			graph.EdgeRespondsWith: true,
		}
		if len(edgeFilter) > 0 {
			allowedEdges = edgeFilter
		}
		var candidates []*graph.Node
		for _, n := range g.Nodes {
			if !includeExternal && n.Attrs["external"] == "true" {
				continue
			}
			isModel := n.Type == graph.NodeModel || n.Type == graph.NodeTable || n.Type == graph.NodeSchema || n.Type == graph.NodeView || n.Attrs["model_table"] != ""
			if !isModel {
				continue
			}
			if pkgFilter != "" && !strings.Contains(nodeFile(n), pkgFilter) {
				continue
			}
			candidates = append(candidates, n)
		}
		sortByDegree(g, candidates)
		totalMatching = len(candidates)
		for _, n := range candidates {
			if len(selected) >= limit {
				break
			}
			addNode(n)
		}

	case "calls":
		allowedEdges = map[string]bool{
			graph.EdgeCalls:       true,
			graph.EdgeHasMethod:   true,
			graph.EdgeReadsState:  true,
			graph.EdgeWritesState: true,
			graph.EdgeHandledBy:   true,
		}
		if len(edgeFilter) > 0 {
			allowedEdges = edgeFilter
		}
		if focus != "" && g.Nodes[focus] != nil {
			type qItem struct {
				id   string
				dist int
			}
			queue := []qItem{{id: focus, dist: 0}}
			distances[focus] = 0
			selected[focus] = g.Nodes[focus]
			for len(queue) > 0 && len(selected) < limit {
				cur := queue[0]
				queue = queue[1:]
				if cur.dist >= depth {
					continue
				}
				for _, ed := range g.EdgesOf(cur.id) {
					if !allowedEdges[ed.Type] {
						continue
					}
					other := ed.To
					if other == cur.id {
						other = ed.From
					}
					if _, seen := distances[other]; seen {
						continue
					}
					nn := g.Nodes[other]
					if nn == nil || (!includeExternal && nn.Attrs["external"] == "true") {
						continue
					}
					distances[other] = cur.dist + 1
					selected[other] = nn
					if len(selected) >= limit {
						break
					}
					queue = append(queue, qItem{id: other, dist: cur.dist + 1})
				}
			}
			totalMatching = len(selected)
		} else {
			var candidates []*graph.Node
			for _, n := range g.Nodes {
				if !includeExternal && n.Attrs["external"] == "true" {
					continue
				}
				if n.Type != graph.NodeFunction && n.Type != graph.NodeDefinition && n.Type != graph.NodeState {
					continue
				}
				if pkgFilter != "" && !matchesPkg(n, pkgFilter) {
					continue
				}
				if len(typeFilter) > 0 && !typeFilter[n.Type] {
					continue
				}
				candidates = append(candidates, n)
			}
			sortByDegree(g, candidates)
			totalMatching = len(candidates)
			for _, n := range candidates {
				if len(selected) >= limit {
					break
				}
				selected[n.ID] = n
			}
		}

	default: // "architecture"
		// When viewing the whole repo with no package or type filter, show an
		// aggregated module/directory dependency graph (10-30 clean boxes instead
		// of hundreds of individual files). Drilling into a package shows its files.
		if pkgFilter == "" && len(typeFilter) == 0 && e.IsCodebase() {
			dirNodes, dirEdges := buildDirectoryGraph(g, includeExternal, limit)
			if len(dirNodes) > 0 {
				writeJSON(w, http.StatusOK, map[string]any{
					"repo":           resolved,
					"view":           view,
					"focus":          focus,
					"package":        pkgFilter,
					"total_matching": len(dirNodes),
					"truncated":      false,
					"nodes":          dirNodes,
					"edges":          dirEdges,
				})
				return
			}
		}

		allowedEdges = map[string]bool{
			graph.EdgeImports:   true,
			graph.EdgeDependsOn: true,
			graph.EdgeCoChanged: true,
			graph.EdgeGenerates: true,
			graph.EdgeUsesStyle: true,
		}
		if pkgFilter != "" {
			allowedEdges[graph.EdgeCalls] = true
			allowedEdges[graph.EdgeHasMethod] = true
		}
		if len(edgeFilter) > 0 {
			allowedEdges = edgeFilter
		}
		var candidates []*graph.Node
		for _, n := range g.Nodes {
			if !includeExternal && n.Attrs["external"] == "true" {
				continue
			}
			if pkgFilter != "" {
				if !matchesPkg(n, pkgFilter) {
					continue
				}
				if len(typeFilter) > 0 {
					if !typeFilter[n.Type] {
						continue
					}
				} else if n.Type != graph.NodeFile && n.Type != graph.NodeFunction && n.Type != graph.NodeDefinition && n.Type != graph.NodeRoute && n.Type != graph.NodeModel {
					continue
				}
			} else {
				if len(typeFilter) > 0 {
					if !typeFilter[n.Type] {
						continue
					}
				} else if n.Type != graph.NodeFile && n.Type != graph.NodePackage && n.Type != graph.NodeComponent {
					continue
				}
			}
			candidates = append(candidates, n)
		}
		sortByDegree(g, candidates)
		totalMatching = len(candidates)
		for _, n := range candidates {
			if len(selected) >= limit {
				break
			}
			selected[n.ID] = n
		}
	}

	// Build response nodes & edges (only edges where both endpoints are in selected).
	nodes := make([]GraphNode, 0, len(selected))
	for _, n := range selected {
		ls, _ := strconv.Atoi(n.Attrs["line_start"])
		le, _ := strconv.Atoi(n.Attrs["line_end"])
		gn := GraphNode{
			ID:        n.ID,
			Type:      n.Type,
			Name:      n.Name,
			Label:     shortLabel(n),
			File:      nodeFile(n),
			Package:   nodePackage(n),
			LineStart: ls,
			LineEnd:   le,
			Degree:    len(g.EdgesOf(n.ID)),
			External:  n.Attrs["external"] == "true",
			Attrs:     compactAttrs(n),
		}
		if d, ok := distances[n.ID]; ok {
			dd := d
			gn.Distance = &dd
		}
		nodes = append(nodes, gn)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	edges := make([]GraphEdge, 0)
	seenEdge := map[string]bool{}
	for _, n := range selected {
		for _, ed := range g.EdgesOf(n.ID) {
			if ed.From != n.ID {
				continue
			}
			if selected[ed.To] == nil {
				continue
			}
			if len(allowedEdges) > 0 && !allowedEdges[ed.Type] {
				continue
			}
			if len(edgeFilter) > 0 && !edgeFilter[ed.Type] {
				continue
			}
			key := ed.From + "|" + ed.Type + "|" + ed.To
			if seenEdge[key] {
				continue
			}
			seenEdge[key] = true
			edges = append(edges, GraphEdge{
				ID:    key,
				From:  ed.From,
				To:    ed.To,
				Type:  ed.Type,
				Attrs: ed.Attrs,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"repo":           resolved,
		"view":           view,
		"focus":          focus,
		"package":        pkgFilter,
		"total_matching": totalMatching,
		"truncated":      totalMatching > len(nodes),
		"nodes":          nodes,
		"edges":          edges,
	})
}

func matchesPkg(n *graph.Node, pkg string) bool {
	if pkg == "(root)" {
		f := nodeFile(n)
		return f != "" && filepath.ToSlash(filepath.Dir(f)) == "."
	}
	f := filepath.ToSlash(nodeFile(n))
	if f == pkg || strings.HasPrefix(f, pkg+"/") {
		return true
	}
	p := filepath.ToSlash(nodePackage(n))
	return p == pkg || strings.HasPrefix(p, pkg+"/")
}

func sortByDegree(g *graph.Graph, nodes []*graph.Node) {
	sort.Slice(nodes, func(i, j int) bool {
		di := len(g.EdgesOf(nodes[i].ID))
		dj := len(g.EdgesOf(nodes[j].ID))
		if di != dj {
			return di > dj
		}
		return nodes[i].Name < nodes[j].Name
	})
}

// buildDirectoryGraph aggregates file-level nodes and IMPORTS/DEPENDS_ON/CALLS
// edges into a clean, readable directory-level architecture graph.
func buildDirectoryGraph(g *graph.Graph, includeExternal bool, limit int) ([]GraphNode, []GraphEdge) {
	type dirAcc struct {
		path      string
		files     int
		functions int
		degree    int
	}
	dirs := map[string]*dirAcc{}
	nodeToDir := map[string]string{}

	dirOf := func(f string) string {
		d := filepath.ToSlash(filepath.Dir(f))
		if d == "." || d == "" {
			return "(root)"
		}
		return d
	}

	for _, n := range g.Nodes {
		if !includeExternal && n.Attrs["external"] == "true" {
			continue
		}
		f := nodeFile(n)
		if f == "" {
			continue
		}
		d := dirOf(f)
		acc := dirs[d]
		if acc == nil {
			acc = &dirAcc{path: d}
			dirs[d] = acc
		}
		nodeToDir[n.ID] = d
		switch n.Type {
		case graph.NodeFile:
			acc.files++
		case graph.NodeFunction:
			acc.functions++
		}
	}
	if len(dirs) <= 1 {
		// Single-directory repo: fall back to showing files directly.
		return nil, nil
	}

	edgeWeights := map[string]int{}
	for _, ed := range g.Edges {
		if ed.Type != graph.EdgeImports && ed.Type != graph.EdgeDependsOn && ed.Type != graph.EdgeCalls {
			continue
		}
		fd, ok1 := nodeToDir[ed.From]
		td, ok2 := nodeToDir[ed.To]
		if !ok1 || !ok2 || fd == td {
			continue
		}
		key := fd + "|" + td
		edgeWeights[key]++
	}
	for k := range edgeWeights {
		parts := strings.SplitN(k, "|", 2)
		if a := dirs[parts[0]]; a != nil {
			a.degree++
		}
		if b := dirs[parts[1]]; b != nil {
			b.degree++
		}
	}

	list := make([]*dirAcc, 0, len(dirs))
	for _, d := range dirs {
		if d.files == 0 && d.functions == 0 {
			continue
		}
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].degree != list[j].degree {
			return list[i].degree > list[j].degree
		}
		if list[i].functions != list[j].functions {
			return list[i].functions > list[j].functions
		}
		return list[i].path < list[j].path
	})
	if len(list) > limit {
		list = list[:limit]
	}

	kept := map[string]bool{}
	nodes := make([]GraphNode, 0, len(list))
	for _, d := range list {
		kept[d.path] = true
		label := d.path
		if d.files > 0 {
			label = d.path + " (" + strconv.Itoa(d.files) + "f)"
		}
		nodes = append(nodes, GraphNode{
			ID:      "dir:" + d.path,
			Type:    graph.NodePackage,
			Name:    d.path,
			Label:   label,
			Package: "",
			Degree:  d.degree,
			Attrs: map[string]string{
				"files":     strconv.Itoa(d.files),
				"functions": strconv.Itoa(d.functions),
			},
		})
	}

	var edges []GraphEdge
	for k, w := range edgeWeights {
		parts := strings.SplitN(k, "|", 2)
		if !kept[parts[0]] || !kept[parts[1]] {
			continue
		}
		edges = append(edges, GraphEdge{
			ID:   "dir:" + parts[0] + "|IMPORTS|dir:" + parts[1],
			From: "dir:" + parts[0],
			To:   "dir:" + parts[1],
			Type: graph.EdgeImports,
			Attrs: map[string]string{
				"weight": strconv.Itoa(w),
			},
		})
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
	return nodes, edges
}

type neighborSummary struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Name     string            `json:"name"`
	Label    string            `json:"label"`
	File     string            `json:"file,omitempty"`
	EdgeType string            `json:"edge_type"`
	Attrs    map[string]string `json:"attrs,omitempty"`
}

type sourcePreview struct {
	File      string `json:"file"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Language  string `json:"language,omitempty"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (h *Handler) handleNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	e, resolved, ok := h.resolveEngine(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id parameter")
		return
	}
	if strings.HasPrefix(id, "dir:") {
		dirPath := strings.TrimPrefix(id, "dir:")
		var files []neighborSummary
		fnCount := 0
		for _, fn := range e.G.Nodes {
			if fn.Attrs["external"] == "true" || !matchesPkg(fn, dirPath) {
				continue
			}
			if fn.Type == graph.NodeFile {
				files = append(files, neighborSummary{
					ID:       fn.ID,
					Type:     fn.Type,
					Name:     fn.Name,
					Label:    shortLabel(fn),
					File:     nodeFile(fn),
					EdgeType: "CONTAINS",
				})
			} else if fn.Type == graph.NodeFunction {
				fnCount++
			}
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
		writeJSON(w, http.StatusOK, map[string]any{
			"repo":     resolved,
			"id":       id,
			"type":     graph.NodePackage,
			"name":     dirPath,
			"label":    dirPath,
			"package":  dirPath,
			"attrs":    map[string]string{"files": strconv.Itoa(len(files)), "functions": strconv.Itoa(fnCount)},
			"incoming": []neighborSummary{},
			"outgoing": files,
			"source":   nil,
		})
		return
	}
	n := e.G.Nodes[id]
	if n == nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}

	var incoming, outgoing []neighborSummary
	for _, ed := range e.G.EdgesOf(id) {
		if ed.From == id {
			if other := e.G.Nodes[ed.To]; other != nil {
				outgoing = append(outgoing, neighborSummary{
					ID:       other.ID,
					Type:     other.Type,
					Name:     other.Name,
					Label:    shortLabel(other),
					File:     nodeFile(other),
					EdgeType: ed.Type,
					Attrs:    ed.Attrs,
				})
			}
		} else if ed.To == id {
			if other := e.G.Nodes[ed.From]; other != nil {
				incoming = append(incoming, neighborSummary{
					ID:       other.ID,
					Type:     other.Type,
					Name:     other.Name,
					Label:    shortLabel(other),
					File:     nodeFile(other),
					EdgeType: ed.Type,
					Attrs:    ed.Attrs,
				})
			}
		}
	}
	sort.Slice(incoming, func(i, j int) bool {
		if incoming[i].EdgeType != incoming[j].EdgeType {
			return incoming[i].EdgeType < incoming[j].EdgeType
		}
		return incoming[i].Name < incoming[j].Name
	})
	sort.Slice(outgoing, func(i, j int) bool {
		if outgoing[i].EdgeType != outgoing[j].EdgeType {
			return outgoing[i].EdgeType < outgoing[j].EdgeType
		}
		return outgoing[i].Name < outgoing[j].Name
	})

	var src *sourcePreview
	var history []engine.CommitEntry
	if e.IsCodebase() && n.Attrs["external"] != "true" {
		src = readNodeSource(e.G, n)
		if f := nodeFile(n); f != "" {
			history = e.FileHistory(r.Context(), f, 8)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"repo":     resolved,
		"id":       n.ID,
		"type":     n.Type,
		"name":     n.Name,
		"label":    shortLabel(n),
		"file":     nodeFile(n),
		"package":  nodePackage(n),
		"attrs":    n.Attrs,
		"incoming": incoming,
		"outgoing": outgoing,
		"source":   src,
		"history":  history,
	})
}

func readNodeSource(g *graph.Graph, n *graph.Node) *sourcePreview {
	const maxLines = 120
	const maxBytes = 12 * 1024

	file := nodeFile(n)
	if file == "" {
		return nil
	}
	lineStart, _ := strconv.Atoi(n.Attrs["line_start"])
	lineEnd, _ := strconv.Atoi(n.Attrs["line_end"])
	if lineStart <= 0 {
		lineStart = 1
	}
	if lineEnd < lineStart {
		if n.Type == graph.NodeFile {
			lineEnd = lineStart + maxLines - 1
		} else {
			lineEnd = lineStart + 35
		}
	}

	// If stored body is available and complete, use it when disk isn't readable.
	rootDir := strings.TrimPrefix(g.Source, "codebase:")
	if rootDir == "" {
		if body := n.Attrs["body"]; body != "" {
			return &sourcePreview{
				File:      file,
				LineStart: lineStart,
				LineEnd:   lineEnd,
				Language:  n.Attrs["lang"],
				Content:   body,
			}
		}
		return nil
	}

	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil
	}
	absFile, err := filepath.Abs(filepath.Join(absRoot, file))
	if err != nil {
		return nil
	}
	// Prevent path traversal outside the repository root.
	rel, err := filepath.Rel(absRoot, absFile)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}

	f, err := os.Open(absFile)
	if err != nil {
		if body := n.Attrs["body"]; body != "" {
			return &sourcePreview{
				File:      file,
				LineStart: lineStart,
				LineEnd:   lineEnd,
				Language:  n.Attrs["lang"],
				Content:   body,
			}
		}
		return nil
	}
	defer f.Close()

	readEnd := lineEnd
	truncated := false
	if readEnd-lineStart+1 > maxLines {
		readEnd = lineStart + maxLines - 1
		truncated = true
	}

	var lines []string
	totalBytes := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	actualEnd := lineStart
	for ln := 1; sc.Scan(); ln++ {
		if ln < lineStart {
			continue
		}
		if ln > readEnd {
			truncated = true
			break
		}
		line := sc.Text()
		totalBytes += len(line) + 1
		if totalBytes > maxBytes {
			truncated = true
			break
		}
		lines = append(lines, line)
		actualEnd = ln
	}
	if len(lines) == 0 {
		return nil
	}
	return &sourcePreview{
		File:      file,
		LineStart: lineStart,
		LineEnd:   actualEnd,
		Language:  n.Attrs["lang"],
		Content:   strings.Join(lines, "\n"),
		Truncated: truncated,
	}
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	e, resolved, ok := h.resolveEngine(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"repo": resolved, "method": "empty", "hits": []any{}})
		return
	}
	nodeType := strings.TrimSpace(r.URL.Query().Get("type"))
	limit := parseIntParam(r, "limit", 25, 1, 100)

	res, err := e.Find(r.Context(), q, nodeType, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	type hitOut struct {
		ID    string  `json:"id"`
		Type  string  `json:"type"`
		Name  string  `json:"name"`
		Label string  `json:"label"`
		File  string  `json:"file,omitempty"`
		Score float32 `json:"score"`
	}
	hits := make([]hitOut, 0, len(res.Hits))
	for _, hit := range res.Hits {
		var file, label string
		label = hit.Name
		if n := e.G.Nodes[hit.ID]; n != nil {
			file = nodeFile(n)
			label = shortLabel(n)
		}
		hits = append(hits, hitOut{
			ID:    hit.ID,
			Type:  hit.Type,
			Name:  hit.Name,
			Label: label,
			File:  file,
			Score: hit.Score,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repo":   resolved,
		"method": res.Method,
		"hits":   hits,
	})
}

type treeNode struct {
	Name      string      `json:"name"`
	Path      string      `json:"path"`
	Kind      string      `json:"kind"` // "dir" or "file"
	NodeID    string      `json:"node_id,omitempty"`
	Functions int         `json:"functions,omitempty"`
	Children  []*treeNode `json:"children,omitempty"`
}

func (h *Handler) handleTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	e, resolved, ok := h.resolveEngine(w, r)
	if !ok {
		return
	}

	fnCountByFile := map[string]int{}
	for _, n := range e.G.Nodes {
		if n.Type == graph.NodeFunction && n.Attrs["external"] != "true" {
			if f := n.Attrs["file"]; f != "" {
				fnCountByFile[filepath.ToSlash(f)]++
			}
		}
	}

	root := &treeNode{Name: "/", Path: "", Kind: "dir"}
	dirIndex := map[string]*treeNode{"": root}

	ensureDir := func(dirPath string) *treeNode {
		if d, ok := dirIndex[dirPath]; ok {
			return d
		}
		parts := strings.Split(dirPath, "/")
		cur := root
		acc := ""
		for _, p := range parts {
			if p == "" || p == "." {
				continue
			}
			if acc == "" {
				acc = p
			} else {
				acc = acc + "/" + p
			}
			next, ok := dirIndex[acc]
			if !ok {
				next = &treeNode{Name: p, Path: acc, Kind: "dir"}
				dirIndex[acc] = next
				cur.Children = append(cur.Children, next)
			}
			cur = next
		}
		return cur
	}

	files := e.G.NodesByType(graph.NodeFile)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	for _, f := range files {
		if f.Attrs["external"] == "true" {
			continue
		}
		p := filepath.ToSlash(f.Name)
		dir := filepath.ToSlash(filepath.Dir(p))
		if dir == "." {
			dir = ""
		}
		parent := ensureDir(dir)
		parent.Children = append(parent.Children, &treeNode{
			Name:      filepath.Base(p),
			Path:      p,
			Kind:      "file",
			NodeID:    f.ID,
			Functions: fnCountByFile[p],
		})
	}

	var sortTree func(*treeNode)
	sortTree = func(t *treeNode) {
		sort.Slice(t.Children, func(i, j int) bool {
			if t.Children[i].Kind != t.Children[j].Kind {
				return t.Children[i].Kind == "dir"
			}
			return t.Children[i].Name < t.Children[j].Name
		})
		for _, c := range t.Children {
			if c.Kind == "dir" {
				sortTree(c)
			}
		}
	}
	sortTree(root)

	writeJSON(w, http.StatusOK, map[string]any{
		"repo":     resolved,
		"children": root.Children,
	})
}
