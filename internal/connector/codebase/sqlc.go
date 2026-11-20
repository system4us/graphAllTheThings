package codebase

// Native sqlc support: connects sqlc.yaml/sqlc.json, schema/*.sql, queries/*.sql,
// and generated *.sql.go / models.go files in the codebase graph.
//
// Edges and nodes produced:
//   - GENERATES (via=sqlc): queries/*.sql -> *.sql.go, schema/*.sql -> models.go
//   - NodeModel (orm=sqlc): tables parsed from CREATE TABLE in schema/*.sql,
//     plus REFERENCES edges for FOREIGN KEY / REFERENCES clauses
//   - USES_MODEL (via=sqlc): generated query function -> model:<table> (and
//     query .sql file -> model:<table>)
//   - DEPENDS_ON (via=sqlc): generated query function -> query .sql file

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"graphallthethings/internal/graph"
)

type sqlcConfig struct {
	Version string      `yaml:"version" json:"version"`
	SQL     []sqlcEntry `yaml:"sql" json:"sql"`
}

type sqlcEntry struct {
	Engine  string          `yaml:"engine" json:"engine"`
	Schema  stringOrSlice   `yaml:"schema" json:"schema"`
	Queries stringOrSlice   `yaml:"queries" json:"queries"`
	Gen     sqlcGenConfig   `yaml:"gen" json:"gen"`
}

type sqlcGenConfig struct {
	Go         *sqlcGenTarget `yaml:"go" json:"go"`
	TypeScript *sqlcGenTarget `yaml:"typescript" json:"typescript"`
	Python     *sqlcGenTarget `yaml:"python" json:"python"`
	Kotlin     *sqlcGenTarget `yaml:"kotlin" json:"kotlin"`
}

type sqlcGenTarget struct {
	Out string `yaml:"out" json:"out"`
}

// stringOrSlice decodes a YAML/JSON field that may be either a single string or
// a list of strings (sqlc allows both for schema and queries).
type stringOrSlice []string

func (s *stringOrSlice) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		if value.Value != "" {
			*s = []string{value.Value}
		}
		return nil
	}
	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*s = list
	return nil
}

var (
	sqlcNameRe       = regexp.MustCompile(`(?m)^--\s*name:\s*([A-Za-z_][A-Za-z0-9_]*)\s+:([a-z]+)\b`)
	sqlcCreateTblRe  = regexp.MustCompile(`(?is)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z0-9_."` + "`" + `]+)\s*\(`)
	sqlcRefRe        = regexp.MustCompile(`(?i)\bREFERENCES\s+([A-Za-z0-9_."` + "`" + `]+)`)
	sqlcCTERe        = regexp.MustCompile(`(?i)(?:\bWITH\s+(?:RECURSIVE\s+)?|,\s*)([A-Za-z_][A-Za-z0-9_]*)\s+AS\s*\(`)
	sqlcTableUsageRe = regexp.MustCompile(`(?i)\b(?:FROM|JOIN|INTO|UPDATE)\s+([A-Za-z0-9_."` + "`" + `]+)`)
)

func cleanSQLIdent(raw string) string {
	raw = strings.TrimSpace(raw)
	// Strip schema prefix (e.g. public.users -> users)
	if idx := strings.LastIndex(raw, "."); idx >= 0 {
		raw = raw[idx+1:]
	}
	return strings.Trim(raw, "\"'`[]")
}

func isSQLKeyword(w string) bool {
	switch strings.ToLower(w) {
	case "select", "where", "set", "values", "on", "as", "inner", "left", "right",
		"outer", "full", "cross", "lateral", "unnest", "generate_series", "only",
		"default", "null", "true", "false", "and", "or", "not", "in", "exists":
		return true
	}
	return false
}

