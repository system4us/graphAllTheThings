package engine

// Compact text renderers. MCP tools return this instead of structured JSON:
// same information, 5-10x fewer tokens. Format is line-oriented so agents
// can quote fragments directly into SQL.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

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
		fmt.Fprintf(&b, "# source: %s\n", fr)
		if e.VectorsStale {
			b.WriteString("# warning: semantic vectors are stale (graph updated since last index; run `gatt index` or keep `gatt serve` running)\n")
		}
		b.WriteString("\n")
	} else if e.VectorsStale {
		b.WriteString("# warning: semantic vectors are stale (graph updated since last index; run `gatt index` or keep `gatt serve` running)\n\n")
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

// coverageSampleCap bounds how many raw hits coverageNote pulls back to
// count distinct files from. Below this, every occurrence in the repo was
// actually seen, so the file count is exact; at or above it, some
// occurrences were never fetched and the count degrades to "at least this
// many, uncorrected" — expected for a very common bare name, where the
// coverage note is the least useful anyway (mostly unrelated identifiers).
const coverageSampleCap = 400

// coverageNote is Impact/Blast's completeness signal: it compares
// directWired (the direct call/handler/middleware edges already found for a
// function — one per *source*, already deduped) against how many distinct
// *files* mention that function's bare name as raw text anywhere in the
// codebase. A gap between the two means some file that mentions the name
// has no wired edge to it at all — computed member access,
// .bind()/.call()/.apply() indirection, a dispatch idiom the extractor
// doesn't special-case — and "no callers found" or "N callers" would
// otherwise read as complete when it might not be. This is exactly the
// kind of miss that let two real bugs (a middleware reference, a
// namespace-export re-export) sit unnoticed until an actual grep
// cross-check caught them — the check that used to live in the person
// running gatt's head now runs on every query instead.
//
// Counting distinct files rather than raw line matches matters: a route
// file that calls requirePermission(...) nine times with nine different
// permission strings gets exactly one CALLS edge (same source, same
// target — wiring nine identical edges would just be noise), so comparing
// wired-edge count against raw *occurrence* count is a false mismatch
// baked into any caller that invokes the same target more than once, which
// permission/validation/logging helpers do constantly. Per-file dedup
// fixes that: nine call sites in one file is still one file.
//
// Over-flagging is fine (a same-named identifier, a comment, a string that
// happens to contain the word): the cost of a false alarm is one glance;
// the cost of silence was two shipped false negatives.
//
// A hit is accounted for — and never counted — when it sits inside a wired
// direct caller's line range, inside the target's own declaration, on an
// import line, on a comment-only line, or in a prose doc. Matching is on
// whole words, so InsertEventParams is not a mention of InsertEvent. The
// sample shows call-shaped lines (`Name(`) first and production files before
// tests: the one line most likely to be the missed caller comes first.
func (e *Engine) coverageNote(target *graph.Node, directCallers []*graph.Node) string {
	name := target.Name
	if name == "" {
		return ""
	}
	opts := GrepOptions{Limit: coverageSampleCap}
	pattern := name
	if isWordName(name) {
		pattern, opts.Regex = `\b`+regexp.QuoteMeta(name)+`\b`, true
	}
	hits, total, _, err := e.grepScan(pattern, opts)
	if err != nil || total == 0 {
		return ""
	}
	type span struct{ from, to int }
	accounted := map[string][]span{}
	wholeFile := map[string]bool{}
	addSpan := func(n *graph.Node) {
		if n == nil {
			return
		}
		if n.Type == graph.NodeFile {
			wholeFile[n.Name] = true
			return
		}
		ls, _ := strconv.Atoi(n.Attrs["line_start"])
		le, _ := strconv.Atoi(n.Attrs["line_end"])
		if le < ls {
			le = ls
		}
		if f := n.Attrs["file"]; f != "" && ls > 0 {
			accounted[f] = append(accounted[f], span{ls, le})
		}
	}
	addSpan(target)
	for _, c := range directCallers {
		addSpan(c)
	}
	isAccounted := func(h GrepHit) bool {
		if wholeFile[h.Path] || isProseDoc(h.Path) {
			return true
		}
		t := strings.TrimSpace(h.Text)
		if strings.HasPrefix(t, "import ") || strings.HasPrefix(t, "import(") || strings.HasPrefix(t, "from ") || isCommentLine(t) {
			return true
		}
		for _, sp := range accounted[h.Path] {
			if h.Line >= sp.from && h.Line <= sp.to {
				return true
			}
		}
		return false
	}
	callShape := regexp.MustCompile(`(^|[^\w$])` + regexp.QuoteMeta(name) + `\s*\(`)
	rank := func(h GrepHit) int {
		r := 0
		if !callShape.MatchString(h.Text) {
			r += 2
		}
		if IsTestFilePath(h.Path) {
			r++
		}
		return r
	}
	best := map[string]GrepHit{}
	for _, h := range hits {
		if isAccounted(h) {
			continue
		}
		if cur, ok := best[h.Path]; !ok || rank(h) < rank(cur) {
			best[h.Path] = h
		}
	}
	if len(best) == 0 {
		return ""
	}
	sample := make([]GrepHit, 0, len(best))
	for _, h := range best {
		sample = append(sample, h)
	}
	sort.Slice(sample, func(i, j int) bool {
		ri, rj := rank(sample[i]), rank(sample[j])
		if ri != rj {
			return ri < rj
		}
		if sample[i].Path != sample[j].Path {
			return sample[i].Path < sample[j].Path
		}
		return sample[i].Line < sample[j].Line
	})
	extra := len(sample)
	if len(sample) > 5 {
		sample = sample[:5]
	}
	truncNote := ""
	if total >= coverageSampleCap {
		truncNote = ", sample truncated — could be more"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n⚠ coverage: %q appears in %d file(s) with no wired edge above (whole-word matches; the definition, wired callers, imports, comments and docs excluded%s)",
		name, extra, truncNote)
	b.WriteString(" — could be an indirect call this analysis can't see (a value whose type it can't follow, computed member access, " +
		".bind()/.call()/.apply(), an unhandled dispatch pattern), or just a same-named identifier or string. " +
		"Call-shaped lines first; worth a quick look before treating this as complete:\n")
	for _, h := range sample {
		fmt.Fprintf(&b, "    %s:%d: %s\n", h.Path, h.Line, strings.TrimSpace(h.Text))
	}
	return b.String()
}

// isWordName reports whether name is made only of identifier characters, so
// a \b-delimited regex finds it as a whole word.
func isWordName(name string) bool {
	for _, r := range name {
		if !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return false
		}
	}
	return name != ""
}

