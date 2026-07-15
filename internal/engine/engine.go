// Package engine implements the query operations over a metadata graph:
// overview, semantic/keyword find, describe, join paths, and question
// context packs. Both the CLI and the MCP server are thin wrappers over it.
package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"graphallthethings/internal/embed"
	"graphallthethings/internal/graph"
	"graphallthethings/internal/store"
)

type Engine struct {
	G   *graph.Graph
	VS  store.VectorStore // nil disables semantic search
	Emb *embed.Client
	// Chat is an optional local chat model used to expand a query into a
	// couple of alternate phrasings before embedding, so a conceptual query
	// phrased in the user's words ("prevent a duplicate request") has a shot
	// at matching code that only ever says it in implementation vocabulary
	// ("idempotency key"). nil disables expansion — Find behaves exactly as
	// it did before Chat existed.
	Chat *embed.ChatClient
	// FTS is an optional indexed keyword search (SQLite FTS5) returning node
	// ids best-first. keywordFind uses it instead of scanning every NodeText
	// in memory; nil or an empty result falls back to the scan.
	FTS func(query, nodeType string, limit int) []string
}

func New(g *graph.Graph, vs store.VectorStore, emb *embed.Client) *Engine {
	return &Engine{G: g, VS: vs, Emb: emb}
}

// queryExpansions is how many alternate phrasings Chat is asked for. Kept
// small: each extra variant is a full embed-and-search pass, and expansion
// exists to bridge one vocabulary gap, not to brute-force every synonym.
const queryExpansions = 2

type Overview struct {
	Source     string            `json:"source"`
	NodeCounts map[string]int    `json:"node_counts"`
	Tables     []TableSummary    `json:"tables"`              // tables (SQL) or schemas (API)
	Endpoints  []EndpointSummary `json:"endpoints,omitempty"` // API only
}

type TableSummary struct {
	Name       string   `json:"name"`
	RowCount   string   `json:"row_count,omitempty"`
	Columns    int      `json:"columns"` // columns (SQL) or properties (API)
	References []string `json:"references,omitempty"`
}

type EndpointSummary struct {
	Name    string   `json:"name"` // "GET /users/{id}"
	Summary string   `json:"summary,omitempty"`
	Schemas []string `json:"schemas,omitempty"` // schemas the endpoint accepts/returns
}

type FindHit struct {
	ID    string  `json:"id"`
	Type  string  `json:"type"`
	Name  string  `json:"name"`
	Score float32 `json:"score"`
	Text  string  `json:"text"`
}

type FindResult struct {
	Method string    `json:"method"` // "semantic", "keyword", "hybrid" or "name"
	Hits   []FindHit `json:"hits"`
}

// nameFind matches the query against node names: exact > basename/prefix >
// substring, case-insensitive. External stubs are skipped.
func (e *Engine) nameFind(query, nodeType string, limit int) FindResult {
	q := strings.ToLower(query)
	out := FindResult{Method: "name"}
	for id, n := range e.G.Nodes {
		if nodeType != "" && n.Type != nodeType {
			continue
		}
		if n.Attrs["external"] == "true" {
			continue
		}
		lname := strings.ToLower(n.Name)
		var score float32
		switch {
		case lname == q:
			score = 1.0
		case strings.HasSuffix(lname, "/"+q) || strings.HasPrefix(lname, q):
			score = 0.9
		case strings.Contains(lname, q):
			score = 0.8
		default:
			continue
		}
		out.Hits = append(out.Hits, FindHit{
			ID: id, Type: n.Type, Name: n.Name,
			Score: score, Text: e.G.NodeText(id),
		})
	}
	sort.Slice(out.Hits, func(i, j int) bool {
		if out.Hits[i].Score != out.Hits[j].Score {
			return out.Hits[i].Score > out.Hits[j].Score
		}
		return out.Hits[i].Name < out.Hits[j].Name
	})
	if len(out.Hits) > limit {
		out.Hits = out.Hits[:limit]
	}
	return out
}

type EdgeInfo struct {
	Type  string            `json:"type"`
	Other string            `json:"other"`
	Dir   string            `json:"dir"` // "out" or "in"
	Attrs map[string]string `json:"attrs,omitempty"`
}

