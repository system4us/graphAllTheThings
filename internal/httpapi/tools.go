package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/system4us/graphAllTheThings/internal/codeutil"
	"github.com/system4us/graphAllTheThings/internal/connector/codebase"
	"github.com/system4us/graphAllTheThings/internal/engine"
	"github.com/system4us/graphAllTheThings/pkg/graph"
)

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	st, err := h.p.StatusFor(r.Context(), repo)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *Handler) handleGrep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	e, resolved, ok := h.resolveEngine(w, r)
	if !ok {
		return
	}
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		writeErr(w, http.StatusBadRequest, "missing q parameter")
		return
	}
	useRegex := r.URL.Query().Get("regex") == "true"
	limit := parseIntParam(r, "limit", 100, 0, 500)
	include := splitGlobs(r.URL.Query().Get("include"))
	exclude := splitGlobs(r.URL.Query().Get("exclude"))

	res, err := e.GrepWithOptions(q, engine.GrepOptions{
		Regex:   useRegex,
		Limit:   limit,
		Include: include,
		Exclude: exclude,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Build file -> file node & functions index so hits can jump directly to graph nodes.
	fileNodeByPath := map[string]string{}
	type spanNode struct {
		id, label string
		start     int
		end       int
	}
	spansByFile := map[string][]spanNode{}
	for _, n := range e.G.Nodes {
		if n.Attrs["external"] == "true" {
			continue
		}
		if n.Type == graph.NodeFile {
			fileNodeByPath[filepath.ToSlash(n.Name)] = n.ID
		} else if n.Type == graph.NodeFunction || n.Type == graph.NodeDefinition || n.Type == graph.NodeRoute {
			f := filepath.ToSlash(n.Attrs["file"])
			ls, _ := strconv.Atoi(n.Attrs["line_start"])
			le, _ := strconv.Atoi(n.Attrs["line_end"])
			if f != "" && ls > 0 {
				if le < ls {
					le = ls
				}
				spansByFile[f] = append(spansByFile[f], spanNode{
					id: n.ID, label: shortLabel(n), start: ls, end: le,
				})
			}
		}
	}

	type grepHitOut struct {
		Path       string `json:"path"`
		Line       int    `json:"line"`
		Text       string `json:"text"`
		FileNodeID string `json:"file_node_id,omitempty"`
		NodeID     string `json:"node_id,omitempty"`
		NodeLabel  string `json:"node_label,omitempty"`
	}
	hits := make([]grepHitOut, 0, len(res.Hits))
	for _, hit := range res.Hits {
		p := filepath.ToSlash(hit.Path)
		out := grepHitOut{
			Path:       p,
			Line:       hit.Line,
			Text:       hit.Text,
			FileNodeID: fileNodeByPath[p],
		}
		// Pick the tightest enclosing symbol span if available.
		bestLen := int(^uint(0) >> 1)
		for _, sp := range spansByFile[p] {
			if hit.Line >= sp.start && hit.Line <= sp.end {
				spanLen := sp.end - sp.start
				if spanLen < bestLen {
					bestLen = spanLen
					out.NodeID = sp.id
					out.NodeLabel = sp.label
				}
			}
		}
		if out.NodeID == "" {
			out.NodeID = out.FileNodeID
		}
		hits = append(hits, out)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"repo":          resolved,
		"total":         res.Total,
		"shown":         res.Shown,
		"files_scanned": res.FilesScanned,
		"truncated":     res.Truncated,
		"hits":          hits,
	})
}

