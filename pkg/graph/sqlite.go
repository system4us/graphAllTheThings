package graph

// SQLite persistence for the graph. Selected by file extension (.db/.sqlite/.sqlite3):
// a full save rewrites all rows in one transaction; a journaled save (see
// StartJournal) writes only the delta, keeping incremental refreshes fast.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
		g.StartJournal()
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
			continue
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
			if g.Nodes[e.From] == nil || g.Nodes[e.To] == nil {
				continue
			}
		}
		if _, err := es.Exec(e.From, e.To, e.Type, marshalAttrs(e.Attrs)); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS fts_stale(id TEXT PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM fts_stale`); err != nil {
		return err
	}
	ins, err := tx.Prepare(`INSERT OR IGNORE INTO fts_stale(id) VALUES(?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for id := range j.removedNodes {
		if _, err := ins.Exec(id); err != nil {
			return err
		}
	}
	for id := range j.addedNodes {
		if g.Nodes[id] == nil {
			continue
		}
		if _, err := ins.Exec(id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM fts WHERE id IN (SELECT id FROM fts_stale)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE fts_stale`); err != nil {
		return err
	}

	fi, err := tx.Prepare(`INSERT INTO fts(id,type,text) VALUES(?,?,?)`)
	if err != nil {
		return err
	}
	defer fi.Close()
	for id := range j.addedNodes {
		n := g.Nodes[id]
		if n == nil {
			continue
		}
		if _, err := fi.Exec(id, n.Type, g.NodeText(id)); err != nil {
			return err
		}
	}
	return nil
}

// FTSQuery runs an indexed full-text search over a SQLite graph's fts table.
// Terms are OR-quoted so free-form phrasing cannot break MATCH syntax. Returns
// node ids best-first (bm25); empty on any error or when the table has no rows.
func FTSQuery(path, query, nodeType string, limit int) []string {
	return FTSQueryTerms(path, QueryTerms(query), nodeType, limit)
}

// FTSQueryTerms runs an indexed full-text search over a SQLite graph's fts
// table using pre-tokenized search terms (e.g. from a domain-aware tokenizer
// that preserves short acronyms like "it" or "hr").
func FTSQueryTerms(path string, terms []string, nodeType string, limit int) []string {
	if len(terms) == 0 || limit <= 0 {
		return nil
	}
	db, err := openGraphDB(path)
	if err != nil {
		return nil
	}
	defer db.Close()

	var quoted []string
	for _, t := range terms {
		if t = strings.ReplaceAll(strings.TrimSpace(t), `"`, ""); t != "" {
			if st := Stem(t); len([]rune(st)) >= 4 {
				quoted = append(quoted, `"`+st+`"*`)
			} else {
				quoted = append(quoted, `"`+t+`"`)
			}
		}
	}
	if len(quoted) == 0 {
		return nil
	}
	runMatch := func(match string, capLimit int) []string {
		q := `SELECT id FROM fts WHERE fts MATCH ? ORDER BY bm25(fts) LIMIT ?`
		args := []any{match, capLimit}
		if nodeType != "" {
			q = `SELECT id FROM fts WHERE fts MATCH ? AND type = ? ORDER BY bm25(fts) LIMIT ?`
			args = []any{match, nodeType, capLimit}
		}
		rows, err := db.Query(q, args...)
		if err != nil {
			return nil
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				out = append(out, id)
			}
		}
		return out
	}

	var ids []string
	seen := map[string]bool{}
	if len(quoted) > 1 {
		for _, id := range runMatch(strings.Join(quoted, " AND "), limit) {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) < limit {
		for _, id := range runMatch(strings.Join(quoted, " OR "), limit) {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
				if len(ids) >= limit {
					break
				}
			}
		}
	}
	return ids
}

// LoadMeta reads the graph metadata key/value pairs (such as "source" and
// "extracted_at") without materializing nodes or edges for SQLite graphs.
// Falls back to LoadRaw for JSON graphs.
func LoadMeta(path string) (map[string]string, error) {
	if !IsSQLitePath(path) {
		g, err := LoadRaw(path)
		if err != nil {
			return nil, err
		}
		meta := map[string]string{"source": g.Source}
		if !g.ExtractedAt.IsZero() {
			meta["extracted_at"] = g.ExtractedAt.Format(time.RFC3339Nano)
		}
		return meta, nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := openGraphDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	meta := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		meta[k] = v
	}
	return meta, rows.Err()
}

// QueryNodesByType reads only nodes of the given type from a SQLite graph
// without materializing the rest of the graph (useful for tools that only need
// one entity kind, such as route facts or file mtimes). Falls back to LoadRaw
// for JSON graphs. Results are sorted by Name then ID.
func QueryNodesByType(path, nodeType string) ([]*Node, error) {
	if !IsSQLitePath(path) {
		g, err := LoadRaw(path)
		if err != nil {
			return nil, err
		}
		return g.NodesByType(nodeType), nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := openGraphDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT id, type, name, attrs FROM nodes WHERE type = ?`, nodeType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Node
	for rows.Next() {
		var n Node
		var attrsJSON string
		if err := rows.Scan(&n.ID, &n.Type, &n.Name, &attrsJSON); err != nil {
			return nil, err
		}
		n.Attrs = map[string]string{}
		if err := json.Unmarshal([]byte(attrsJSON), &n.Attrs); err != nil {
			return nil, fmt.Errorf("node %s attrs: %w", n.ID, err)
		}
		out = append(out, &n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
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
