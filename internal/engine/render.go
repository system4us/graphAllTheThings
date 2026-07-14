package engine

// Compact text renderers. MCP tools return this instead of structured JSON:
// same information, 5-10x fewer tokens. Format is line-oriented so agents
// can quote fragments directly into SQL.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"graphallthethings/internal/graph"
)

var timestampCols = map[string]bool{
	"created_at": true, "updated_at": true, "deleted_at": true,
	"createdat": true, "updatedat": true, "deletedat": true,
}

// Render renders one node in the compact form appropriate to its type:
// tables/views as SQL blocks, schemas and endpoints as API blocks. It is the
// entry point behind describe_entity so a single tool works for both sources.
func (e *Engine) Render(id string) (string, error) {
	d, err := e.Describe(id)
	if err != nil {
		return "", err
	}
	switch d.Type {
	case graph.NodeSchema:
		return e.RenderSchema(d.ID)
	case graph.NodeEndpoint:
		return e.RenderEndpoint(d.ID)
	default:
		return e.RenderTable(d.ID)
	}
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
	if en := d.Attrs["entity_note"]; en != "" {
		b.WriteString("-- note: " + en + "\n")
	}

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
	if df := d.Attrs["default_filter"]; df != "" {
		b.WriteString("default filter: " + df + "\n")
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

// apiBaseURL returns the server base URL recorded on the api node, or "".
func (e *Engine) apiBaseURL() string {
	for _, n := range e.G.NodesByType(graph.NodeAPI) {
		return n.Attrs["base_url"]
	}
	return ""
}

// RenderSchema renders one API component schema: its properties (types,
// enums, required flags, $ref targets), the schemas it references, and the
// endpoints that consume or produce it.
func (e *Engine) RenderSchema(id string) (string, error) {
	d, err := e.Describe(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s (schema)", d.Name)
	if c := d.Attrs["comment"]; c != "" {
		b.WriteString(" -- " + c)
	}
	b.WriteString("\n")
	if en := d.Attrs["entity_note"]; en != "" {
		b.WriteString("-- note: " + en + "\n")
	}
	// An enum-only schema (a named value domain) has no properties, just values.
	if ev := d.Attrs["enum_values"]; ev != "" {
		b.WriteString("values: " + ev + "\n")
	}
	if src := d.Attrs["source"]; src != "" {
		b.WriteString("source: " + src + "\n")
	}

	var required []string
	for _, ed := range d.Edges {
		if ed.Type != graph.EdgeHasProperty || ed.Dir != "out" {
			continue
		}
		prop := e.G.Nodes[ed.Other]
		if prop == nil {
			continue
		}
		short := prop.Name[strings.LastIndex(prop.Name, ".")+1:]
		if prop.Attrs["not_null"] == "true" {
			required = append(required, short)
		}
		b.WriteString(e.renderColumn(prop, short))
	}
	if len(required) > 0 {
		b.WriteString("required: " + strings.Join(required, ", ") + "\n")
	}
	if df := d.Attrs["default_filter"]; df != "" {
		b.WriteString("default filter: " + df + "\n")
	}

	var refs, refBy, usedBy []string
	for _, ed := range d.Edges {
		switch {
		case ed.Type == graph.EdgeReferences && ed.Dir == "out":
			ref := strings.TrimPrefix(ed.Other, "schema:")
			if via := ed.Attrs["from_property"]; via != "" {
				if ed.Attrs["cardinality"] == "array" {
					via += "[]"
				}
				ref += " (via " + via + ")"
			}
			refs = append(refs, ref)
		case ed.Type == graph.EdgeReferences && ed.Dir == "in":
			refBy = append(refBy, strings.TrimPrefix(ed.Other, "schema:"))
		case (ed.Type == graph.EdgeAccepts || ed.Type == graph.EdgeRespondsWith) && ed.Dir == "in":
			role := "request"
			if ed.Type == graph.EdgeRespondsWith {
				role = "response"
			}
			usedBy = append(usedBy, strings.TrimPrefix(ed.Other, "endpoint:")+" ("+role+")")
		}
	}
	if len(refs) > 0 {
		b.WriteString("references: " + strings.Join(refs, ", ") + "\n")
	}
	if len(refBy) > 0 {
		b.WriteString("referenced by: " + strings.Join(refBy, ", ") + "\n")
	}
	if len(usedBy) > 0 {
		b.WriteString("used by: " + strings.Join(usedBy, ", ") + "\n")
	}
	return b.String(), nil
}

// RenderEndpoint renders one HTTP operation: its parameters and the request /
// response schemas it accepts and returns.
func (e *Engine) RenderEndpoint(id string) (string, error) {
	d, err := e.Describe(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s (endpoint)", d.Name)
	if c := d.Attrs["comment"]; c != "" {
		b.WriteString(" -- " + c)
	}
	b.WriteString("\n")
	// The full URL a curl needs: base URL (from the api node) + the path.
	if base := e.apiBaseURL(); base != "" {
		if p := d.Attrs["path"]; p != "" {
			b.WriteString("url: " + base + p + "\n")
		}
	}
	if a := d.Attrs["auth"]; a != "" {
		b.WriteString("auth: " + a + "\n")
	}
	if src := d.Attrs["source"]; src != "" {
		b.WriteString("source: " + src + "\n")
	}
	if t := d.Attrs["tags"]; t != "" {
		b.WriteString("tags: " + t + "\n")
	}
	for _, p := range []struct{ key, label string }{
		{"path_params", "path params"}, {"query_params", "query params"}, {"header_params", "header params"},
	} {
		if v := d.Attrs[p.key]; v != "" {
			b.WriteString(p.label + ": " + v + "\n")
		}
	}

	var accepts, returns []string
	for _, ed := range d.Edges {
		if ed.Dir != "out" {
			continue
		}
		schema := strings.TrimPrefix(ed.Other, "schema:")
		if ed.Attrs["cardinality"] == "array" {
			schema = "array<" + schema + ">"
		}
		switch ed.Type {
		case graph.EdgeAccepts:
			accepts = append(accepts, schema)
		case graph.EdgeRespondsWith:
			if st := ed.Attrs["status"]; st != "" {
				schema += " (" + st + ")"
			}
			returns = append(returns, schema)
		}
	}
	if len(accepts) > 0 {
		b.WriteString("accepts: " + strings.Join(accepts, ", ") + "\n")
	}
	if len(returns) > 0 {
		b.WriteString("returns: " + strings.Join(returns, ", ") + "\n")
	}
	return b.String(), nil
}

// ContextPack answers a data question with one compact block: the most
// relevant tables plus the join paths between them. Designed so one tool
// call is enough to write SQL.
func (e *Engine) ContextPack(ctx context.Context, question string, limit int) (string, error) {
	if limit <= 0 {
		limit = 4
	}
	if e.G.IsAPI() {
		return e.apiContextPack(ctx, question, limit)
	}
	found, err := e.Find(ctx, question, graph.NodeTable, limit)
	if err != nil {
		return "", err
	}
	if len(found.Hits) == 0 {
		return "no tables matched the question; try find_entities with different wording", nil
	}
	var b strings.Builder
	if fr := e.G.Freshness(time.Now()); fr != "" {
		fmt.Fprintf(&b, "# source: %s\n", fr)
	}
	if dl := e.G.Dialect(); dl != "" {
		fmt.Fprintf(&b, "dialect: %s\n\n", dl)
	}
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

// apiContextPack answers a question against an API graph: the most relevant
// schemas and endpoints rendered in full, plus the $ref relationships between
// the returned schemas. The API-side analogue of the SQL context pack.
func (e *Engine) apiContextPack(ctx context.Context, question string, limit int) (string, error) {
	schemas, err := e.Find(ctx, question, graph.NodeSchema, limit)
	if err != nil {
		return "", err
	}
	endpoints, err := e.Find(ctx, question, graph.NodeEndpoint, limit)
	if err != nil {
		return "", err
	}
	if len(schemas.Hits) == 0 && len(endpoints.Hits) == 0 {
		return "no schemas or endpoints matched the question; try find_entities with different wording", nil
	}
	var b strings.Builder
	if fr := e.G.Freshness(time.Now()); fr != "" {
		fmt.Fprintf(&b, "# source: %s\n\n", fr)
	}
	var names []string
	for _, h := range endpoints.Hits {
		if s, err := e.RenderEndpoint(h.ID); err == nil {
			b.WriteString(s + "\n")
		}
	}
	for _, h := range schemas.Hits {
		if s, err := e.RenderSchema(h.ID); err == nil {
			b.WriteString(s + "\n")
			names = append(names, strings.TrimPrefix(h.ID, "schema:"))
		}
	}
	// $ref chains between every pair of returned schemas, deduplicated.
	chains := map[string]bool{}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			jp := e.Join(names[i], names[j])
			if jp.Found && len(jp.Steps) <= 3 {
				chains[jp.Hint] = true
			}
		}
	}
	if len(chains) > 0 {
		keys := make([]string, 0, len(chains))
		for k := range chains {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("## relationships\n" + strings.Join(keys, "\n") + "\n")
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
		case graph.NodeTable, graph.NodeView, graph.NodeSchema:
			cols, refs := 0, []string{}
			for _, ed := range e.G.EdgesOf(h.ID) {
				switch {
				case (ed.Type == graph.EdgeHasColumn || ed.Type == graph.EdgeHasProperty) && ed.From == h.ID:
					cols++
				case ed.Type == graph.EdgeReferences && ed.From == h.ID:
					refs = append(refs, strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(ed.To, "table:"), "schema:"), "public."))
				}
			}
			unit := " cols"
			if h.Type == graph.NodeSchema {
				unit = " props"
			}
			fmt.Fprintf(&b, " (%d%s", cols, unit)
			if len(refs) > 0 {
				fmt.Fprintf(&b, "; → %s", strings.Join(refs, ", "))
			}
			b.WriteString(")")
		case graph.NodeColumn, graph.NodeProperty:
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
		case graph.NodeEndpoint:
			if n := e.G.Nodes[h.ID]; n != nil {
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
	if fr := e.G.Freshness(time.Now()); fr != "" {
		fmt.Fprintf(&b, "# %s\n", fr)
	}
	fmt.Fprintf(&b, "source: %s\n", ov.Source)
	if base := e.apiBaseURL(); base != "" {
		fmt.Fprintf(&b, "base url: %s\n", base)
	}
	var kinds []string
	for k := range ov.NodeCounts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Fprintf(&b, "%s: %d  ", k, ov.NodeCounts[k])
	}
	label, unit := "tables", " cols"
	if e.G.IsAPI() {
		label, unit = "schemas", " props"
	}
	fmt.Fprintf(&b, "\n%s:\n", label)
	for _, t := range ov.Tables {
		line := fmt.Sprintf("%s (%d%s", strings.TrimPrefix(t.Name, "public."), t.Columns, unit)
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
	if len(ov.Endpoints) > 0 {
		b.WriteString("\nendpoints:\n")
		for _, ep := range ov.Endpoints {
			line := ep.Name
			if len(ep.Schemas) > 0 {
				line += " → " + strings.Join(ep.Schemas, ", ")
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Codebase renderers
// ---------------------------------------------------------------------------

// IsCodebase reports whether this graph was extracted from source code
// (as opposed to a database or API spec).
func (e *Engine) IsCodebase() bool {
	return strings.HasPrefix(e.G.Source, "codebase:")
}

// RenderFunction renders a function/method node compactly: signature, file
// location with line numbers, and its call graph (callers + callees).
func (e *Engine) RenderFunction(id string) (string, error) {
	d, err := e.Describe(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	sig := d.Attrs["signature"]
	if sig == "" {
		sig = d.Name
	}
	fmt.Fprintf(&b, "## func %s\n", sig)
	if f := d.Attrs["file"]; f != "" {
		loc := f
		if ls := d.Attrs["line_start"]; ls != "" {
			loc += ":" + ls
			if le := d.Attrs["line_end"]; le != "" && le != ls {
				loc += "-" + le
			}
		}
		fmt.Fprintf(&b, "location: %s\n", loc)
	}
	if c := d.Attrs["doc"]; c != "" {
		fmt.Fprintf(&b, "doc: %s\n", c)
	}

	var calls, callers []string
	for _, ed := range d.Edges {
		if on := e.G.Nodes[ed.Other]; on != nil {
			label := on.Name
			if loc := on.Attrs["file"]; loc != "" {
				if ls := on.Attrs["line_start"]; ls != "" {
					label += " (" + loc + ":" + ls + ")"
				} else {
					label += " (" + loc + ")"
				}
			}
			switch {
			case ed.Type == "CALLS" && ed.Dir == "out" && on.Attrs["external"] != "true":
				calls = append(calls, label)
			case ed.Type == "CALLS" && ed.Dir == "in":
				callers = append(callers, label)
			}
		}
	}
	if len(callers) > 0 {
		sort.Strings(callers)
		b.WriteString("called by: " + joinCapped(callers, 12) + "\n")
	}
	if len(calls) > 0 {
		sort.Strings(calls)
		b.WriteString("calls: " + joinCapped(calls, 12) + "\n")
	}
	if body := d.Attrs["body"]; body != "" {
		fmt.Fprintf(&b, "```\n%s\n```\n", body)
	}
	return b.String(), nil
}

// Impact walks CALLS edges backwards from a function — every caller,
// transitively, up to depth levels — answering "what breaks if I change this
// signature" before a refactor. Accepts a node id or a bare function name
// (must be unique). Test-file callers are tagged [test].
func (e *Engine) Impact(id string, depth int) (string, error) {
	n := e.G.Nodes[id]
	if n == nil {
		var matches []string
		for nid, nn := range e.G.Nodes {
			if nn.Type == graph.NodeFunction && nn.Name == id && nn.Attrs["external"] != "true" {
				matches = append(matches, nid)
			}
		}
		switch len(matches) {
		case 0:
			return "", fmt.Errorf("function %q not found; use find_entities to locate it", id)
		case 1:
			id = matches[0]
			n = e.G.Nodes[id]
		default:
			sort.Strings(matches)
			return "", fmt.Errorf("ambiguous name %q — pick one id:\n  %s", id, strings.Join(matches, "\n  "))
		}
	}
	if depth <= 0 {
		depth = 3
	}

	funcLoc := func(nn *graph.Node) string {
		loc := nn.Attrs["file"]
		if ls := nn.Attrs["line_start"]; ls != "" {
			loc += ":" + ls
		}
		return loc
	}

	var b strings.Builder
	fmt.Fprintf(&b, "impact of changing %s (%s)\n", n.Name, funcLoc(n))
	visited := map[string]bool{id: true}
	level := []string{id}
	total := 0
	for d := 1; d <= depth && len(level) > 0; d++ {
		var next []string
		for _, cur := range level {
			for _, ed := range e.G.EdgesOf(cur) {
				if ed.Type == graph.EdgeCalls && ed.To == cur && !visited[ed.From] {
					visited[ed.From] = true
					next = append(next, ed.From)
				}
			}
		}
		if len(next) == 0 {
			break
		}
		label := "direct callers"
		if d > 1 {
			label = fmt.Sprintf("depth %d", d)
		}
		fmt.Fprintf(&b, "\n%s (%d):\n", label, len(next))
		var lines []string
		for _, cid := range next {
			cn := e.G.Nodes[cid]
			if cn == nil {
				continue
			}
			tag := ""
			if strings.Contains(cn.Attrs["file"], "_test.") || strings.Contains(cn.Attrs["file"], ".test.") {
				tag = " [test]"
			}
			lines = append(lines, fmt.Sprintf("  %s (%s)%s", cn.Name, funcLoc(cn), tag))
		}
		sort.Strings(lines)
		shown := lines
		if len(shown) > 25 {
			shown = shown[:25]
		}
		b.WriteString(strings.Join(shown, "\n") + "\n")
		if extra := len(lines) - len(shown); extra > 0 {
			fmt.Fprintf(&b, "  … +%d more\n", extra)
		}
		total += len(next)
		level = next
	}
	if total == 0 {
		b.WriteString("no callers found — signature change is local\n")
	} else {
		fmt.Fprintf(&b, "\ntotal affected: %d function(s) within depth %d\n", total, depth)
	}
	return b.String(), nil
}

// joinCapped joins up to max items, summarizing the overflow. Keeps renders
// of hub nodes (a function with 3000 callers) bounded.
func joinCapped(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(" … +%d more", len(items)-max)
}

// RenderDefinition renders a type/struct/class with its methods.
func (e *Engine) RenderDefinition(id string) (string, error) {
	d, err := e.Describe(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## type %s\n", d.Name)
	if f := d.Attrs["file"]; f != "" {
		loc := f
		if ls := d.Attrs["line_start"]; ls != "" {
			loc += ":" + ls
		}
		fmt.Fprintf(&b, "location: %s\n", loc)
	}
	if c := d.Attrs["doc"]; c != "" {
		fmt.Fprintf(&b, "doc: %s\n", c)
	}

	var methods []string
	for _, ed := range d.Edges {
		if ed.Type == "HAS_METHOD" && ed.Dir == "out" {
			if fn := e.G.Nodes[ed.Other]; fn != nil {
				entry := fn.Name
				if sig := fn.Attrs["signature"]; sig != "" {
					entry = sig
				}
				if ls := fn.Attrs["line_start"]; ls != "" {
					entry += " :" + ls
				}
				methods = append(methods, entry)
			}
		}
	}
	if len(methods) > 0 {
		sort.Strings(methods)
		b.WriteString("methods:\n")
		shown := methods
		if len(shown) > 30 {
			shown = shown[:30]
		}
		for _, m := range shown {
			fmt.Fprintf(&b, "  %s\n", m)
		}
		if extra := len(methods) - len(shown); extra > 0 {
			fmt.Fprintf(&b, "  … +%d more methods\n", extra)
		}
	}
	return b.String(), nil
}

// CodeContextPack is the codebase analogue of sql_context: it finds the most
// relevant functions, types, and files for a natural-language question, renders
// them compactly, and gives a complete orientation in one tool call instead of
// many round-trips through the graph.
func (e *Engine) CodeContextPack(ctx context.Context, question string, limit int) (string, error) {
	if limit <= 0 {
		limit = 6
	}
	var b strings.Builder
	if fr := e.G.Freshness(time.Now()); fr != "" {
		fmt.Fprintf(&b, "# source: %s\n\n", fr)
	}

	funcHits, err := e.Find(ctx, question, graph.NodeFunction, limit)
	if err != nil {
		return "", err
	}
	defHits, err := e.Find(ctx, question, graph.NodeDefinition, limit/2+1)
	if err != nil {
		return "", err
	}
	fileHits, err := e.Find(ctx, question, graph.NodeFile, limit/2)
	if err != nil {
		return "", err
	}

	seen := map[string]bool{}
	rendered := 0
	const budget = 16 * 1024 // hard cap: keep the pack one cheap tool call

	// Generated code (ANTLR parsers, protobufs…) stays findable via
	// find_entities but never earns context-pack space.
	skipGenerated := func(id string) bool {
		n := e.G.Nodes[id]
		return n != nil && n.Attrs["generated"] == "true"
	}

	// 1. Type definitions (orientation layer).
	for _, h := range defHits.Hits {
		if seen[h.ID] || b.Len() > budget || skipGenerated(h.ID) {
			continue
		}
		seen[h.ID] = true
		s, err := e.RenderDefinition(h.ID)
		if err != nil {
			continue
		}
		b.WriteString(s + "\n")
		rendered++
	}

	// 2. Relevant functions.
	for _, h := range funcHits.Hits {
		if seen[h.ID] || b.Len() > budget || skipGenerated(h.ID) {
			continue
		}
		if n := e.G.Nodes[h.ID]; n != nil && n.Attrs["external"] == "true" {
			continue
		}
		seen[h.ID] = true
		s, err := e.RenderFunction(h.ID)
		if err != nil {
			continue
		}
		b.WriteString(s + "\n")
		rendered++
	}

	// 3. Files (docs / README / plain source files).
	for _, h := range fileHits.Hits {
		if seen[h.ID] || rendered >= limit*2 || b.Len() > budget {
			break
		}
		if skipGenerated(h.ID) {
			continue
		}
		n := e.G.Nodes[h.ID]
		if n == nil {
			continue
		}
		seen[h.ID] = true
		if body := n.Attrs["doc_body"]; body != "" {
			if len(body) > 200 {
				body = body[:200] + "…"
			}
			fmt.Fprintf(&b, "## doc: %s\n%s\n\n", n.Name, body)
		} else {
			var fileFuncs []string
			for _, ed := range e.G.EdgesOf(h.ID) {
				if ed.Type == "BELONGS_TO" && ed.To == h.ID {
					if fn := e.G.Nodes[ed.From]; fn != nil && fn.Type == graph.NodeFunction {
						sig := fn.Attrs["signature"]
						if sig == "" {
							sig = fn.Name
						}
						fileFuncs = append(fileFuncs, sig)
					}
				}
			}
			sort.Strings(fileFuncs)
			if len(fileFuncs) > 0 {
				fmt.Fprintf(&b, "## file: %s\nfunctions: %s\n\n", n.Name, strings.Join(fileFuncs, ", "))
			}
		}
		rendered++
	}

	if rendered == 0 {
		return "no code entities matched the question; try find_entities with different wording", nil
	}
	return b.String(), nil
}