func splitGlobs(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (h *Handler) handleDiff(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	e, resolved, ok := h.resolveEngine(w, r)
	if !ok {
		return
	}
	if !e.IsCodebase() {
		writeErr(w, http.StatusBadRequest, "code_diff requires a codebase graph")
		return
	}
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		ref = "HEAD"
	}
	limit := parseIntParam(r, "limit", 60, 1, 200)
	dir := strings.TrimPrefix(e.G.Source, "codebase:")

	pairs, err := codebase.GitChangedFiles(r.Context(), dir, ref)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(pairs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"repo":          resolved,
			"ref":           ref,
			"files_touched": 0,
			"changes":       []any{},
			"nodes":         []any{},
			"edges":         []any{},
		})
		return
	}

	renamed := map[string]string{}
	for _, p := range pairs {
		if p.Old != "" && p.New != "" && p.Old != p.New {
			renamed[p.Old] = p.New
		}
	}
	oldGraph, err := codebase.New(dir).ExtractAt(r.Context(), ref)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "extracting "+ref+": "+err.Error())
		return
	}
	d := codeutil.DiffCode(oldGraph, e.G, pairs, renamed)

	type callerOut struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		File  string `json:"file,omitempty"`
		Label string `json:"label"`
	}
	type changeOut struct {
		Kind    string      `json:"kind"`
		Type    string      `json:"type"`
		OldID   string      `json:"old_id,omitempty"`
		NewID   string      `json:"new_id,omitempty"`
		OldName string      `json:"old_name,omitempty"`
		NewName string      `json:"new_name,omitempty"`
		OldFile string      `json:"old_file,omitempty"`
		NewFile string      `json:"new_file,omitempty"`
		Callers []callerOut `json:"callers,omitempty"`
	}

	shown := d.Changes
	if len(shown) > limit {
		shown = shown[:limit]
	}

	nodeMap := map[string]GraphNode{}
	edgeMap := map[string]GraphEdge{}
	changes := make([]changeOut, 0, len(shown))

	for _, c := range shown {
		co := changeOut{
			Kind:    string(c.Kind),
			Type:    c.Type,
			NewID:   c.NewID,
			OldName: c.OldName,
			NewName: c.NewName,
			OldFile: c.OldFile,
			NewFile: c.NewFile,
		}

		status := "modified"
		switch c.Kind {
		case codeutil.CodeAdded:
			status = "added"
		case codeutil.CodeRemoved:
			status = "removed"
		}

		if c.NewID != "" {
			if n := e.G.Nodes[c.NewID]; n != nil {
				gn := toGraphNode(e.G, n)
				gn.DiffStatus = status
				nodeMap[n.ID] = gn

				for _, ed := range e.G.EdgesOf(n.ID) {
					if ed.Type != graph.EdgeCalls || ed.To != n.ID {
						continue
					}
					caller := e.G.Nodes[ed.From]
					if caller == nil || caller.Attrs["external"] == "true" {
						continue
					}
					co.Callers = append(co.Callers, callerOut{
						ID:    caller.ID,
						Name:  caller.Name,
						File:  nodeFile(caller),
						Label: shortLabel(caller),
					})
					if _, exists := nodeMap[caller.ID]; !exists {
						cn := toGraphNode(e.G, caller)
						cn.DiffStatus = "caller"
						nodeMap[caller.ID] = cn
					}
					ekey := ed.From + "|" + ed.Type + "|" + ed.To
					edgeMap[ekey] = GraphEdge{ID: ekey, From: ed.From, To: ed.To, Type: ed.Type, Attrs: ed.Attrs}
				}
			}
		} else if c.OldName != "" {
			// Removed symbol: synthesize a ghost node so it's visible in red on the canvas.
			ghostID := "removed:" + c.OldFile + ":" + c.OldName
			co.OldID = ghostID
			nodeMap[ghostID] = GraphNode{
				ID:         ghostID,
				Type:       c.Type,
				Name:       c.OldName,
				Label:      c.OldName,
				File:       c.OldFile,
				DiffStatus: "removed",
				Degree:     1,
			}
		}

		changes = append(changes, co)
	}

	nodes := make([]GraphNode, 0, len(nodeMap))
	for _, n := range nodeMap {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	edges := make([]GraphEdge, 0, len(edgeMap))
	for _, ed := range edgeMap {
		edges = append(edges, ed)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"repo":          resolved,
		"ref":           ref,
		"files_touched": len(pairs),
		"total_changes": len(d.Changes),
		"changes":       changes,
		"nodes":         nodes,
		"edges":         edges,
	})
}

