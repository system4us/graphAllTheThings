package engine

// Compact text renderers. MCP tools return this instead of structured JSON:
// same information, 5-10x fewer tokens. Format is line-oriented so agents
// can quote fragments directly into SQL.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"graphallthethings/internal/graph"
)

var timestampCols = map[string]bool{
	"created_at": true, "updated_at": true, "deleted_at": true,
	"createdat": true, "updatedat": true, "deletedat": true,
}

// RenderTable renders one table/view as an annotated compact block.
func (e *Engine) RenderTable(id string) (string, error) {
	d, err := e.Describe(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s (%s", d.Name, d.Type)
	if rc := d.Attrs["row_count"]; rc != "" {
		fmt.Fprintf(&b, ", %s rows", rc)
	} else if rc := d.Attrs["row_count_estimate"]; rc != "" && rc != "0" {
		fmt.Fprintf(&b, ", ~%s rows", rc)
	}
	b.WriteString(")")
	if c := d.Attrs["comment"]; c != "" {
		b.WriteString(" -- " + c)
	}
	b.WriteString("\n")

	var timestamps []string
	softDelete := false
	for _, ed := range d.Edges {
		if ed.Type != graph.EdgeHasColumn || ed.Dir != "out" {
			continue
		}
		col := e.G.Nodes[ed.Other]
		if col == nil {
			continue
		}
		short := col.Name[strings.LastIndex(col.Name, ".")+1:]
		if timestampCols[strings.ToLower(short)] {
			timestamps = append(timestamps, short)
			if strings.HasPrefix(strings.ToLower(short), "deleted") {
				softDelete = true
			}
			continue
		}
		b.WriteString(e.renderColumn(col, short))
	}
	if len(timestamps) > 0 {
		b.WriteString("timestamps: " + strings.Join(timestamps, ", "))
		if softDelete {
			b.WriteString(" (soft delete: filter deleted_at IS NULL)")
		}
		b.WriteString("\n")
	}

	var refs, refBy []string
	for _, ed := range d.Edges {
		if ed.Type != graph.EdgeReferences {
			continue
		}
		other := strings.TrimPrefix(strings.TrimPrefix(ed.Other, "table:"), "view:")
		if ed.Dir == "out" {
			refs = append(refs, fmt.Sprintf("%s (%s→%s)", other, ed.Attrs["from_column"], ed.Attrs["to_column"]))
		} else {
			refBy = append(refBy, fmt.Sprintf("%s (%s)", other, ed.Attrs["from_column"]))
		}
	}
	if len(refs) > 0 {
		b.WriteString("references: " + strings.Join(refs, ", ") + "\n")
	}
	if len(refBy) > 0 {
		b.WriteString("referenced by: " + strings.Join(refBy, ", ") + "\n")
	}
	return b.String(), nil
}

func (e *Engine) renderColumn(col *graph.Node, short string) string {
	line := short
	if dt := col.Attrs["data_type"]; dt != "" {
		if ev := col.Attrs["enum_values"]; ev != "" {
			line += " enum(" + ev + ")"
		} else {
			line += " " + dt
		}
	}
	if col.Attrs["primary_key"] == "true" {
		line += " PK"
	} else {
		if col.Attrs["unique"] == "true" {
			line += " unique"
		}
		if col.Attrs["not_null"] == "true" {
			line += " NN"
		}
	}
	// FK target from the column-level edge
	for _, ed := range e.G.EdgesOf(col.ID) {
		if ed.Type == graph.EdgeForeignKey && ed.From == col.ID {
			line += " → " + strings.TrimPrefix(ed.To, "column:")
			break
		}
	}
	if dflt := col.Attrs["default"]; dflt != "" && !strings.Contains(dflt, "nextval") {
		if len(dflt) > 40 {
			dflt = dflt[:40] + "…"
		}
		line += " = " + dflt
	}
	if c := col.Attrs["comment"]; c != "" {
		line += " -- " + c
	}
	return line + "\n"
}

// ContextPack answers a data question with one compact block: the most
// relevant tables plus the join paths between them. Designed so one tool
// call is enough to write SQL.
func (e *Engine) ContextPack(ctx context.Context, question string, limit int) (string, error) {
	if limit <= 0 {
		limit = 4
	}
	found, err := e.Find(ctx, question, graph.NodeTable, limit)
	if err != nil {
		return "", err
	}
	if len(found.Hits) == 0 {
		return "no tables matched the question; try find_entities with different wording", nil
	}
	var b strings.Builder
	var names []string
	for _, h := range found.Hits {
		s, err := e.RenderTable(h.ID)
		if err != nil {
			continue
		}
		b.WriteString(s + "\n")
		names = append(names, strings.TrimPrefix(h.ID, "table:"))
	}
	// join paths between every pair of returned tables, deduplicated
	joins := map[string]bool{}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			jp := e.Join(names[i], names[j])
			if !jp.Found || len(jp.Steps) > 3 {
				continue
			}
			for _, st := range jp.Steps {
				// canonical order so a.x = b.y and b.y = a.x dedupe
				l := st.FromTable + "." + st.FromColumn
				r := st.ToTable + "." + st.ToColumn
				if l > r {
					l, r = r, l
				}
				joins[l+" = "+r] = true
			}
		}
	}
	if len(joins) > 0 {
		keys := make([]string, 0, len(joins))
		for k := range joins {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("## joins\n" + strings.Join(keys, "\n") + "\n")
	}
	return b.String(), nil
}

// RenderFind renders search hits one per line. Table hits get a short
// column count + references summary instead of the full column list.
func (e *Engine) RenderFind(res FindResult) string {
	if len(res.Hits) == 0 {
		return "no matches"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "(%s)\n", res.Method)
	for _, h := range res.Hits {
		fmt.Fprintf(&b, "%.2f %s %s", h.Score, h.Type, strings.TrimPrefix(strings.TrimPrefix(h.ID, h.Type+":"), "public."))
		switch h.Type {
		case graph.NodeTable, graph.NodeView:
			cols, refs := 0, []string{}
			for _, ed := range e.G.EdgesOf(h.ID) {
				switch {
				case ed.Type == graph.EdgeHasColumn && ed.From == h.ID:
					cols++
				case ed.Type == graph.EdgeReferences && ed.From == h.ID:
					refs = append(refs, strings.TrimPrefix(strings.TrimPrefix(ed.To, "table:"), "public."))
				}
			}
			fmt.Fprintf(&b, " (%d cols", cols)
			if len(refs) > 0 {
				fmt.Fprintf(&b, "; → %s", strings.Join(refs, ", "))
			}
			b.WriteString(")")
		case graph.NodeColumn:
			if n := e.G.Nodes[h.ID]; n != nil {
				if ev := n.Attrs["enum_values"]; ev != "" {
					fmt.Fprintf(&b, " enum(%s)", ev)
				} else if dt := n.Attrs["data_type"]; dt != "" {
					b.WriteString(" " + dt)
				}
				if c := n.Attrs["comment"]; c != "" {
					b.WriteString(" -- " + c)
				}
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// RenderJoin renders a join path result.
func (e *Engine) RenderJoin(jp JoinPath) string {
	if !jp.Found {
		return jp.Hint
	}
	return jp.Hint
}

// RenderOverview renders the graph summary.
func (e *Engine) RenderOverview() string {
	ov := e.Overview()
	var b strings.Builder
	fmt.Fprintf(&b, "source: %s\n", ov.Source)
	var kinds []string
	for k := range ov.NodeCounts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Fprintf(&b, "%s: %d  ", k, ov.NodeCounts[k])
	}
	b.WriteString("\ntables:\n")
	for _, t := range ov.Tables {
		line := fmt.Sprintf("%s (%d cols", strings.TrimPrefix(t.Name, "public."), t.Columns)
		if t.RowCount != "" {
			line += ", " + t.RowCount + " rows"
		}
		line += ")"
		if len(t.References) > 0 {
			var short []string
			for _, r := range t.References {
				short = append(short, strings.TrimPrefix(r, "public."))
			}
			line += " → " + strings.Join(short, ", ")
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}