// isCommentLine reports a line that is only a comment in the common syntaxes.
func isCommentLine(t string) bool {
	for _, p := range []string{"//", "#", "/*", "* ", "*/", "--", "<!--", `"""`, "'''"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return t == "*"
}

// isProseDoc reports documentation files whose mentions are never calls.
func isProseDoc(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".md", ".mdx", ".txt", ".rst", ".adoc":
		return true
	}
	return false
}

// modelMatches returns the ids of every NodeModel node with the given name.
// Shared by Impact (to tell "not a function, it's a model" apart from "not
// found at all") and Blast (as its model-by-name fallback).
// callerFileKey returns the file a caller node lives in, for grouping
// direct-caller counts by file: a function/route node's own "file" attr
// for everything except a file node itself, which (since it *is* the file)
// carries the path under "path" instead — using n.Attrs["file"] on a file
// node returns "", collapsing every file-sourced caller into one bucket.
func callerFileKey(n *graph.Node) string {
	if n.Type == graph.NodeFile {
		return n.Attrs["path"]
	}
	return n.Attrs["file"]
}

// externalStub returns "call:<name>"'s node if it exists — a target that
// was only ever seen as a *reference* (called, or named as a route
// handler), never as a definition. Its most common cause: a handler
// declared in a shape the extractor doesn't parse as a function at all
// (an unusual export/assignment pattern) — same root cause as the
// CommonJS `exports.x = fn` gap, before that got its own capture. Lets
// Impact/Blast tell that apart from "genuinely doesn't exist anywhere,"
// which get a different, more useful error.
func (e *Engine) externalStub(name string) *graph.Node {
	if n := e.G.Nodes["call:"+name]; n != nil && n.Type == graph.NodeFunction && n.Attrs["external"] == "true" {
		return n
	}
	return nil
}