func toGraphNode(g *graph.Graph, n *graph.Node) GraphNode {
	ls, _ := strconv.Atoi(n.Attrs["line_start"])
	le, _ := strconv.Atoi(n.Attrs["line_end"])
	return GraphNode{
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
}

func (h *Handler) handleDocDrift(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	e, resolved, ok := h.resolveEngine(w, r)
	if !ok {
		return
	}
	limit := parseIntParam(r, "limit", 50, 1, 200)
	text, err := e.DocDrift(limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Collect doc file nodes mentioned in the drift output for one-click jump.
	type docRef struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	var docs []docRef
	seen := map[string]bool{}
	for _, f := range e.G.NodesByType(graph.NodeFile) {
		p := filepath.ToSlash(f.Name)
		if !seen[f.ID] && strings.Contains(text, p) {
			seen[f.ID] = true
			docs = append(docs, docRef{ID: f.ID, Path: p})
		}
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })

	writeJSON(w, http.StatusOK, map[string]any{
		"repo": resolved,
		"text": text,
		"docs": docs,
	})
}

type annotateReq struct {
	ID             string `json:"id"`
	EntityNote     string `json:"entity_note"`
	DefaultFilter  string `json:"default_filter"`
	RouteMethod    string `json:"route_method"`
	RoutePath      string `json:"route_path"`
	RouteFramework string `json:"route_framework"`
	ModelTable     string `json:"model_table"`
	Clear          bool   `json:"clear"`
}

func (h *Handler) handleAnnotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	e, resolved, ok := h.resolveEngine(w, r)
	if !ok {
		return
	}
	var req annotateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeErr(w, http.StatusBadRequest, "missing node id")
		return
	}
	if e.G.Nodes[req.ID] == nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}

	set := map[string]string{}
	if req.EntityNote != "" {
		set["entity_note"] = req.EntityNote
	}
	if req.DefaultFilter != "" {
		set["default_filter"] = req.DefaultFilter
	}
	if req.RouteMethod != "" {
		set["route_method"] = req.RouteMethod
	}
	if req.RoutePath != "" {
		set["route_path"] = req.RoutePath
	}
	if req.RouteFramework != "" {
		set["route_framework"] = req.RouteFramework
	}
	if req.ModelTable != "" {
		set["model_table"] = req.ModelTable
	}
	if !req.Clear && len(set) == 0 {
		writeErr(w, http.StatusBadRequest, "provide at least one annotation field or clear=true")
		return
	}

	graphPath, err := h.p.GraphPathFor(r.Context(), repo)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	annPath := filepath.Join(filepath.Dir(graphPath), graph.AnnotationsFile)
	result, err := graph.SetAnnotation(annPath, req.ID, set, req.Clear)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = h.p.ReloadRepo(r.Context(), repo)

	writeJSON(w, http.StatusOK, map[string]any{
		"repo":        resolved,
		"id":          req.ID,
		"annotations": result,
	})
}

