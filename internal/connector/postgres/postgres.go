// Package postgres extracts metadata from a PostgreSQL database into a
// semantic graph: schemas, tables, columns, native enums, comments,
// foreign keys (incl. multi-column), indexes, views and their dependencies.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"

	"graphallthethings/internal/graph"
)

type Connector struct {
	DSN string // postgres://user:pass@host:port/db?sslmode=...
}

func New(dsn string) *Connector { return &Connector{DSN: dsn} }

func (c *Connector) Name() string { return "postgres" }

const schemaFilter = `NOT IN ('pg_catalog','information_schema','pg_toast') AND n.nspname NOT LIKE 'pg_temp%'`

func (c *Connector) Extract(ctx context.Context) (*graph.Graph, error) {
	db, err := sql.Open("pgx", c.DSN)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	var dbName string
	if err := db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		return nil, err
	}
	g := graph.New("postgres:" + dbName)
	dbID := "database:" + dbName
	g.AddNode(&graph.Node{ID: dbID, Type: graph.NodeDatabase, Name: dbName})

	enums, err := c.enums(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("enums: %w", err)
	}
	if err := c.relations(ctx, db, g, dbID, enums); err != nil {
		return nil, fmt.Errorf("relations: %w", err)
	}
	if err := c.constraints(ctx, db, g); err != nil {
		return nil, fmt.Errorf("constraints: %w", err)
	}
	if err := c.indexes(ctx, db, g); err != nil {
		return nil, fmt.Errorf("indexes: %w", err)
	}
	if err := c.viewDeps(ctx, db, g); err != nil {
		return nil, fmt.Errorf("view deps: %w", err)
	}
	return g, nil
}

// display returns the human name: unqualified for public, qualified otherwise.
func display(schema, name string) string {
	if schema == "public" {
		return name
	}
	return schema + "." + name
}

func tableID(schema, name string) string  { return "table:" + schema + "." + name }
func viewID(schema, name string) string   { return "view:" + schema + "." + name }
func columnID(schema, table, col string) string {
	return "column:" + schema + "." + table + "." + col
}