// wireSqlc detects sqlc configurations and generated files, creating GENERATES
// edges, schema-derived NodeModel nodes, and query -> model USES_MODEL edges.
func (c *Connector) wireSqlc(g *graph.Graph) {
	// Index all file nodes by slash-normalized relative path.
	fileNodes := map[string]string{} // relPath -> nodeID
	var configFiles []string
	for id, n := range g.Nodes {
		if n.Type != graph.NodeFile || n.Attrs["external"] == "true" {
			continue
		}
		p := filepath.ToSlash(n.Name)
		fileNodes[p] = id
		base := strings.ToLower(filepath.Base(p))
		if base == "sqlc.yaml" || base == "sqlc.yml" || base == "sqlc.json" {
			configFiles = append(configFiles, p)
		}
	}

	// Collect schema files, query files, and output dirs from sqlc configs.
	type sqlcScope struct {
		schemaFiles []string
		queryFiles  []string
		outDirs     []string
	}
	var scopes []sqlcScope

	resolveSQLPaths := func(baseDir string, specs []string) []string {
		seen := map[string]bool{}
		var out []string
		for _, spec := range specs {
			rel := filepath.ToSlash(filepath.Clean(filepath.Join(baseDir, spec)))
			rel = strings.TrimPrefix(rel, "./")
			if _, ok := fileNodes[rel]; ok && strings.HasSuffix(strings.ToLower(rel), ".sql") {
				if !seen[rel] {
					seen[rel] = true
					out = append(out, rel)
				}
				continue
			}
			prefix := rel
			if prefix != "" && !strings.HasSuffix(prefix, "/") {
				prefix += "/"
			}
			for p := range fileNodes {
				if strings.HasPrefix(p, prefix) && strings.HasSuffix(strings.ToLower(p), ".sql") {
					if !seen[p] {
						seen[p] = true
						out = append(out, p)
					}
				}
			}
		}
		sort.Strings(out)
		return out
	}

	for _, cfgRel := range configFiles {
		data, err := os.ReadFile(filepath.Join(c.dir, filepath.FromSlash(cfgRel)))
		if err != nil {
			continue
		}
		var cfg sqlcConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			continue
		}
		cfgDir := filepath.ToSlash(filepath.Dir(cfgRel))
		if cfgDir == "." {
			cfgDir = ""
		}
		for _, entry := range cfg.SQL {
			sc := sqlcScope{
				schemaFiles: resolveSQLPaths(cfgDir, entry.Schema),
				queryFiles:  resolveSQLPaths(cfgDir, entry.Queries),
			}
			for _, tgt := range []*sqlcGenTarget{entry.Gen.Go, entry.Gen.TypeScript, entry.Gen.Python, entry.Gen.Kotlin} {
				if tgt != nil && tgt.Out != "" {
					outDir := filepath.ToSlash(filepath.Clean(filepath.Join(cfgDir, tgt.Out)))
					outDir = strings.TrimPrefix(outDir, "./")
					sc.outDirs = append(sc.outDirs, outDir)
				}
			}
			if len(sc.schemaFiles) > 0 || len(sc.queryFiles) > 0 || len(sc.outDirs) > 0 {
				scopes = append(scopes, sc)
			}
		}
	}

	// Also detect sqlc-generated files directly via header even without sqlc.yaml.
	type genSqlcFile struct {
		relPath   string
		sourceSQL string // basename from "// source: users.sql"
	}
	var genFiles []genSqlcFile
	for relPath := range fileNodes {
		ext := strings.ToLower(filepath.Ext(relPath))
		if ext != ".go" && ext != ".ts" && ext != ".py" && ext != ".kt" {
			continue
		}
		if isSqlc, src := inspectSqlcHeader(filepath.Join(c.dir, filepath.FromSlash(relPath))); isSqlc {
			genFiles = append(genFiles, genSqlcFile{relPath: relPath, sourceSQL: src})
		}
	}
	if len(scopes) == 0 && len(genFiles) == 0 {
		return
	}

	// 1. Parse schema/*.sql into NodeModel nodes + REFERENCES edges.
	tableToModelID := map[string]string{} // lowercase table name -> model node ID
	for id, n := range g.Nodes {
		if n.Type == graph.NodeModel {
			tbl := strings.ToLower(n.Attrs["table"])
			if tbl == "" {
				tbl = strings.ToLower(n.Name)
			}
			tableToModelID[tbl] = id
		}
	}

	type pendingFK struct {
		fromTable string
		toTable   string
	}
	var fks []pendingFK

	schemaSet := map[string]bool{}
	for _, sc := range scopes {
		for _, sf := range sc.schemaFiles {
			if schemaSet[sf] {
				continue
			}
			schemaSet[sf] = true
			data, err := os.ReadFile(filepath.Join(c.dir, filepath.FromSlash(sf)))
			if err != nil {
				continue
			}
			tables := parseSQLSchemaTables(string(data))
			for _, tbl := range tables {
				lower := strings.ToLower(tbl.name)
				mid, exists := tableToModelID[lower]
				if !exists {
					mid = "model:" + tbl.name
					attrs := map[string]string{
						"table": tbl.name,
						"file":  sf,
						"orm":   "sqlc",
					}
					if len(tbl.columns) > 0 {
						attrs["fields"] = strings.Join(tbl.columns, ", ")
					}
					g.AddNode(&graph.Node{
						ID:    mid,
						Type:  graph.NodeModel,
						Name:  tbl.name,
						Attrs: attrs,
					})
					if fid := fileNodes[sf]; fid != "" {
						g.AddEdge(mid, fid, graph.EdgeBelongsTo, nil)
					}
					tableToModelID[lower] = mid
				}
				for _, ref := range tbl.refs {
					fks = append(fks, pendingFK{fromTable: lower, toTable: strings.ToLower(ref)})
				}
			}
		}
	}
	for _, fk := range fks {
		fromID := tableToModelID[fk.fromTable]
		toID := tableToModelID[fk.toTable]
		if fromID != "" && toID != "" && fromID != toID {
			g.AddEdge(fromID, toID, graph.EdgeReferences, map[string]string{"via": "sqlc"})
		}
	}

	// 2. Wire GENERATES edges: schema/*.sql -> out/models.go, queries/*.sql -> *.sql.go
	for _, sc := range scopes {
		for _, outDir := range sc.outDirs {
			modelsRel := filepath.ToSlash(filepath.Join(outDir, "models.go"))
			if dstID := fileNodes[modelsRel]; dstID != "" {
				for _, sf := range sc.schemaFiles {
					if srcID := fileNodes[sf]; srcID != "" {
						g.AddEdge(srcID, dstID, graph.EdgeGenerates, map[string]string{"via": "sqlc"})
					}
				}
			}
		}
	}

	// Build query file lookup by basename for matching `// source: users.sql`.
	queryFilesByBase := map[string][]string{}
	allQueryFiles := map[string]bool{}
	for _, sc := range scopes {
		for _, qf := range sc.queryFiles {
			allQueryFiles[qf] = true
		}
	}
	for relPath := range fileNodes {
		if strings.HasSuffix(strings.ToLower(relPath), ".sql") && !schemaSet[relPath] {
			base := filepath.Base(relPath)
			queryFilesByBase[base] = append(queryFilesByBase[base], relPath)
		}
	}

	// Index functions by file path so we can link generated functions to queries & models.
	funcsByFile := map[string][]*graph.Node{}
	for _, n := range g.Nodes {
		if n.Type == graph.NodeFunction && n.Attrs["external"] != "true" {
			f := filepath.ToSlash(n.Attrs["file"])
			funcsByFile[f] = append(funcsByFile[f], n)
		}
	}

	// Match generated files -> source query .sql files.
	genFileToQueryFile := map[string]string{}
	for _, gf := range genFiles {
		if gf.sourceSQL == "" {
			continue
		}
		candidates := queryFilesByBase[gf.sourceSQL]
		var matched string
		if len(candidates) == 1 {
			matched = candidates[0]
		} else if len(candidates) > 1 {
			// Prefer one explicitly listed in a sqlc.yaml queries scope.
			for _, cand := range candidates {
				if allQueryFiles[cand] {
					matched = cand
					break
				}
			}
		}
		if matched != "" {
			genFileToQueryFile[gf.relPath] = matched
			allQueryFiles[matched] = true
			if srcID, dstID := fileNodes[matched], fileNodes[gf.relPath]; srcID != "" && dstID != "" {
				g.AddEdge(srcID, dstID, graph.EdgeGenerates, map[string]string{"via": "sqlc"})
			}
		}
	}

	// 3. Parse `-- name: QueryName :cmd` blocks in query .sql files and wire
	//    generated Go functions -> query .sql file (DEPENDS_ON) & -> models (USES_MODEL).
	type queryMeta struct {
		name      string
		cmd       string
		queryFile string
		tables    []string
	}
	queriesByFile := map[string][]queryMeta{}
	for qf := range allQueryFiles {
		data, err := os.ReadFile(filepath.Join(c.dir, filepath.FromSlash(qf)))
		if err != nil {
			continue
		}
		qs := parseSqlcQueries(string(data))
		for _, q := range qs {
			queriesByFile[qf] = append(queriesByFile[qf], queryMeta{
				name:      q.name,
				cmd:       q.cmd,
				queryFile: qf,
				tables:    q.tables,
			})
			// Ensure table models exist even if schema wasn't parsed, and link query file -> model.
			if qFileID := fileNodes[qf]; qFileID != "" {
				for _, tbl := range q.tables {
					if mid := tableToModelID[strings.ToLower(tbl)]; mid != "" {
						g.AddEdge(qFileID, mid, graph.EdgeUsesModel, map[string]string{"via": "sqlc", "query": q.name})
					}
				}
			}
		}
	}

	// Link generated functions to their sqlc query source file and touched models.
	for genRel, qf := range genFileToQueryFile {
		qList := queriesByFile[qf]
		if len(qList) == 0 {
			continue
		}
		byName := map[string]queryMeta{}
		for _, q := range qList {
			byName[strings.ToLower(q.name)] = q
		}
		qFileID := fileNodes[qf]
		for _, fn := range funcsByFile[genRel] {
			qm, ok := byName[strings.ToLower(fn.Name)]
			if !ok {
				continue
			}
			if qFileID != "" {
				g.AddEdge(fn.ID, qFileID, graph.EdgeDependsOn, map[string]string{
					"via":   "sqlc",
					"query": qm.name,
					"cmd":   qm.cmd,
				})
			}
			for _, tbl := range qm.tables {
				if mid := tableToModelID[strings.ToLower(tbl)]; mid != "" {
					g.AddEdge(fn.ID, mid, graph.EdgeUsesModel, map[string]string{
						"via":   "sqlc",
						"query": qm.name,
					})
				}
			}
		}
	}
}