func (h *Handler) handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	e, resolved, ok := h.resolveEngine(w, r)
	if !ok {
		return
	}
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	if from == "" || to == "" {
		writeErr(w, http.StatusBadRequest, "from and to parameters are required")
		return
	}

	jp := e.Join(from, to)
	rendered := e.RenderJoin(jp)

	// Build a highlighted subgraph of the join path steps.
	nodesMap := map[string]GraphNode{}
	var edges []GraphEdge
	findByName := func(name string) *graph.Node {
		if n := e.G.Nodes[name]; n != nil {
			return n
		}
		for _, n := range e.G.Nodes {
			if strings.EqualFold(n.Name, name) &&
				(n.Type == graph.NodeTable || n.Type == graph.NodeSchema || n.Type == graph.NodeModel || n.Type == graph.NodeView) {
				return n
			}
		}
		return nil
	}

	for i, step := range jp.Steps {
		fn := findByName(step.FromTable)
		tn := findByName(step.ToTable)
		var fid, tid string
		if fn != nil {
			fid = fn.ID
			nodesMap[fid] = toGraphNode(e.G, fn)
		} else {
			fid = "join:" + step.FromTable
			nodesMap[fid] = GraphNode{ID: fid, Type: graph.NodeTable, Name: step.FromTable, Label: step.FromTable, Degree: 1}
		}
		if tn != nil {
			tid = tn.ID
			nodesMap[tid] = toGraphNode(e.G, tn)
		} else {
			tid = "join:" + step.ToTable
			nodesMap[tid] = GraphNode{ID: tid, Type: graph.NodeTable, Name: step.ToTable, Label: step.ToTable, Degree: 1}
		}
		label := "JOIN"
		if step.FromColumn != "" && step.ToColumn != "" {
			label = step.FromColumn + " = " + step.ToColumn
		}
		edges = append(edges, GraphEdge{
			ID:   "join:" + strconv.Itoa(i) + ":" + fid + "->" + tid,
			From: fid,
			To:   tid,
			Type: label,
		})
	}

	nodes := make([]GraphNode, 0, len(nodesMap))
	for _, n := range nodesMap {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	writeJSON(w, http.StatusOK, map[string]any{
		"repo":   resolved,
		"found":  jp.Found,
		"steps":  jp.Steps,
		"hint":   jp.Hint,
		"text":   rendered,
		"nodes":  nodes,
		"edges":  edges,
	})
}