// enums returns typename -> comma-joined labels for every native enum type.
func (c *Connector) enums(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT n.nspname, t.typname, e.enumlabel
		FROM pg_enum e
		JOIN pg_type t ON t.oid = e.enumtypid
		JOIN pg_namespace n ON n.oid = t.typnamespace
		ORDER BY t.typname, e.enumsortorder`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var schema, typ, label string
		if err := rows.Scan(&schema, &typ, &label); err != nil {
			return nil, err
		}
		key := display(schema, typ)
		if out[key] == "" {
			out[key] = label
		} else {
			out[key] += ", " + label
		}
	}
	return out, rows.Err()
}

// relations extracts tables, views, materialized views and their columns.
func (c *Connector) relations(ctx context.Context, db *sql.DB, g *graph.Graph, dbID string, enums map[string]string) error {
	rows, err := db.QueryContext(ctx, `
		SELECT n.nspname, c.relname, c.relkind::text,
		       COALESCE(obj_description(c.oid, 'pg_class'), ''),
		       c.reltuples::bigint
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r','p','v','m') AND n.nspname `+schemaFilter+`
		ORDER BY n.nspname, c.relname`)
	if err != nil {
		return err
	}
	type rel struct{ schema, name, kind string }
	var rels []rel
	for rows.Next() {
		var r rel
		var comment string
		var tuples int64
		if err := rows.Scan(&r.schema, &r.name, &r.kind, &comment, &tuples); err != nil {
			return err
		}
		attrs := map[string]string{}
		if comment != "" {
			attrs["comment"] = comment
		}
		id := tableID(r.schema, r.name)
		typ := graph.NodeTable
		switch r.kind {
		case "v", "m":
			id = viewID(r.schema, r.name)
			typ = graph.NodeView
			if r.kind == "m" {
				attrs["materialized"] = "true"
			}
		default:
			if tuples >= 0 {
				attrs["row_count_estimate"] = fmt.Sprint(tuples)
			}
			if r.kind == "p" {
				attrs["partitioned"] = "true"
			}
		}
		attrs["schema"] = r.schema
		g.AddNode(&graph.Node{ID: id, Type: typ, Name: display(r.schema, r.name), Attrs: attrs})
		g.AddEdge(dbID, id, graph.EdgeHasTable, nil)
		rels = append(rels, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	colRows, err := db.QueryContext(ctx, `
		SELECT n.nspname, c.relname, c.relkind::text, a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       a.attnotnull,
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
		       COALESCE(col_description(c.oid, a.attnum), ''),
		       t.typtype::text,
		       tn.nspname, t.typname
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_type t ON t.oid = a.atttypid
		JOIN pg_namespace tn ON tn.oid = t.typnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attnum > 0 AND NOT a.attisdropped
		  AND c.relkind IN ('r','p','v','m') AND n.nspname `+schemaFilter+`
		ORDER BY n.nspname, c.relname, a.attnum`)
	if err != nil {
		return err
	}
	defer colRows.Close()
	for colRows.Next() {
		var schema, table, kind, col, dataType, dflt, comment, typtype, typSchema, typName string
		var notNull bool
		if err := colRows.Scan(&schema, &table, &kind, &col, &dataType, &notNull, &dflt, &comment, &typtype, &typSchema, &typName); err != nil {
			return err
		}
		a := map[string]string{"data_type": dataType}
		if notNull {
			a["not_null"] = "true"
		}
		if dflt != "" {
			a["default"] = dflt
		}
		if comment != "" {
			a["comment"] = comment
		}
		if typtype == "e" {
			if vals := enums[display(typSchema, typName)]; vals != "" {
				a["enum_values"] = vals
			}
		}
		// TODO(#2): categorical string columns (status, type, source) carry no
		// declared enum, so the agent can't know their valid values without
		// querying. For low-cardinality text/varchar columns, sample
		// `SELECT DISTINCT <col> ... LIMIT N` here and store it as
		// a["sample_values"]; renderColumn already has the enum-style slot to
		// print it. Guard with a distinct-count check to skip free-text columns.
		parentID := tableID(schema, table)
		if kind == "v" || kind == "m" {
			parentID = viewID(schema, table)
		}
		cid := columnID(schema, table, col)
		g.AddNode(&graph.Node{ID: cid, Type: graph.NodeColumn,
			Name: display(schema, table) + "." + col, Attrs: a})
		g.AddEdge(parentID, cid, graph.EdgeHasColumn, nil)
	}
	return colRows.Err()
}

// constraints extracts primary keys, foreign keys (incl. multi-column) and
// marks unique columns.
func (c *Connector) constraints(ctx context.Context, db *sql.DB, g *graph.Graph) error {
	rows, err := db.QueryContext(ctx, `
		SELECT n.nspname, cl.relname, con.contype::text,
		       (SELECT array_to_string(array_agg(a.attname ORDER BY x.ord), ',')
		        FROM unnest(con.conkey) WITH ORDINALITY x(attnum, ord)
		        JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = x.attnum),
		       COALESCE(fn.nspname, ''), COALESCE(fl.relname, ''),
		       COALESCE((SELECT array_to_string(array_agg(a.attname ORDER BY x.ord), ',')
		        FROM unnest(con.confkey) WITH ORDINALITY x(attnum, ord)
		        JOIN pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = x.attnum), '')
		FROM pg_constraint con
		JOIN pg_class cl ON cl.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		LEFT JOIN pg_class fl ON fl.oid = con.confrelid
		LEFT JOIN pg_namespace fn ON fn.oid = fl.relnamespace
		WHERE con.contype IN ('p','f','u') AND n.nspname `+schemaFilter)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table, ctype, cols, fSchema, fTable, fCols string
		if err := rows.Scan(&schema, &table, &ctype, &cols, &fSchema, &fTable, &fCols); err != nil {
			return err
		}
		switch ctype {
		case "p", "u":
			key := "primary_key"
			if ctype == "u" {
				key = "unique"
			}
			for _, col := range strings.Split(cols, ",") {
				if n := g.Nodes[columnID(schema, table, col)]; n != nil {
					n.Attrs[key] = "true"
				}
			}
		case "f":
			from := strings.Split(cols, ",")
			to := strings.Split(fCols, ",")
			for i := range from {
				if i < len(to) {
					g.AddEdge(columnID(schema, table, from[i]), columnID(fSchema, fTable, to[i]),
						graph.EdgeForeignKey, nil)
				}
			}
			g.AddEdge(tableID(schema, table), tableID(fSchema, fTable), graph.EdgeReferences,
				map[string]string{"from_column": cols, "to_column": fCols})
		}
	}
	return rows.Err()
}

func (c *Connector) indexes(ctx context.Context, db *sql.DB, g *graph.Graph) error {
	rows, err := db.QueryContext(ctx, `
		SELECT n.nspname, t.relname, i.relname, ix.indisunique,
		       COALESCE((SELECT array_to_string(array_agg(a.attname ORDER BY x.ord), ',')
		        FROM unnest(ix.indkey::int2[]) WITH ORDINALITY x(attnum, ord)
		        JOIN pg_attribute a ON a.attrelid = ix.indrelid AND a.attnum = x.attnum
		        WHERE x.attnum > 0), '')
		FROM pg_index ix
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_class t ON t.oid = ix.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE t.relkind IN ('r','p','m') AND NOT ix.indisprimary AND n.nspname `+schemaFilter)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table, index, cols string
		var unique bool
		if err := rows.Scan(&schema, &table, &index, &unique, &cols); err != nil {
			return err
		}
		idxID := "index:" + schema + "." + index
		a := map[string]string{"schema": schema}
		if unique {
			a["unique"] = "true"
		}
		g.AddNode(&graph.Node{ID: idxID, Type: graph.NodeIndex, Name: display(schema, index), Attrs: a})
		g.AddEdge(tableID(schema, table), idxID, graph.EdgeHasIndex, nil)
		for _, col := range strings.Split(cols, ",") {
			if col == "" {
				continue
			}
			g.AddEdge(idxID, columnID(schema, table, col), graph.EdgeIndexes, nil)
		}
	}
	return rows.Err()
}

// viewDeps links views to the relations they read from, via pg_rewrite.
func (c *Connector) viewDeps(ctx context.Context, db *sql.DB, g *graph.Graph) error {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT vn.nspname, v.relname, sn.nspname, s.relname, s.relkind::text
		FROM pg_depend d
		JOIN pg_rewrite r ON r.oid = d.objid
		JOIN pg_class v ON v.oid = r.ev_class
		JOIN pg_class s ON s.oid = d.refobjid
		JOIN pg_namespace vn ON vn.oid = v.relnamespace
		JOIN pg_namespace n ON n.oid = s.relnamespace
		JOIN pg_namespace sn ON sn.oid = s.relnamespace
		WHERE d.classid = 'pg_rewrite'::regclass
		  AND v.oid <> s.oid
		  AND v.relkind IN ('v','m') AND s.relkind IN ('r','p','v','m')
		  AND n.nspname `+schemaFilter)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var vSchema, vName, sSchema, sName, sKind string
		if err := rows.Scan(&vSchema, &vName, &sSchema, &sName, &sKind); err != nil {
			return err
		}
		target := tableID(sSchema, sName)
		if sKind == "v" || sKind == "m" {
			target = viewID(sSchema, sName)
		}
		g.AddEdge(viewID(vSchema, vName), target, graph.EdgeReferences,
			map[string]string{"via": "view"})
	}
	return rows.Err()
}