func (e *Engine) modelMatches(name string) []string {
	var matches []string
	for id, nn := range e.G.Nodes {
		if nn.Type == graph.NodeModel && nn.Name == name {
			matches = append(matches, id)
		}
	}
	return matches
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
			if len(e.modelMatches(id)) > 0 {
				return "", fmt.Errorf("%q is a model, not a function — impact only walks the call graph; use `gatt blast %s` for its associations and route usage", id, id)
			}
			if e.externalStub(id) != nil {
				return "", fmt.Errorf("%q was only ever seen as a reference (called, or named as a route handler), never as a definition — "+
					"this usually means it's declared in a way the extractor doesn't recognize as a function (an unusual export/assignment pattern). "+
					"Try `gatt grep %q` to find where it's actually defined", id, id)
			}
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
	if note := n.Attrs["entity_note"]; note != "" {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	if inv := n.Attrs["invariant"]; inv != "" {
		fmt.Fprintf(&b, "⚠ invariant: %s\n", inv)
	}
	if g := n.Attrs["gotcha"]; g != "" {
		fmt.Fprintf(&b, "⚠ gotcha: %s\n", g)
	}
	visited := map[string]bool{id: true}
	level := []string{id}
	total := 0
	var directCallerNodes []*graph.Node
	for d := 1; d <= depth && len(level) > 0; d++ {
		var next []string
		for _, cur := range level {
			for _, ed := range e.G.EdgesOf(cur) {
				isCaller := ed.Type == graph.EdgeCalls || ed.Type == graph.EdgeHandledBy || ed.Type == graph.EdgeUsesMiddleware
				if isCaller && ed.To == cur && !visited[ed.From] {
					visited[ed.From] = true
					next = append(next, ed.From)
				}
			}
		}
		if len(next) == 0 {
			break
		}
		if d == 1 {
			// Distinct files, not edge count: coverageNote compares this
			// against a distinct-file grep count, and a single file can
			// legitimately source many direct edges (several routes in one
			// routes.ts all using the same middleware) without that being
			// several files' worth of "coverage" — counting edges here
			// would make well-covered targets look under-covered by the
			// file-based comparison and mask real gaps in the noise.
			for _, cid := range next {
				if cn := e.G.Nodes[cid]; cn != nil {
					directCallerNodes = append(directCallerNodes, cn)
				}
			}
		}
		label := "direct callers"
		if d > 1 {
			label = fmt.Sprintf("depth %d", d)
		}
		fmt.Fprintf(&b, "\n%s (%d):\n", label, len(next))
		type callerLine struct {
			label   string
			snippet string
		}
		var items []callerLine
		for _, cid := range next {
			cn := e.G.Nodes[cid]
			if cn == nil {
				continue
			}
			tag := ""
			if strings.Contains(cn.Attrs["file"], "_test.") || strings.Contains(cn.Attrs["file"], ".test.") {
				tag = " [test]"
			}
			item := callerLine{label: fmt.Sprintf("  %s (%s)%s", cn.Name, funcLoc(cn), tag)}
			// Content only for direct callers — these are what the agent
			// actually edits; deeper levels stay location-only so the pack
			// doesn't balloon at higher --depth values.
			if d == 1 {
				item.snippet = e.snippetFor(cn)
			}
			items = append(items, item)
		}
		sort.Slice(items, func(i, j int) bool { return items[i].label < items[j].label })
		shown := items
		if len(shown) > 25 {
			shown = shown[:25]
		}
		for _, it := range shown {
			b.WriteString(it.label + "\n")
			if it.snippet != "" {
				b.WriteString(indentSnippet(it.snippet, "      "))
			}
		}
		if extra := len(items) - len(shown); extra > 0 {
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
	b.WriteString(e.coverageNote(n, directCallerNodes))
	b.WriteString(e.sharedStateText(id))
	return b.String(), nil
}

// sharedStateText renders the cross-file "you'll also have to look at this"
// signal CALLS can't see: for each shared-state node (a tracked property on
// a singleton — see internal/connector/codebase/stateflow.go, JS/TS/JSX
// only, v1) the given function id reads or writes, list the *other*
// functions that also read or write it. One hop only, not transitive.
// Returns "" when the function touches no tracked shared state.
func (e *Engine) sharedStateText(id string) string {
	var stateIDs []string
	seen := map[string]bool{}
	for _, ed := range e.G.EdgesOf(id) {
		if ed.From != id || (ed.Type != graph.EdgeWritesState && ed.Type != graph.EdgeReadsState) {
			continue
		}
		if !seen[ed.To] {
			seen[ed.To] = true
			stateIDs = append(stateIDs, ed.To)
		}
	}
	if len(stateIDs) == 0 {
		return ""
	}
	sort.Strings(stateIDs)

	var b strings.Builder
	b.WriteString("\nshares mutable state:\n")
	any := false
	for _, sid := range stateIDs {
		sn := e.G.Nodes[sid]
		if sn == nil {
			continue
		}
		var writers, readers []string
		for _, ed := range e.G.EdgesOf(sid) {
			if ed.To != sid || ed.From == id {
				continue
			}
			on := e.G.Nodes[ed.From]
			if on == nil {
				continue
			}
			label := on.Name
			if f := on.Attrs["file"]; f != "" && on.Attrs["line_start"] != "" {
				label += fmt.Sprintf(" (%s:%s)", f, on.Attrs["line_start"])
			}
			if ed.Type == graph.EdgeWritesState {
				writers = append(writers, label)
			} else {
				readers = append(readers, label)
			}
		}
		if len(writers) == 0 && len(readers) == 0 {
			continue
		}
		any = true
		fmt.Fprintf(&b, "  %s:", sn.Name)
		if len(writers) > 0 {
			sort.Strings(writers)
			fmt.Fprintf(&b, " written by %s;", strings.Join(writers, ", "))
		}
		if len(readers) > 0 {
			sort.Strings(readers)
			fmt.Fprintf(&b, " read by %s", strings.Join(readers, ", "))
		}
		b.WriteString("\n")
	}
	if !any {
		return ""
	}
	return b.String()
}

// blastCommonBasenames are file names that repeat across every project by
// convention; same-basename copy detection skips them as pure noise.
var blastCommonBasenames = map[string]bool{
	"package.json": true, "package-lock.json": true, "tsconfig.json": true,
	"index.ts": true, "index.tsx": true, "index.js": true,
	"__init__.py": true, "mod.rs": true, "README.md": true, "Cargo.toml": true,
}

// Blast computes the blast radius of modifying any node — a file (code or
// data), function, or definition. Where Impact walks only reverse CALLS edges
// from a function, Blast seeds a file with its member functions/types and also
// walks reverse IMPORTS/REFERENCES and forward GENERATES edges, warns when the
// target is itself generated, and flags same-basename copies (identical vs
// diverged by content hash). Accepts a node id, a repo-relative path, a bare
// file name, or a unique function name.
func (e *Engine) Blast(target string, depth int) (string, error) {
	n := e.G.Nodes[target]
	if n == nil {
		n = e.G.Nodes["file:"+target]
	}
	if n == nil {
		var matches []string
		for id, nn := range e.G.Nodes {
			if nn.Type == graph.NodeFile &&
				(nn.Name == target || strings.HasSuffix(nn.Name, "/"+target) || filepath.Base(nn.Name) == target) {
				matches = append(matches, id)
			}
		}
		if len(matches) == 0 {
			for id, nn := range e.G.Nodes {
				if nn.Type == graph.NodeFunction && nn.Name == target && nn.Attrs["external"] != "true" {
					matches = append(matches, id)
				}
			}
		}
		if len(matches) == 0 {
			matches = e.modelMatches(target)
		}
		switch len(matches) {
		case 0:
			if e.externalStub(target) != nil {
				return "", fmt.Errorf("%q was only ever seen as a reference (called, or named as a route handler), never as a definition — "+
					"this usually means it's declared in a way the extractor doesn't recognize as a function (an unusual export/assignment pattern). "+
					"Try `gatt grep %q` to find where it's actually defined", target, target)
			}
			return "", fmt.Errorf("%q not found as file, function, or model; use search to locate it", target)
		case 1:
			n = e.G.Nodes[matches[0]]
		default:
			sort.Strings(matches)
			var lines []string
			for _, id := range matches {
				h := e.G.Nodes[id].Attrs["hash"]
				if h != "" {
					h = " hash:" + h
				}
				lines = append(lines, "  "+id+h)
			}
			return "", fmt.Errorf("ambiguous %q — %d matches (equal hash = identical content), pick one id:\n%s",
				target, len(matches), strings.Join(lines, "\n"))
		}
	}
	if depth <= 0 {
		depth = 3
	}
	if n.Type == graph.NodeModel && depth > 1 {
		// REFERENCES between models isn't transitively "breaks" the way CALLS/
		// IMPORTS is: Opportunity referencing Lead doesn't mean everything that
		// references Opportunity breaks when Lead changes. Cap at direct
		// references/routes only — depth >1 was schema-wide noise (hundreds of
		// unrelated models pulled in via chained FKs).
		depth = 1
	}

	loc := func(nn *graph.Node) string {
		l := nn.Attrs["file"]
		if l == "" {
			l = nn.Name
		}
		if ls := nn.Attrs["line_start"]; ls != "" {
			l += ":" + ls
		}
		return l
	}

	var b strings.Builder
	fmt.Fprintf(&b, "blast radius of modifying %s (%s)\n", n.Name, n.Type)
	if note := n.Attrs["entity_note"]; note != "" {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	if inv := n.Attrs["invariant"]; inv != "" {
		fmt.Fprintf(&b, "⚠ invariant: %s\n", inv)
	}
	if g := n.Attrs["gotcha"]; g != "" {
		fmt.Fprintf(&b, "⚠ gotcha: %s\n", g)
	}

	// Warn when the target is a generated artifact: edits get overwritten.
	if n.Attrs["generated"] == "true" {
		b.WriteString("⚠ marked as generated code — direct edits are likely overwritten\n")
	}
	for _, ed := range e.G.EdgesOf(n.ID) {
		if ed.Type == graph.EdgeGenerates && ed.To == n.ID {
			if src := e.G.Nodes[ed.From]; src != nil {
				fmt.Fprintf(&b, "⚠ generated from %s — edit the source, not this file\n", src.Name)
			}
		}
	}

	// Seed: the node itself, plus member functions/types when it is a file.
	visited := map[string]bool{n.ID: true}
	level := []string{n.ID}
	if n.Type == graph.NodeFile {
		for _, ed := range e.G.EdgesOf(n.ID) {
			if ed.Type == graph.EdgeBelongsTo && ed.To == n.ID && !visited[ed.From] {
				visited[ed.From] = true
				level = append(level, ed.From)
			}
		}
	}

	// Reverse BFS over CALLS/IMPORTS/REFERENCES, forward over GENERATES.
	type hit struct {
		node  *graph.Node
		depth int
	}
	byEdge := map[string][]hit{}
	total := 0
	for d := 1; d <= depth && len(level) > 0; d++ {
		var next []string
		for _, cur := range level {
			for _, ed := range e.G.EdgesOf(cur) {
				var nb string
				switch {
				case ed.To == cur && (ed.Type == graph.EdgeCalls || ed.Type == graph.EdgeImports || ed.Type == graph.EdgeReferences || ed.Type == graph.EdgeMentions || ed.Type == graph.EdgeCallsEndpoint || ed.Type == graph.EdgeUsesModel || ed.Type == graph.EdgeUsesStyle || ed.Type == graph.EdgeHandledBy || ed.Type == graph.EdgeUsesMiddleware):
					nb = ed.From
				case ed.From == cur && ed.Type == graph.EdgeGenerates:
					nb = ed.To
				}
				if nb == "" || visited[nb] {
					continue
				}
				visited[nb] = true
				nn := e.G.Nodes[nb]
				if nn == nil || nn.Attrs["external"] == "true" {
					continue
				}
				byEdge[ed.Type] = append(byEdge[ed.Type], hit{nn, d})
				next = append(next, nb)
				total++
			}
		}
		level = next
	}

	// Split USES_MODEL hits into HTTP routes vs sqlc/code queries so labels stay accurate.
	if um := byEdge[graph.EdgeUsesModel]; len(um) > 0 {
		var routeHits, queryHits []hit
		for _, h := range um {
			if h.node != nil && h.node.Type == graph.NodeRoute {
				routeHits = append(routeHits, h)
			} else {
				queryHits = append(queryHits, h)
			}
		}
		byEdge[graph.EdgeUsesModel] = routeHits
		if len(queryHits) > 0 {
			byEdge["USES_MODEL_QUERY"] = queryHits
		}
	}

	sections := []struct{ edge, label string }{
		{graph.EdgeImports, "importers"},
		{graph.EdgeCalls, "callers"},
		{graph.EdgeGenerates, "regenerated outputs"},
		{graph.EdgeReferences, "references"},
		{graph.EdgeUsesModel, "routes touching this model — API surface affected"},
		{"USES_MODEL_QUERY", "queries/functions touching this model"},
		{graph.EdgeCallsEndpoint, "frontend call sites hitting this route"},
		{graph.EdgeUsesStyle, "UI files using this stylesheet's selectors"},
		{graph.EdgeMentions, "documented in — update these docs too"},
		{graph.EdgeHandledBy, "routes handled by this function"},
		{graph.EdgeUsesMiddleware, "routes using this as middleware"},
	}
	for _, s := range sections {
		hits := byEdge[s.edge]
		if len(hits) == 0 {
			continue
		}
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].depth != hits[j].depth {
				return hits[i].depth < hits[j].depth
			}
			return hits[i].node.Name < hits[j].node.Name
		})
		fmt.Fprintf(&b, "\n%s (%d):\n", s.label, len(hits))
		shown := hits
		if len(shown) > 25 {
			shown = shown[:25]
		}
		for _, h := range shown {
			tag := ""
			f := h.node.Attrs["file"]
			if f == "" {
				f = h.node.Name
			}
			if strings.Contains(f, "_test.") || strings.Contains(f, ".test.") {
				tag = " [test]"
			}
			where := loc(h.node)
			if where == h.node.Name {
				// File nodes: the location is the name; don't print it twice.
				fmt.Fprintf(&b, "  %s [depth %d]%s\n", h.node.Name, h.depth, tag)
			} else {
				fmt.Fprintf(&b, "  %s (%s) [depth %d]%s\n", h.node.Name, where, h.depth, tag)
			}
			// Content only for depth-1 function hits — direct callers/
			// importers are what the agent edits; file-level hits (whole
			// modules) and deeper levels stay location-only.
			if h.depth == 1 && h.node.Type == graph.NodeFunction {
				if snip := e.snippetFor(h.node); snip != "" {
					b.WriteString(indentSnippet(snip, "      "))
				}
			}
		}
		if extra := len(hits) - len(shown); extra > 0 {
			fmt.Fprintf(&b, "  … +%d more\n", extra)
		}
	}

	// Co-change companions from git history: files with no static edge to the
	// target that nonetheless ship together — the "you'll also have to touch
	// these" list (stylesheets, docs, e2e tests, i18n).
	if n.Type == graph.NodeFile {
		type comp struct {
			name string
			cnt  int
			conf string
		}
		var comps []comp
		for _, ed := range e.G.EdgesOf(n.ID) {
			if ed.Type != graph.EdgeCoChanged {
				continue
			}
			other := ed.To
			if other == n.ID {
				other = ed.From
			}
			if on := e.G.Nodes[other]; on != nil {
				cnt := 0
				fmt.Sscanf(ed.Attrs["count"], "%d", &cnt)
				comps = append(comps, comp{on.Name, cnt, ed.Attrs["confidence"]})
			}
		}
		if len(comps) > 0 {
			sort.Slice(comps, func(i, j int) bool {
				if comps[i].cnt != comps[j].cnt {
					return comps[i].cnt > comps[j].cnt
				}
				return comps[i].name < comps[j].name
			})
			if len(comps) > 10 {
				comps = comps[:10]
			}
			fmt.Fprintf(&b, "\nco-change companions (git history — usually ship together):\n")
			for _, cp := range comps {
				fmt.Fprintf(&b, "  %s (%d shared commits, confidence %s)\n", cp.name, cp.cnt, cp.conf)
			}
		}
	}

	// Same-basename copies: schema/config files duplicated across a monorepo.
	if n.Type == graph.NodeFile {
		base := filepath.Base(n.Name)
		if !blastCommonBasenames[base] {
			var copies []string
			for id, nn := range e.G.Nodes {
				if id == n.ID || nn.Type != graph.NodeFile || filepath.Base(nn.Name) != base {
					continue
				}
				state := ""
				if h, h2 := n.Attrs["hash"], nn.Attrs["hash"]; h != "" && h2 != "" {
					if h == h2 {
						state = " [identical]"
					} else {
						state = " [diverged]"
					}
				}
				copies = append(copies, "  "+nn.Name+state)
			}
			if len(copies) > 0 {
				sort.Strings(copies)
				fmt.Fprintf(&b, "\nsame-basename copies (%d) — check which one is authoritative:\n%s\n",
					len(copies), strings.Join(copies, "\n"))
			}
		}
	}

	if total == 0 {
		b.WriteString("no dependents found — change is local\n")
	} else {
		fmt.Fprintf(&b, "\ntotal affected: %d node(s) within depth %d\n", total, depth)
	}
	if n.Type == graph.NodeFunction {
		// Distinct files, not edge count — see Impact's directFiles for why:
		// one file can legitimately source many direct edges (several
		// routes in one routes.ts using the same middleware) without that
		// being several files' worth of coverage.
		var directCallers []*graph.Node
		for _, edgeType := range [...]string{graph.EdgeCalls, graph.EdgeHandledBy, graph.EdgeUsesMiddleware} {
			for _, h := range byEdge[edgeType] {
				if h.depth == 1 {
					directCallers = append(directCallers, h.node)
				}
			}
		}
		b.WriteString(e.coverageNote(n, directCallers))
		b.WriteString(e.sharedStateText(n.ID))
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

// RenderDefinition renders a type/struct/class/const with its methods.
func (e *Engine) RenderDefinition(id string) (string, error) {
	d, err := e.Describe(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	kind := d.Attrs["kind"]
	if kind == "" {
		kind = "type"
	}
	fmt.Fprintf(&b, "## %s %s\n", kind, d.Name)
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
	return e.codeContextPack(ctx, question, limit, 0)
}

// packSection is one rendered entry of a code context pack. kind orders the
// output (types, functions, comments, docs/files); prio orders what a token
// budget keeps — the best functions and types first, test code and prose last.
type packSection struct {
	kind int
	prio int
	seq  int
	text string
}

// codeContextPack builds the pack. Ranking rules on top of Find's order:
//   - production code before tests: a test function or a type declared in a
//     test file fills a slot only after every non-test hit (unless the
//     question is about tests), so "how does X work" answers with X;
//   - the best floating comments get their own short section: a file-header
//     comment ("Per-event hash chain…") often names the right file when no
//     single function does;
//   - with maxTokens > 0 the budget keeps whole sections by priority (top
//     functions and types, then the rest, then comments and docs) and still
//     prints them in the usual order — instead of cutting the tail, which
//     dropped the functions because types come first.
func (e *Engine) codeContextPack(ctx context.Context, question string, limit, maxTokens int) (string, error) {
	if limit <= 0 {
		limit = 6
	}
	var head strings.Builder
	if fr := e.G.Freshness(time.Now()); fr != "" {
		fmt.Fprintf(&head, "# source: %s\n", fr)
		if e.VectorsStale {
			head.WriteString("# warning: semantic vectors are stale (graph updated since last index; run `gatt index` or keep `gatt serve` running)\n")
		}
		head.WriteString("\n")
	} else if e.VectorsStale {
		head.WriteString("# warning: semantic vectors are stale (graph updated since last index; run `gatt index` or keep `gatt serve` running)\n\n")
	}

	aboutTests := false
	for _, t := range graph.QueryTerms(question) {
		if strings.HasPrefix(t, "test") || t == "spec" || t == "prueba" || t == "pruebas" {
			aboutTests = true
		}
	}
	// Over-fetch so demoted test hits can be replaced by production ones.
	funcHits, err := e.Find(ctx, question, graph.NodeFunction, limit*2)
	if err != nil {
		return "", err
	}
	defHits, err := e.Find(ctx, question, graph.NodeDefinition, limit+2)
	if err != nil {
		return "", err
	}
	fileHits, err := e.Find(ctx, question, graph.NodeFile, limit/2)
	if err != nil {
		return "", err
	}
	commentHits, err := e.Find(ctx, question, graph.NodeComment, 4)
	if err != nil {
		return "", err
	}

	// Generated code (ANTLR parsers, protobufs…) stays findable via
	// find_entities but never earns context-pack space.
	usable := func(id string) (*graph.Node, bool) {
		n := e.G.Nodes[id]
		return n, n != nil && n.Attrs["generated"] != "true" && n.Attrs["external"] != "true"
	}
	isTest := func(n *graph.Node) bool { return IsTestFilePath(n.Attrs["file"]) }
	// pick keeps Find's order but moves test hits behind production ones.
	pick := func(hits []FindHit, max int) []*graph.Node {
		var prod, tests []*graph.Node
		for _, h := range hits {
			n, ok := usable(h.ID)
			if !ok {
				continue
			}
			if !aboutTests && isTest(n) {
				tests = append(tests, n)
			} else {
				prod = append(prod, n)
			}
		}
		out := append(prod, tests...)
		if len(out) > max {
			out = out[:max]
		}
		return out
	}

	const (
		kindDef = iota
		kindFunc
		kindComment
		kindFile
	)
	var sections []packSection
	seen := map[string]bool{}
	add := func(kind, prio int, id, text string) {
		if seen[id] || text == "" {
			return
		}
		seen[id] = true
		sections = append(sections, packSection{kind: kind, prio: prio, seq: len(sections), text: text})
	}

	for i, n := range pick(defHits.Hits, limit/2+1) {
		if s, err := e.RenderDefinition(n.ID); err == nil {
			prio := 1
			if i >= 2 {
				prio = 3
			}
			if isTest(n) {
				prio = 5
			}
			add(kindDef, prio, n.ID, s+"\n")
		}
	}
	for i, n := range pick(funcHits.Hits, limit) {
		if s, err := e.RenderFunction(n.ID); err == nil {
			prio := 0
			if i >= 3 {
				prio = 2
			}
			if isTest(n) && !aboutTests {
				prio = 5
			}
			add(kindFunc, prio, n.ID, s+"\n")
		}
	}
	shownComments := 0
	for _, h := range commentHits.Hits {
		n, ok := usable(h.ID)
		if !ok || shownComments >= 2 || (isTest(n) && !aboutTests) {
			continue
		}
		text := strings.Join(strings.Fields(n.Attrs["text"]), " ")
		if text == "" {
			continue
		}
		if len(text) > 240 {
			text = text[:240] + "…"
		}
		add(kindComment, 4, n.ID, fmt.Sprintf("## comment: %s:%s\n%s\n\n", n.Attrs["file"], n.Attrs["line"], text))
		shownComments++
	}
	for _, h := range fileHits.Hits {
		n, ok := usable(h.ID)
		if !ok {
			continue
		}
		if body := n.Attrs["doc_body"]; body != "" {
			if len(body) > 200 {
				body = body[:200] + "…"
			}
			add(kindFile, 6, h.ID, fmt.Sprintf("## doc: %s\n%s\n\n", n.Name, body))
			continue
		}
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
			add(kindFile, 6, h.ID, fmt.Sprintf("## file: %s\nfunctions: %s\n\n", n.Name, strings.Join(fileFuncs, ", ")))
		}
	}

	if len(sections) == 0 {
		return "no code entities matched the question; try find_entities with different wording or gatt grep for an exhaustive literal scan", nil
	}

	// Choose what to keep: everything up to the hard cap (one cheap tool
	// call), or, under a token budget, the highest-priority sections that fit.
	const hardCap = 16 * 1024
	capChars := hardCap
	if maxTokens > 0 && maxTokens*4 < capChars {
		capChars = maxTokens * 4
	}
	byPrio := make([]int, len(sections))
	for i := range byPrio {
		byPrio[i] = i
	}
	sort.SliceStable(byPrio, func(a, b int) bool { return sections[byPrio[a]].prio < sections[byPrio[b]].prio })
	keep := map[int]bool{}
	used, dropped := head.Len(), 0
	for _, i := range byPrio {
		if used+len(sections[i].text) > capChars && len(keep) > 0 {
			dropped++
			continue
		}
		keep[i] = true
		used += len(sections[i].text)
	}
	sort.SliceStable(sections, func(a, b int) bool {
		if sections[a].kind != sections[b].kind {
			return sections[a].kind < sections[b].kind
		}
		return sections[a].seq < sections[b].seq
	})
	var b strings.Builder
	b.WriteString(head.String())
	for i := range sections {
		// keep is indexed by the pre-sort position, which seq records.
		if keep[sections[i].seq] {
			b.WriteString(sections[i].text)
		}
	}
	if dropped > 0 && maxTokens > 0 {
		fmt.Fprintf(&b, "# note: %d lower-ranked section(s) left out to fit ~%d tokens; raise max_tokens for more\n", dropped, maxTokens)
	}
	b.WriteString("# note: top-N ranked context pack (not exhaustive); use `gatt grep` for exhaustive literal/regex scans\n")
	return b.String(), nil
}

var (
	docInlineCodeRe = regexp.MustCompile("`([^`\n]{1,80})`")
	docIdentLikeRe  = regexp.MustCompile(`^[A-Za-z_][\w./-]*$`)
)

// docSourceExts are extensions a doc-cited path must carry to count as a
// checkable source reference; docSkipDirs are first path segments that the
// extractor never indexes, so their absence from the graph proves nothing.
var docSourceExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".py": true, ".rs": true,
	".java": true, ".cs": true, ".md": true,
	".json": true, ".yaml": true, ".yml": true, ".sql": true, ".toml": true,
	".css": true, ".scss": true, ".less": true,
}
var docSkipDirs = map[string]bool{
	"node_modules": true, "gatt-out": true, "vendor": true, "dist": true, "build": true,
}

// mineDocPathTokens re-extracts file-path references from a doc's current
// text (fenced blocks skipped): identifier-shaped, containing a "/", ending
// in an indexed source extension, not under a never-indexed directory. Paths
// are the one token class where "resolves to no file node" reliably means
// "this doc points at a file that moved or is gone" even right after a full
// re-extract; bare symbol names are instead checked via mentions_resolved,
// which survives incremental refresh (the everyday flow).
func mineDocPathTokens(data []byte) []string {
	var tokens []string
	inFence := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence || len(tokens) >= 200 {
			continue
		}
		for _, m := range docInlineCodeRe.FindAllStringSubmatch(line, -1) {
			tok := strings.TrimSuffix(strings.TrimSpace(m[1]), "()")
			if len(tok) < 3 || !strings.Contains(tok, "/") || !docIdentLikeRe.MatchString(tok) {
				continue
			}
			if !docSourceExts[filepath.Ext(tok)] {
				continue
			}
			if first, _, ok := strings.Cut(tok, "/"); ok && docSkipDirs[first] {
				continue
			}
			tokens = append(tokens, tok)
		}
	}
	return tokens
}

// DocDrift reports markdown docs whose inline-code references either no
// longer resolve in the graph (broken) or resolve to code that was last
// touched, per git, after the doc itself was (stale). Broken combines two
// detectors: tokens that resolved at extract time and no longer do (precise;
// survives incremental refresh, which is how symbol renames surface), and
// doc-cited source paths that match no file node (path existence is
// re-checkable even after a full re-extract recomputes mentions_resolved).
// Staleness needs a git checkout; outside one, only broken refs are reported.
func (e *Engine) DocDrift(limit int) (string, error) {
	if !e.IsCodebase() {
		return "", fmt.Errorf("doc drift needs a codebase graph")
	}
	if limit <= 0 {
		limit = 15
	}
	dir := strings.TrimPrefix(e.G.Source, "codebase:")
	hasGit := false
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		hasGit = true
	}
	commitCache := map[string]int64{}
	lastCommit := func(relPath string) int64 {
		if !hasGit {
			return 0
		}
		if t, ok := commitCache[relPath]; ok {
			return t
		}
		out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%at", "--", relPath).Output()
		var t int64
		if err == nil {
			fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &t)
		}
		commitCache[relPath] = t
		return t
	}

	type drift struct {
		name   string
		broken []string
		stale  []string
	}
	var drifted []drift

	// Re-resolution indexes, mirroring the connector's wireMentions rules.
	// broken means "resolved at extract time, resolves to nothing now" — a
	// deleted/renamed symbol. Tokens that never resolved (prose, flags, header
	// names) are not code references and are never reported.
	fileByName := map[string]bool{}
	defIDs := map[string][]string{}
	funcNames := map[string]bool{}
	for nid, nn := range e.G.Nodes {
		switch {
		case nn.Type == graph.NodeFile && !strings.HasPrefix(nid, "doc:"):
			fileByName[nn.Name] = true
		case nn.Type == graph.NodeDefinition:
			defIDs[nn.Name] = append(defIDs[nn.Name], nid)
		case nn.Type == graph.NodeFunction && nn.Attrs["external"] != "true":
			funcNames[nn.Name] = true
		}
	}
	methodOf := map[string]bool{}
	for _, ed := range e.G.Edges {
		if ed.Type != graph.EdgeHasMethod {
			continue
		}
		if fn := e.G.Nodes[ed.To]; fn != nil {
			methodOf[ed.From+"\x00"+fn.Name] = true
		}
	}
	resolvesNow := func(tok string) bool {
		if strings.ContainsAny(tok, "/.") {
			if fileByName[tok] {
				return true
			}
			for name := range fileByName {
				if strings.HasSuffix(name, "/"+tok) {
					return true
				}
			}
			if !strings.Contains(tok, "/") {
				if left, right, ok := strings.Cut(tok, "."); ok && right != "" && !strings.Contains(right, ".") {
					if ids := defIDs[left]; len(ids) == 1 && methodOf[ids[0]+"\x00"+right] {
						return true
					}
				}
			}
		}
		return len(defIDs[tok]) > 0 || funcNames[tok]
	}

	for id, n := range e.G.Nodes {
		if !strings.HasPrefix(id, "doc:") {
			continue
		}
		relPath := n.Attrs["file"]
		if relPath == "" {
			continue
		}

		var d drift
		d.name = relPath
		seen := map[string]bool{}
		for _, tok := range strings.Fields(n.Attrs["mentions_resolved"]) {
			if !seen[tok] && !resolvesNow(tok) {
				seen[tok] = true
				d.broken = append(d.broken, tok)
			}
		}
		if data, err := os.ReadFile(filepath.Join(dir, relPath)); err == nil {
			for _, tok := range mineDocPathTokens(data) {
				if !seen[tok] && !resolvesNow(tok) {
					seen[tok] = true
					d.broken = append(d.broken, tok)
				}
			}
		}

		if hasGit {
			docTime := lastCommit(relPath)
			staleSeen := map[string]bool{}
			for _, ed := range e.G.EdgesOf(id) {
				if ed.Type != graph.EdgeMentions || ed.From != id {
					continue
				}
				tn := e.G.Nodes[ed.To]
				if tn == nil {
					continue
				}
				targetPath := tn.Attrs["file"]
				if targetPath == "" && tn.Type == graph.NodeFile {
					targetPath = tn.Name
				}
				if targetPath == "" || staleSeen[targetPath] {
					continue
				}
				staleSeen[targetPath] = true
				if t := lastCommit(targetPath); t > docTime && docTime > 0 {
					d.stale = append(d.stale, targetPath)
				}
			}
		}

		if len(d.broken) > 0 || len(d.stale) > 0 {
			sort.Strings(d.broken)
			sort.Strings(d.stale)
			drifted = append(drifted, d)
		}
	}

	if len(drifted) == 0 {
		if hasGit {
			return "no doc drift found — every inline reference resolves and no referenced code outdates its doc\n", nil
		}
		return "no broken doc references found (no git checkout — staleness not checked)\n", nil
	}

	sort.Slice(drifted, func(i, j int) bool {
		ci := len(drifted[i].broken) + len(drifted[i].stale)
		cj := len(drifted[j].broken) + len(drifted[j].stale)
		if ci != cj {
			return ci > cj
		}
		return drifted[i].name < drifted[j].name
	})
	if len(drifted) > limit {
		drifted = drifted[:limit]
	}

	anyStale := false
	for _, d := range drifted {
		if len(d.stale) > 0 {
			anyStale = true
			break
		}
	}

	var b strings.Builder
	if !hasGit {
		b.WriteString("no git checkout found — staleness not checked, only broken references below\n\n")
	}
	if anyStale {
		b.WriteString("note: \"stale\" compares git COMMIT dates, not working-tree mtimes — an uncommitted rewrite of the doc itself still looks stale against any committed code, since git can't see it yet. Commit the doc (or judge staleness yourself) before trusting this list on a doc you just edited.\n\n")
	}
	fmt.Fprintf(&b, "%d doc(s) with drift:\n", len(drifted))
	for _, d := range drifted {
		fmt.Fprintf(&b, "\n%s\n", d.name)
		if len(d.broken) > 0 {
			fmt.Fprintf(&b, "  broken: %s\n", strings.Join(d.broken, ", "))
		}
		if len(d.stale) > 0 {
			fmt.Fprintf(&b, "  stale — changed after this doc: %s\n", strings.Join(d.stale, ", "))
		}
	}
	return b.String(), nil
}