type Description struct {
	ID    string            `json:"id"`
	Type  string            `json:"type"`
	Name  string            `json:"name"`
	Attrs map[string]string `json:"attrs,omitempty"`
	Edges []EdgeInfo        `json:"edges"`
}

type JoinStep struct {
	FromTable  string `json:"from_table"`
	ToTable    string `json:"to_table"`
	FromColumn string `json:"from_column,omitempty"`
	ToColumn   string `json:"to_column,omitempty"`
}

type JoinPath struct {
	Found bool       `json:"found"`
	Steps []JoinStep `json:"steps,omitempty"`
	Hint  string     `json:"hint,omitempty"`
}

func (e *Engine) Overview() Overview {
	out := Overview{Source: e.G.Source, NodeCounts: map[string]int{}}
	for _, n := range e.G.Nodes {
		out.NodeCounts[n.Type]++
	}
	// Containers are tables for a SQL source, schemas for an API spec; both count
	// their members (columns / properties) and their outgoing REFERENCES.
	containerType := graph.NodeTable
	if e.G.IsAPI() {
		containerType = graph.NodeSchema
	}
	for _, t := range e.G.NodesByType(containerType) {
		ts := TableSummary{Name: t.Name, RowCount: t.Attrs["row_count"]}
		for _, ed := range e.G.EdgesOf(t.ID) {
			switch {
			case (ed.Type == graph.EdgeHasColumn || ed.Type == graph.EdgeHasProperty) && ed.From == t.ID:
				ts.Columns++
			case ed.Type == graph.EdgeReferences && ed.From == t.ID:
				if n := e.G.Nodes[ed.To]; n != nil {
					ts.References = append(ts.References, n.Name)
				}
			}
		}
		out.Tables = append(out.Tables, ts)
	}
	for _, ep := range e.G.NodesByType(graph.NodeEndpoint) {
		es := EndpointSummary{Name: ep.Name, Summary: ep.Attrs["comment"]}
		for _, ed := range e.G.EdgesOf(ep.ID) {
			if (ed.Type == graph.EdgeAccepts || ed.Type == graph.EdgeRespondsWith) && ed.From == ep.ID {
				if n := e.G.Nodes[ed.To]; n != nil {
					es.Schemas = append(es.Schemas, n.Name)
				}
			}
		}
		out.Endpoints = append(out.Endpoints, es)
	}
	return out
}

// Find locates nodes matching a natural-language query, semantically when a
// vector index is available, falling back to keyword scoring.
func (e *Engine) Find(ctx context.Context, query, nodeType string, limit int) (FindResult, error) {
	if limit <= 0 {
		limit = 8
	}
	// A spaceless query is an identifier or path, not a description: exact and
	// substring name matches beat any semantic neighbor ("product.schema" must
	// return the file, not a conceptually similar validator).
	if q := strings.TrimSpace(query); q != "" && !strings.ContainsAny(q, " \t") {
		if r := e.nameFind(q, nodeType, limit); len(r.Hits) > 0 {
			return r, nil
		}
	}
	if e.VS != nil && e.Emb != nil {
		method, hits, err := e.semanticSearch(ctx, query, nodeType, limit)
		if err == nil {
			out := FindResult{Method: method}
			seen := map[string]bool{}
			for _, h := range hits {
				// Skip vectors for nodes no longer in the graph (stale index).
				if e.G.Nodes[h.NodeID] == nil {
					continue
				}
				seen[h.NodeID] = true
				out.Hits = append(out.Hits, FindHit{
					ID: h.NodeID, Type: h.Type, Name: h.Name,
					Score: h.Score, Text: e.G.NodeText(h.NodeID),
				})
			}
			// Hybrid: nodes added after the last index run have no vector,
			// so a pure semantic answer silently misses them. Merge keyword
			// hits — a query term matching the node *name* is high-signal
			// and may evict the semantic tail; others only fill spare slots.
			var nameHits, textHits []FindHit
			terms := strings.Fields(strings.ToLower(query))
			for _, kh := range e.keywordFind(query, nodeType, limit).Hits {
				if seen[kh.ID] {
					continue
				}
				lname := strings.ToLower(kh.Name)
				matched := false
				for _, t := range terms {
					if strings.Contains(lname, t) {
						matched = true
						break
					}
				}
				if matched {
					nameHits = append(nameHits, kh)
				} else {
					textHits = append(textHits, kh)
				}
			}
			if reserve := min(len(nameHits), 3); len(out.Hits) > limit-reserve {
				out.Hits = out.Hits[:limit-reserve]
			}
			merged := false
			for _, kh := range append(nameHits, textHits...) {
				if len(out.Hits) >= limit {
					break
				}
				out.Hits = append(out.Hits, kh)
				merged = true
			}
			// "hybrid" replaces "semantic", but must not erase "+expanded" —
			// that tag is what tells a caller "some of these hits came from
			// a paraphrase, not your literal words," which stays true
			// whether or not keyword hits also got merged in.
			if merged {
				out.Method = strings.Replace(out.Method, "semantic", "hybrid", 1)
			}
			return out, nil
		}
		// fall through to keyword on any semantic failure
	}
	return e.keywordFind(query, nodeType, limit), nil
}

