package graph

// SQLite persistence for the graph. Selected by file extension (.db/.sqlite):
// a full save rewrites all rows in one transaction; a journaled save (see
// StartJournal) writes only the delta, which keeps incremental refresh of a
// big repo at milliseconds instead of re-serializing the whole graph.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// IsSQLitePath reports whether path selects the SQLite persistence format.
func IsSQLitePath(path string) bool {
	switch filepath.Ext(path) {
	case ".db", ".sqlite", ".sqlite3":
		return true
	}
	return false
}

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS nodes (
	id    TEXT PRIMARY KEY,
	type  TEXT NOT NULL,
	name  TEXT NOT NULL,
	attrs TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS edges (
	efrom TEXT NOT NULL,
	eto   TEXT NOT NULL,
	etype TEXT NOT NULL,
	attrs TEXT NOT NULL DEFAULT '{}',
	UNIQUE(efrom, eto, etype)
);
CREATE INDEX IF NOT EXISTS idx_edges_from ON edges(efrom);
CREATE INDEX IF NOT EXISTS idx_edges_to   ON edges(eto);
CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(id UNINDEXED, type UNINDEXED, text);
`

func openGraphDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(sqliteSchema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func marshalAttrs(attrs map[string]string) string {
	if len(attrs) == 0 {
		return "{}"
	}
	b, err := json.Marshal(attrs)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// saveSQLite persists the graph. With an active journal and an existing
// database it applies only the delta; otherwise it rewrites everything.
func (g *Graph) saveSQLite(path string) error {
	_, statErr := os.Stat(path)
	db, err := openGraphDB(path)
	if err != nil {
		return err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	j := g.journal
	if j != nil && statErr == nil {
		if err := applyDelta(tx, g, j); err != nil {
			return err
		}
	} else {
		for _, table := range []string{"nodes", "edges", "fts"} {
			if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
				return err
			}
		}
		if err := insertAll(tx, g); err != nil {
			return err
		}
	}

	for k, v := range map[string]string{
		"source":       g.Source,
		"extracted_at": g.ExtractedAt.Format(time.RFC3339Nano),
	} {
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if j != nil {
		g.StartJournal() // reset: the delta is on disk now
	}
	return nil
}

func insertAll(tx *sql.Tx, g *Graph) error {
	ns, err := tx.Prepare(`INSERT OR REPLACE INTO nodes(id,type,name,attrs) VALUES(?,?,?,?)`)
	if err != nil {
		return err
	}
	defer ns.Close()
	for _, n := range g.Nodes {
		if _, err := ns.Exec(n.ID, n.Type, n.Name, marshalAttrs(n.Attrs)); err != nil {
			return err
		}
	}
	es, err := tx.Prepare(`INSERT OR IGNORE INTO edges(efrom,eto,etype,attrs) VALUES(?,?,?,?)`)
	if err != nil {
		return err
	}
	defer es.Close()
	for _, e := range g.Edges {
		if _, err := es.Exec(e.From, e.To, e.Type, marshalAttrs(e.Attrs)); err != nil {
			return err
		}
	}
	fs, err := tx.Prepare(`INSERT INTO fts(id,type,text) VALUES(?,?,?)`)
	if err != nil {
		return err
	}
	defer fs.Close()
	for id, n := range g.Nodes {
		if _, err := fs.Exec(id, n.Type, g.NodeText(id)); err != nil {
			return err
		}
	}
	return nil
}

func applyDelta(tx *sql.Tx, g *Graph, j *journal) error {
	nd, err := tx.Prepare(`DELETE FROM nodes WHERE id = ?`)
	if err != nil {
		return err
	}
	defer nd.Close()
	ed, err := tx.Prepare(`DELETE FROM edges WHERE efrom = ? OR eto = ?`)
	if err != nil {
		return err
	}
	defer ed.Close()
	for id := range j.removedNodes {
		if _, err := nd.Exec(id); err != nil {
			return err
		}
		if _, err := ed.Exec(id, id); err != nil {
			return err
		}
	}

	ns, err := tx.Prepare(`INSERT OR REPLACE INTO nodes(id,type,name,attrs) VALUES(?,?,?,?)`)
	if err != nil {
		return err
	}
	defer ns.Close()
	for id := range j.addedNodes {
		n := g.Nodes[id]
		if n == nil {
			continue // added then removed within the same session
		}
		if _, err := ns.Exec(n.ID, n.Type, n.Name, marshalAttrs(n.Attrs)); err != nil {
			return err
		}
	}

	es, err := tx.Prepare(`INSERT OR IGNORE INTO edges(efrom,eto,etype,attrs) VALUES(?,?,?,?)`)
	if err != nil {
		return err
	}
	defer es.Close()
	for _, e := range j.addedEdges {
		if j.removedNodes[e.From] || j.removedNodes[e.To] {
			// Endpoint later evicted; the edge died with it.
			if g.Nodes[e.From] == nil || g.Nodes[e.To] == nil {
				continue
			}
		}
		if _, err := es.Exec(e.From, e.To, e.Type, marshalAttrs(e.Attrs)); err != nil {
			return err
		}
	}

	fd, err := tx.Prepare(`DELETE FROM fts WHERE id = ?`)
	if err != nil {
		return err
	}
	defer fd.Close()
	fi, err := tx.Prepare(`INSERT INTO fts(id,type,text) VALUES(?,?,?)`)
	if err != nil {
		return err
	}
	defer fi.Close()
	for id := range j.removedNodes {
		if _, err := fd.Exec(id); err != nil {
			return err
		}
	}
	for id := range j.addedNodes {
		n := g.Nodes[id]
		if n == nil {
			continue
		}
		if _, err := fd.Exec(id); err != nil {
			return err
		}
		if _, err := fi.Exec(id, n.Type, g.NodeText(id)); err != nil {
			return err
		}
	}
	return nil
}

// FTSQuery runs an indexed full-text search over a SQLite graph's fts table.
// Terms are OR-quoted so free-form phrasing can't break MATCH syntax. Returns
// node ids best-first (bm25); empty on any error or when the table has no
// rows yet (pre-FTS graphs), so callers fall back to the in-memory scan.
func FTSQuery(path, query, nodeType string, limit int) []string {
	db, err := openGraphDB(path)
	if err != nil {
		return nil
	}
	defer db.Close()

	var quoted []string
	for _, t := range strings.Fields(query) {
		if t = strings.ReplaceAll(t, `"`, ""); t != "" {
			quoted = append(quoted, `"`+t+`"`)
		}
	}
	if len(quoted) == 0 {
		return nil
	}
	match := strings.Join(quoted, " OR ")

	q := `SELECT id FROM fts WHERE fts MATCH ? ORDER BY bm25(fts) LIMIT ?`
	args := []any{match, limit}
	if nodeType != "" {
		q = `SELECT id FROM fts WHERE fts MATCH ? AND type = ? ORDER BY bm25(fts) LIMIT ?`
		args = []any{match, nodeType, limit}
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// FileMtimes returns relPath → mtime for every physical file node — the
// recorded disk state a drift check compares against.
func FileMtimes(g *Graph) map[string]string {
	m := map[string]string{}
	for _, n := range g.Nodes {
		if n.Type == NodeFile && n.Attrs["path"] != "" {
			m[n.Name] = n.Attrs["mtime"]
		}
	}
	return m
}

// LoadCodebaseState reads just the source string and the file-node mtimes —
// the inputs of a drift check — without materializing the whole graph.
// Cheap for SQLite; falls back to a full load for JSON graphs.
func LoadCodebaseState(path string) (string, map[string]string, error) {
	if !IsSQLitePath(path) {
		g, err := LoadRaw(path)
		if err != nil {
			return "", nil, err
		}
		return g.Source, FileMtimes(g), nil
	}
	if _, err := os.Stat(path); err != nil {
		return "", nil, err
	}
	db, err := openGraphDB(path)
	if err != nil {
		return "", nil, err
	}
	defer db.Close()

	var source string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='source'`).Scan(&source); err != nil {
		return "", nil, err
	}
	rows, err := db.Query(`SELECT name, attrs FROM nodes WHERE type = ?`, NodeFile)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var name, attrsJSON string
		if err := rows.Scan(&name, &attrsJSON); err != nil {
			return "", nil, err
		}
		attrs := map[string]string{}
		if json.Unmarshal([]byte(attrsJSON), &attrs) == nil && attrs["path"] != "" {
			m[name] = attrs["mtime"]
		}
	}
	return source, m, rows.Err()
}

func loadSQLite(path string) (*Graph, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := openGraphDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	g := New("")
	rows, err := db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return nil, err
		}
		switch k {
		case "source":
			g.Source = v
		case "extracted_at":
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				g.ExtractedAt = t
			}
		}
	}
	rows.Close()

	rows, err = db.Query(`SELECT id, type, name, attrs FROM nodes`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n Node
		var attrs string
		if err := rows.Scan(&n.ID, &n.Type, &n.Name, &attrs); err != nil {
			rows.Close()
			return nil, err
		}
		n.Attrs = map[string]string{}
		if err := json.Unmarshal([]byte(attrs), &n.Attrs); err != nil {
			rows.Close()
			return nil, fmt.Errorf("node %s attrs: %w", n.ID, err)
		}
		g.Nodes[n.ID] = &n
	}
	rows.Close()

	rows, err = db.Query(`SELECT efrom, eto, etype, attrs FROM edges`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Edge
		var attrs string
		if err := rows.Scan(&e.From, &e.To, &e.Type, &attrs); err != nil {
			return nil, err
		}
		if attrs != "{}" {
			e.Attrs = map[string]string{}
			if err := json.Unmarshal([]byte(attrs), &e.Attrs); err != nil {
				return nil, fmt.Errorf("edge %s→%s attrs: %w", e.From, e.To, err)
			}
		}
		g.Edges = append(g.Edges, e)
	}
	return g, rows.Err()
}