// inspectSqlcHeader checks the first 12 lines of a file for the standard sqlc
// generation marker and optional `// source: <file>.sql` line.
func inspectSqlcHeader(absPath string) (isSqlc bool, sourceSQL string) {
	f, err := os.Open(absPath)
	if err != nil {
		return false, ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for i := 0; i < 12 && sc.Scan(); i++ {
		line := strings.TrimSpace(sc.Text())
		lower := strings.ToLower(line)
		if strings.Contains(lower, "generated by sqlc") {
			isSqlc = true
		}
		if strings.HasPrefix(lower, "// source:") || strings.HasPrefix(lower, "# source:") {
			idx := strings.Index(line, ":")
			if idx >= 0 {
				sourceSQL = strings.TrimSpace(line[idx+1:])
			}
		}
	}
	return isSqlc, sourceSQL
}

type parsedSQLTable struct {
	name    string
	columns []string
	refs    []string
}

func parseSQLSchemaTables(sqlText string) []parsedSQLTable {
	var out []parsedSQLTable
	locs := sqlcCreateTblRe.FindAllStringSubmatchIndex(sqlText, -1)
	for _, loc := range locs {
		name := cleanSQLIdent(sqlText[loc[2]:loc[3]])
		if name == "" {
			continue
		}
		// Walk balanced parens starting at the '(' at loc[1]-1.
		openIdx := loc[1] - 1
		depth := 0
		endIdx := openIdx
		for i := openIdx; i < len(sqlText); i++ {
			switch sqlText[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					endIdx = i
				}
			}
			if depth == 0 && i > openIdx {
				break
			}
		}
		body := ""
		if endIdx > openIdx {
			body = sqlText[openIdx+1 : endIdx]
		}
		var cols []string
		refSeen := map[string]bool{}
		var refs []string
		for _, m := range sqlcRefRe.FindAllStringSubmatch(body, -1) {
			refTbl := cleanSQLIdent(m[1])
			if refTbl != "" && !refSeen[refTbl] {
				refSeen[refTbl] = true
				refs = append(refs, refTbl)
			}
		}
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "--") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			first := strings.ToLower(strings.Trim(fields[0], "\"'`"))
			switch first {
			case "primary", "foreign", "unique", "constraint", "check", "index", "key":
				continue
			}
			col := strings.Trim(fields[0], "\"'`,")
			if col != "" {
				cols = append(cols, col)
			}
		}
		out = append(out, parsedSQLTable{name: name, columns: cols, refs: refs})
	}
	return out
}