// semanticSearch embeds query (expanded into a couple of alternate
// phrasings first when e.Chat is set) and returns the union of every
// variant's hits, deduped by node id keeping each node's best score, sorted
// and capped to limit. Method is "semantic" when only the original query
// was used, "semantic+expanded" when at least one alternate phrasing
// contributed a hit that wasn't already found by the original.
func (e *Engine) semanticSearch(ctx context.Context, query, nodeType string, limit int) (string, []store.Hit, error) {
	variants := []string{query}
	if e.Chat != nil {
		variants = e.Chat.Expand(ctx, query, queryExpansions)
	}
	vecs, err := e.Emb.Embed(ctx, variants)
	if err != nil {
		return "", nil, err
	}
	best := map[string]store.Hit{}
	expandedContributed := false
	var lastSearchErr error
	for i, vec := range vecs {
		hits, searchErr := e.VS.Search(ctx, vec, limit, nodeType)
		if searchErr != nil {
			lastSearchErr = searchErr
			continue
		}
		for _, h := range hits {
			prev, ok := best[h.NodeID]
			if i > 0 && !ok {
				expandedContributed = true
			}
			if !ok || h.Score > prev.Score {
				best[h.NodeID] = h
			}
		}
	}
	if len(best) == 0 {
		// Every variant's search failed outright (not "no matches" — an
		// actual error, e.g. the store is unreachable): surface it instead
		// of silently returning an empty result set. A variant that merely
		// found zero hits isn't an error and isn't distinguished here.
		return "", nil, lastSearchErr
	}
	out := make([]store.Hit, 0, len(best))
	for _, h := range best {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	method := "semantic"
	if expandedContributed {
		method = "semantic+expanded"
	}
	return method, out, nil
}

func (e *Engine) keywordFind(query, typ string, limit int) FindResult {
	terms := strings.Fields(strings.ToLower(query))

	if e.FTS != nil {
		if ids := e.FTS(query, typ, limit); len(ids) > 0 {
			out := FindResult{Method: "keyword-fts"}
			for _, id := range ids {
				n := e.G.Nodes[id]
				if n == nil {
					continue
				}
				text := e.G.NodeText(id)
				var sc float32
				lower := strings.ToLower(n.Name + " " + text)
				for _, t := range terms {
					if strings.Contains(lower, t) {
						sc++
					}
				}
				out.Hits = append(out.Hits, FindHit{ID: id, Type: n.Type, Name: n.Name, Score: sc, Text: text})
			}
			if len(out.Hits) > 0 {
				return out
			}
		}
	}

	type scored struct {
		hit   FindHit
		score float32
	}
	var results []scored
	for id, n := range e.G.Nodes {
		if typ != "" && n.Type != typ {
			continue
		}
		text := strings.ToLower(n.Name + " " + e.G.NodeText(id))
		var sc float32
		for _, t := range terms {
			if strings.Contains(text, t) {
				sc++
			}
		}
		if sc > 0 {
			results = append(results, scored{FindHit{ID: id, Type: n.Type, Name: n.Name, Score: sc, Text: e.G.NodeText(id)}, sc})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	if len(results) > limit {
		results = results[:limit]
	}
	out := FindResult{Method: "keyword"}
	for _, r := range results {
		out.Hits = append(out.Hits, r.hit)
	}
	return out
}

// Describe returns one node in full. Bare names are tolerated:
// "users" resolves to table:users or table:public.users.
func (e *Engine) Describe(id string) (Description, error) {
	n := e.G.Nodes[id]
	if n == nil {
	outer:
		for _, prefix := range []string{"table:", "column:", "view:", "index:", "schema:", "property:", "endpoint:", "api:"} {
			for _, cand := range []string{prefix + id, prefix + "public." + id} {
				if e.G.Nodes[cand] != nil {
					n = e.G.Nodes[cand]
					id = cand
					break outer
				}
			}
		}
		if n == nil {
			return Description{}, fmt.Errorf("node %q not found; use find or overview to list ids", id)
		}
	}
	out := Description{ID: id, Type: n.Type, Name: n.Name, Attrs: n.Attrs}
	for _, ed := range e.G.EdgesOf(id) {
		eo := EdgeInfo{Type: ed.Type, Attrs: ed.Attrs}
		if ed.From == id {
			eo.Dir = "out"
			eo.Other = ed.To
		} else {
			eo.Dir = "in"
			eo.Other = ed.From
		}
		out.Edges = append(out.Edges, eo)
	}
	return out, nil
}

// Join finds the cheapest foreign-key join path between two tables,
// penalizing hub tables (tenant_id-style FKs every table carries) whose
// joins are usually semantically wrong even at fewer hops.
func (e *Engine) Join(fromName, toName string) JoinPath {
	prefix := "table:"
	if e.G.IsAPI() {
		prefix = "schema:"
	}
	resolve := func(name string) string {
		if e.G.Nodes[prefix+name] == nil && e.G.Nodes[prefix+"public."+name] != nil {
			return prefix + "public." + name
		}
		return prefix + name
	}
	from, to := resolve(fromName), resolve(toName)
	kind := "table"
	if e.G.IsAPI() {
		kind = "schema"
	}
	for name, id := range map[string]string{fromName: from, toName: to} {
		if e.G.Nodes[id] == nil {
			return JoinPath{Found: false, Hint: fmt.Sprintf("%s %q not found; use find to locate it", kind, name)}
		}
	}
	hubCost := func(id string) float64 {
		var deg float64
		for _, ed := range e.G.EdgesOf(id) {
			if ed.Type == graph.EdgeReferences {
				deg++
			}
		}
		return deg
	}
	path := e.G.CheapestPath(from, to, hubCost, graph.EdgeReferences)
	if path == nil {
		miss := "no foreign-key path between these tables; they may be unrelated or joined through data (not schema)"
		if e.G.IsAPI() {
			miss = "no $ref path between these schemas; they may be unrelated"
		}
		return JoinPath{Found: false, Hint: miss}
	}
	out := JoinPath{Found: true}
	cur := from
	for _, ed := range path {
		var step JoinStep
		next := ed.To
		if e.G.IsAPI() {
			step.FromColumn = ed.Attrs["from_property"]
			if ed.Attrs["cardinality"] == "array" && step.FromColumn != "" {
				step.FromColumn += "[]"
			}
		} else {
			step.FromColumn, step.ToColumn = ed.Attrs["from_column"], ed.Attrs["to_column"]
		}
		if next == cur {
			next = ed.From
			if !e.G.IsAPI() {
				// edge traversed backwards: swap column roles
				step.FromColumn, step.ToColumn = step.ToColumn, step.FromColumn
			}
		}
		step.FromTable = strings.TrimPrefix(cur, prefix)
		step.ToTable = strings.TrimPrefix(next, prefix)
		out.Steps = append(out.Steps, step)
		cur = next
	}
	// API: render the reference chain (A → B via property). SQL: render a ready
	// JOIN clause with the exact key columns.
	if e.G.IsAPI() {
		parts := []string{strings.TrimPrefix(from, prefix)}
		for _, st := range out.Steps {
			if st.FromColumn != "" {
				parts = append(parts, fmt.Sprintf("→ %s (%s)", st.ToTable, st.FromColumn))
			} else {
				parts = append(parts, "→ "+st.ToTable)
			}
		}
		out.Hint = strings.Join(parts, " ")
		return out
	}
	var joins []string
	for _, st := range out.Steps {
		joins = append(joins, fmt.Sprintf("JOIN %s ON %s.%s = %s.%s",
			st.ToTable, st.FromTable, st.FromColumn, st.ToTable, st.ToColumn))
	}
	out.Hint = strings.Join(joins, " ")
	return out
}
