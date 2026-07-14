// Package sqlite extracts metadata from a SQLite database file into a
// semantic graph: tables, columns, foreign keys, indexes, views, and
// CHECK-constraint pseudo-enums (SQLite has no native enums or comments).
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	_ "modernc.org/sqlite"

	"graphallthethings/internal/graph"
)

type Connector struct {
	Path string
}

func New(path string) *Connector { return &Connector{Path: path} }

func (c *Connector) Name() string { return "sqlite" }

func (c *Connector) Extract(ctx context.Context) (*graph.Graph, error) {
	db, err := sql.Open("sqlite", c.Path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	g := graph.New("sqlite:" + c.Path)
	dbID := "database:" + c.Path
	g.AddNode(&graph.Node{ID: dbID, Type: graph.NodeDatabase, Name: c.Path})

	type obj struct{ name, kind, createSQL string }
	var objs []obj
	rows, err := db.QueryContext(ctx,
		`SELECT name, type, COALESCE(sql,'') FROM sqlite_master
		 WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.name, &o.kind, &o.createSQL); err != nil {
			return nil, err
		}
		objs = append(objs, o)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	var tableNames []string
	for _, o := range objs {
		if o.kind == "table" {
			tableNames = append(tableNames, o.name)
		}
	}

	for _, o := range objs {
		if o.kind == "view" {
			viewID := "view:" + o.name
			g.AddNode(&graph.Node{ID: viewID, Type: graph.NodeView, Name: o.name,
				Attrs: map[string]string{"sql": o.createSQL}})
			g.AddEdge(dbID, viewID, graph.EdgeHasTable, nil)
			for _, t := range referencedTables(o.createSQL, o.name, tableNames) {
				g.AddEdge(viewID, "table:"+t, graph.EdgeReferences, map[string]string{"via": "view"})
			}
			continue
		}
		if err := c.extractTable(ctx, db, g, dbID, o.name, o.createSQL); err != nil {
			return nil, fmt.Errorf("table %s: %w", o.name, err)
		}
	}
	return g, nil
}

func (c *Connector) extractTable(ctx context.Context, db *sql.DB, g *graph.Graph, dbID, table, createSQL string) error {
	tableID := "table:" + table
	attrs := map[string]string{"sql": createSQL}
	var count int64
	if err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %q`, table)).Scan(&count); err == nil {
		attrs["row_count"] = fmt.Sprint(count)
	}
	g.AddNode(&graph.Node{ID: tableID, Type: graph.NodeTable, Name: table, Attrs: attrs})
	g.AddEdge(dbID, tableID, graph.EdgeHasTable, nil)

	enums := enumsFromCheck(createSQL)

	// Columns
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%q)`, table))
	if err != nil {
		return err
	}
	pkByTable := ""
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		colID := "column:" + table + "." + name
		a := map[string]string{"data_type": ctype}
		if notnull == 1 {
			a["not_null"] = "true"
		}
		if pk > 0 {
			a["primary_key"] = "true"
			if pkByTable == "" {
				pkByTable = name
			}
		}
		if dflt.Valid {
			a["default"] = dflt.String
		}
		if ev, ok := enums[strings.ToLower(name)]; ok {
			a["enum_values"] = ev
		}
		g.AddNode(&graph.Node{ID: colID, Type: graph.NodeColumn, Name: table + "." + name, Attrs: a})
		g.AddEdge(tableID, colID, graph.EdgeHasColumn, nil)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	// Foreign keys
	fkRows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA foreign_key_list(%q)`, table))
	if err != nil {
		return err
	}
	defer fkRows.Close()
	for fkRows.Next() {
		var id, seq int
		var refTable, from string
		var to sql.NullString
		var onUpdate, onDelete, match string
		if err := fkRows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return err
		}
		toCol := to.String
		if toCol == "" {
			toCol = primaryKeyOf(ctx, db, refTable) // FK to implicit PK
		}
		g.AddEdge("column:"+table+"."+from, "column:"+refTable+"."+toCol, graph.EdgeForeignKey,
			map[string]string{"on_update": onUpdate, "on_delete": onDelete})
		g.AddEdge(tableID, "table:"+refTable, graph.EdgeReferences,
			map[string]string{"from_column": from, "to_column": toCol})
	}

	// Indexes
	idxRows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA index_list(%q)`, table))
	if err != nil {
		return err
	}
	type idx struct {
		name   string
		unique bool
	}
	var idxs []idx
	for idxRows.Next() {
		var seq, uniq, partial int
		var name, origin string
		if err := idxRows.Scan(&seq, &name, &uniq, &origin, &partial); err != nil {
			return err
		}
		idxs = append(idxs, idx{name, uniq == 1})
	}
	if err := idxRows.Close(); err != nil {
		return err
	}
	for _, ix := range idxs {
		idxID := "index:" + ix.name
		a := map[string]string{}
		if ix.unique {
			a["unique"] = "true"
		}
		g.AddNode(&graph.Node{ID: idxID, Type: graph.NodeIndex, Name: ix.name, Attrs: a})
		g.AddEdge(tableID, idxID, graph.EdgeHasIndex, nil)
		colRows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA index_info(%q)`, ix.name))
		if err != nil {
			return err
		}
		for colRows.Next() {
			var seqno, cid int
			var col sql.NullString
			if err := colRows.Scan(&seqno, &cid, &col); err != nil {
				return err
			}
			if col.Valid {
				g.AddEdge(idxID, "column:"+table+"."+col.String, graph.EdgeIndexes, nil)
			}
		}
		if err := colRows.Close(); err != nil {
			return err
		}
	}
	return nil
}

func primaryKeyOf(ctx context.Context, db *sql.DB, table string) string {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%q)`, table))
	if err != nil {
		return "rowid"
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return "rowid"
		}
		if pk > 0 {
			return name
		}
	}
	return "rowid"
}

var checkInRe = regexp.MustCompile(`(?i)CHECK\s*\(\s*"?(\w+)"?\s+IN\s*\(([^)]+)\)`)

// enumsFromCheck parses CHECK (col IN ('a','b')) constraints out of the
// CREATE TABLE statement — the closest thing SQLite has to enums.
func enumsFromCheck(createSQL string) map[string]string {
	out := map[string]string{}
	for _, m := range checkInRe.FindAllStringSubmatch(createSQL, -1) {
		var vals []string
		for _, v := range strings.Split(m[2], ",") {
			vals = append(vals, strings.Trim(strings.TrimSpace(v), `'"`))
		}
		out[strings.ToLower(m[1])] = strings.Join(vals, ", ")
	}
	return out
}

// referencedTables finds known table names mentioned in a view's SQL.
func referencedTables(viewSQL, viewName string, tables []string) []string {
	var out []string
	for _, t := range tables {
		if t == viewName {
			continue
		}
		re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(t) + `\b`)
		if re.MatchString(viewSQL) {
			out = append(out, t)
		}
	}
	return out
}