type parsedSqlcQuery struct {
	name   string
	cmd    string
	tables []string
}

func parseSqlcQueries(sqlText string) []parsedSqlcQuery {
	matches := sqlcNameRe.FindAllStringSubmatchIndex(sqlText, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []parsedSqlcQuery
	for i, loc := range matches {
		name := sqlText[loc[2]:loc[3]]
		cmd := sqlText[loc[4]:loc[5]]
		bodyStart := loc[1]
		bodyEnd := len(sqlText)
		if i+1 < len(matches) {
			bodyEnd = matches[i+1][0]
		}
		body := sqlText[bodyStart:bodyEnd]
		out = append(out, parsedSqlcQuery{
			name:   name,
			cmd:    cmd,
			tables: extractSQLTables(body),
		})
	}
	return out
}

func extractSQLTables(queryBody string) []string {
	ctes := map[string]bool{}
	for _, m := range sqlcCTERe.FindAllStringSubmatch(queryBody, -1) {
		ctes[strings.ToLower(m[1])] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range sqlcTableUsageRe.FindAllStringSubmatch(queryBody, -1) {
		tbl := cleanSQLIdent(m[1])
		lower := strings.ToLower(tbl)
		if tbl == "" || isSQLKeyword(lower) || ctes[lower] || seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, tbl)
	}
	return out
}
