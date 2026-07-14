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
}

func New(g *graph.Graph, vs store.VectorStore, emb *embed.Client) *Engine {
	return &Engine{G: g, VS: vs, Emb: emb}
}

type Overview struct {
	Source     string         `json:"source"`
	NodeCounts map[string]int `json:"node_counts"`
	Tables     []TableSummary `json:"tables"`
}

type TableSummary struct {
	Name       string   `json:"name"`
	RowCount   string   `json:"row_count,omitempty"`
	Columns    int      `json:"columns"`
	References []string `json:"references,omitempty"`
}

type FindHit struct {
	ID    string  `json:"id"`
	Type  string  `json:"type"`
	Name  string  `json:"name"`
	Score float32 `json:"score"`
	Text  string  `json:"text"`
}

type FindResult struct {
	Method string    `json:"method"` // "semantic" or "keyword"
	Hits   []FindHit `json:"hits"`
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

type Context struct {
	Tables []Description `json:"tables"`
	Hint   string        `json:"hint"`
}

func (e *Engine) Overview() Overview {
	out := Overview{Source: e.G.Source, NodeCounts: map[string]int{}}
	for _, n := range e.G.Nodes {
		out.NodeCounts[n.Type]++
	}
	for _, t := range e.G.NodesByType(graph.NodeTable) {
		ts := TableSummary{Name: t.Name, RowCount: t.Attrs["row_count"]}
		for _, ed := range e.G.EdgesOf(t.ID) {
			switch {
			case ed.Type == graph.EdgeHasColumn && ed.From == t.ID:
				ts.Columns++
			case ed.Type == graph.EdgeReferences && ed.From == t.ID:
				if n := e.G.Nodes[ed.To]; n != nil {
					ts.References = append(ts.References, n.Name)
				}
			}
		}
		out.Tables = append(out.Tables, ts)
	}
	return out
}

// Find locates nodes matching a natural-language query, semantically when a
// vector index is available, falling back to keyword scoring.
func (e *Engine) Find(ctx context.Context, query, nodeType string, limit int) (FindResult, error) {
	if limit <= 0 {
		limit = 8
	}
	if e.VS != nil && e.Emb != nil {
		vecs, err := e.Emb.Embed(ctx, []string{query})
		if err == nil {
			hits, err := e.VS.Search(ctx, vecs[0], limit, nodeType)
			if err == nil {
				out := FindResult{Method: "semantic"}
				for _, h := range hits {
					out.Hits = append(out.Hits, FindHit{
						ID: h.NodeID, Type: h.Type, Name: h.Name,
						Score: h.Score, Text: e.G.NodeText(h.NodeID),
					})
				}
				return out, nil
			}
		}
		// fall through to keyword on any semantic failure
	}
	return e.keywordFind(query, nodeType, limit), nil
}

func (e *Engine) keywordFind(query, typ string, limit int) FindResult {
	terms := strings.Fields(strings.ToLower(query))
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
		for _, prefix := range []string{"table:", "column:", "view:", "index:"} {
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
	resolve := func(name string) string {
		if e.G.Nodes["table:"+name] == nil && e.G.Nodes["table:public."+name] != nil {
			return "table:public." + name
		}
		return "table:" + name
	}
	from, to := resolve(fromName), resolve(toName)
	for name, id := range map[string]string{fromName: from, toName: to} {
		if e.G.Nodes[id] == nil {
			return JoinPath{Found: false, Hint: fmt.Sprintf("table %q not found; use find to locate it", name)}
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
		return JoinPath{Found: false, Hint: "no foreign-key path between these tables; they may be unrelated or joined through data (not schema)"}
	}
	out := JoinPath{Found: true}
	cur := from
	for _, ed := range path {
		step := JoinStep{FromColumn: ed.Attrs["from_column"], ToColumn: ed.Attrs["to_column"]}
		next := ed.To
		if next == cur {
			next = ed.From
			// edge traversed backwards: swap column roles
			step.FromColumn, step.ToColumn = step.ToColumn, step.FromColumn
		}
		step.FromTable = strings.TrimPrefix(cur, "table:")
		step.ToTable = strings.TrimPrefix(next, "table:")
		out.Steps = append(out.Steps, step)
		cur = next
	}
	var joins []string
	for _, st := range out.Steps {
		joins = append(joins, fmt.Sprintf("JOIN %s ON %s.%s = %s.%s",
			st.ToTable, st.FromTable, st.FromColumn, st.ToTable, st.ToColumn))
	}
	out.Hint = strings.Join(joins, " ")
	return out
}

// QuestionContext builds the context pack for a data question: the most
// relevant tables fully described, with column detail inlined.
func (e *Engine) QuestionContext(ctx context.Context, question string, limit int) (Context, error) {
	if limit <= 0 {
		limit = 4
	}
	found, err := e.Find(ctx, question, graph.NodeTable, limit)
	if err != nil {
		return Context{}, err
	}
	out := Context{Hint: "Tables most relevant to the question, fully described. Join columns are in the REFERENCES edges (from_column/to_column)."}
	for _, h := range found.Hits {
		d, err := e.Describe(h.ID)
		if err != nil {
			continue
		}
		// inline column detail so the caller gets everything in one call
		for i, ed := range d.Edges {
			if ed.Type == graph.EdgeHasColumn && ed.Dir == "out" {
				if col := e.G.Nodes[ed.Other]; col != nil {
					d.Edges[i].Attrs = col.Attrs
				}
			}
		}
		out.Tables = append(out.Tables, d)
	}
	return out, nil
}