func (h *Handler) handleContext(w http.ResponseWriter, r *http.Request) {
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
		writeErr(w, http.StatusBadRequest, "missing q parameter")
		return
	}
	limit := parseIntParam(r, "limit", 8, 1, 25)
	maxTokens := parseIntParam(r, "max_tokens", 0, 0, 20000)

	var text string
	var err error
	if e.IsCodebase() {
		text, err = e.CodeContextPackWithBudget(r.Context(), q, limit, maxTokens)
	} else {
		text, err = e.ContextPackWithBudget(r.Context(), q, limit, maxTokens)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Also build a relevant subgraph around the top Find hits + their 1-hop neighbors.
	findRes, _ := e.Find(r.Context(), q, "", limit)
	selected := map[string]*graph.Node{}
	for _, hit := range findRes.Hits {
		n := e.G.Nodes[hit.ID]
		if n == nil || n.Attrs["external"] == "true" {
			continue
		}
		selected[n.ID] = n
		added := 0
		for _, ed := range e.G.EdgesOf(n.ID) {
			if added >= 6 || len(selected) >= 80 {
				break
			}
			other := ed.To
			if other == n.ID {
				other = ed.From
			}
			on := e.G.Nodes[other]
			if on == nil || on.Attrs["external"] == "true" || on.Type == graph.NodeProject {
				continue
			}
			if selected[on.ID] == nil {
				selected[on.ID] = on
				added++
			}
		}
	}

	nodes := make([]GraphNode, 0, len(selected))
	for _, n := range selected {
		nodes = append(nodes, toGraphNode(e.G, n))
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	var edges []GraphEdge
	seenEdge := map[string]bool{}
	for _, n := range selected {
		for _, ed := range e.G.EdgesOf(n.ID) {
			if ed.From != n.ID || selected[ed.To] == nil {
				continue
			}
			ekey := ed.From + "|" + ed.Type + "|" + ed.To
			if seenEdge[ekey] {
				continue
			}
			seenEdge[ekey] = true
			edges = append(edges, GraphEdge{ID: ekey, From: ed.From, To: ed.To, Type: ed.Type, Attrs: ed.Attrs})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"repo":   resolved,
		"method": findRes.Method,
		"text":   text,
		"nodes":  nodes,
		"edges":  edges,
	})
}

func (h *Handler) handleTests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	e, resolved, ok := h.resolveEngine(w, r)
	if !ok {
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	depth := parseIntParam(r, "depth", 3, 1, 5)

	res, err := e.AffectedTests(r.Context(), target, depth)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Build a highlighted subgraph of seed nodes + affected test files/functions.
	selected := map[string]*graph.Node{}
	statusByID := map[string]string{}
	for _, id := range res.SeedNodes {
		if n := e.G.Nodes[id]; n != nil {
			selected[id] = n
			statusByID[id] = "modified"
		}
	}
	testFileSet := map[string]bool{}
	for _, f := range res.TestFiles {
		testFileSet[filepath.ToSlash(f)] = true
	}
	for _, n := range e.G.Nodes {
		if n.Type == graph.NodeFile && testFileSet[filepath.ToSlash(n.Name)] {
			selected[n.ID] = n
			if statusByID[n.ID] == "" {
				statusByID[n.ID] = "added"
			}
		}
	}
	for _, tf := range res.TestFunctions {
		if n := e.G.Nodes[tf.ID]; n != nil {
			selected[n.ID] = n
			statusByID[n.ID] = "added"
		}
	}

	nodes := make([]GraphNode, 0, len(selected))
	for _, n := range selected {
		gn := toGraphNode(e.G, n)
		gn.DiffStatus = statusByID[n.ID]
		nodes = append(nodes, gn)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	var edges []GraphEdge
	seenEdge := map[string]bool{}
	for _, n := range selected {
		for _, ed := range e.G.EdgesOf(n.ID) {
			if ed.From != n.ID || selected[ed.To] == nil {
				continue
			}
			ekey := ed.From + "|" + ed.Type + "|" + ed.To
			if seenEdge[ekey] {
				continue
			}
			seenEdge[ekey] = true
			edges = append(edges, GraphEdge{ID: ekey, From: ed.From, To: ed.To, Type: ed.Type, Attrs: ed.Attrs})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"repo":           resolved,
		"target":         res.Target,
		"test_files":     res.TestFiles,
		"test_functions": res.TestFunctions,
		"commands":       res.Commands,
		"text":           res.Text,
		"nodes":          nodes,
		"edges":          edges,
	})
}

func (h *Handler) handleHotspots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	e, resolved, ok := h.resolveEngine(w, r)
	if !ok {
		return
	}
	limit := parseIntParam(r, "limit", 25, 1, 80)
	entries, err := e.Hotspots(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	selected := map[string]*graph.Node{}
	nodes := make([]GraphNode, 0, len(entries))
	for i, hs := range entries {
		n := e.G.Nodes[hs.NodeID]
		if n == nil {
			continue
		}
		selected[n.ID] = n
		gn := toGraphNode(e.G, n)
		if i < 5 {
			gn.DiffStatus = "removed" // red high-risk hotspot
		} else if i < 12 {
			gn.DiffStatus = "modified" // amber medium-risk hotspot
		} else {
			gn.DiffStatus = "caller" // blue moderate
		}
		nodes = append(nodes, gn)
	}

	var edges []GraphEdge
	seenEdge := map[string]bool{}
	for _, n := range selected {
		for _, ed := range e.G.EdgesOf(n.ID) {
			if ed.From != n.ID || selected[ed.To] == nil {
				continue
			}
			if ed.Type != graph.EdgeImports && ed.Type != graph.EdgeCoChanged && ed.Type != graph.EdgeDependsOn {
				continue
			}
			ekey := ed.From + "|" + ed.Type + "|" + ed.To
			if seenEdge[ekey] {
				continue
			}
			seenEdge[ekey] = true
			edges = append(edges, GraphEdge{ID: ekey, From: ed.From, To: ed.To, Type: ed.Type, Attrs: ed.Attrs})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"repo":     resolved,
		"hotspots": entries,
		"nodes":    nodes,
		"edges":    edges,
	})
}

func (h *Handler) handleNodeDiff(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	e, resolved, ok := h.resolveEngine(w, r)
	if !ok {
		return
	}
	file := strings.TrimSpace(r.URL.Query().Get("file"))
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if file == "" {
		writeErr(w, http.StatusBadRequest, "missing file parameter")
		return
	}
	diff, err := e.FileGitDiff(r.Context(), file, ref)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repo": resolved,
		"file": file,
		"ref":  ref,
		"diff": diff,
	})
}
