// gatt — graph all the things. Extract metadata from a source into a
// semantic graph, index it for semantic search, query it from the terminal,
// and serve it to agents over MCP.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"graphallthethings/internal/connector"
	"graphallthethings/internal/connector/codebase"
	"graphallthethings/internal/embed"
	"graphallthethings/internal/engine"
	"graphallthethings/internal/enrich"
	"graphallthethings/internal/graph"
	"graphallthethings/internal/indexer"
	"graphallthethings/internal/mcpserver"
	"graphallthethings/internal/store"
	"graphallthethings/internal/store/local"
	"graphallthethings/internal/store/qdrant"
)

// skillMD is the Claude Code skill that teaches agents graph-first navigation.
// It ships in the binary so `gatt install` drops it next to the MCP
// registration, keeping the skill in lockstep with the commands this build
// actually exposes.
//
//go:embed skill/SKILL.md
var skillMD string

// defaultGraph is gatt-out/graph.db for a fresh project: SQLite's journaled
// save is what makes incremental refresh (autoRefreshCodebase, --update)
// cheap, so a first extract with no --out should land there. An existing
// project that already extracted to the legacy JSON default keeps using it —
// the format is chosen at first extract and stays sticky for every later
// command, never silently switched underneath an existing graph.
var defaultGraph = "gatt-out/graph.db"

func init() {
	if _, err := os.Stat(defaultGraph); err != nil {
		if _, err := os.Stat("gatt-out/graph.json"); err == nil {
			defaultGraph = "gatt-out/graph.json"
		}
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "extract":
		err = cmdExtract(ctx, os.Args[2:])
	case "enrich":
		err = cmdEnrich(ctx, os.Args[2:])
	case "index":
		err = cmdIndex(ctx, os.Args[2:])
	case "search", "find":
		err = cmdSearch(ctx, os.Args[2:])
	case "query":
		err = cmdQuery(ctx, os.Args[2:])
	case "impact":
		err = cmdImpact(ctx, os.Args[2:])
	case "blast":
		err = cmdBlast(ctx, os.Args[2:])
	case "doc-drift", "docdrift":
		err = cmdDocDrift(ctx, os.Args[2:])
	case "grep":
		err = cmdGrep(ctx, os.Args[2:])
	case "tree":
		err = cmdTree(ctx, os.Args[2:])
	case "routes":
		err = cmdRoutes(ctx, os.Args[2:])
	case "models":
		err = cmdModels(ctx, os.Args[2:])
	case "diff":
		err = cmdDiff(ctx, os.Args[2:])
	case "code-query", "codequery":
		err = cmdCodeQuery(ctx, os.Args[2:])
	case "path":
		err = cmdPath(ctx, os.Args[2:])
	case "explain":
		err = cmdExplain(ctx, os.Args[2:])
	case "annotate":
		err = cmdAnnotate(ctx, os.Args[2:])
	case "overview":
		err = cmdOverview(ctx, os.Args[2:])
	case "mcp":
		err = cmdMCP(ctx, os.Args[2:])
	case "install":
		err = cmdInstall(ctx, os.Args[2:])
	case "init":
		err = cmdInit(ctx, os.Args[2:])
	case "help", "--help", "-h":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `gatt — turn any source's metadata into a semantic graph for AI agents

build the graph:
  gatt init                        scaffold an agent-native .gatt/ config for codebase contextualization
  gatt extract codebase <dir>      parse a repo (tree-sitter) into a code graph; merges the .gatt/ overlay if present
  gatt extract sqlite <db-file> [--out gatt-out/graph.db] [--check]
  gatt extract postgres "postgres://user:pass@host:port/db?sslmode=disable" [--out PATH] [--check]
  gatt extract openapi <spec.json|spec.yaml|http://host:8000/openapi.json> [--out PATH] [--check] [--code REPO_ROOT]
      re-extract reports schema drift vs the existing graph; --check reports it without writing
      --code links endpoints/schemas to their Go source (swaggo): describe_entity shows source: file:line
  gatt enrich <repo-root> [--graph PATH]
      re-link an existing graph to its Go source without re-extracting (the cheap post-code-edit update)
  gatt index  [--graph PATH] [--embed-url URL] [--embed-model NAME] [--qdrant URL] [--full]
      only re-embeds nodes whose text changed since the last index; --full forces a full re-embed
      prints a live "embedding D/T nodes..." progress line to stderr while it runs

query it (graphify-style):
  gatt query "<question>"          context pack: relevant tables, columns, joins
  gatt code-query "<question>"     context pack for code: functions, types, docs, call graph
  gatt search "<text>"             semantic search over all nodes [--type table|column|...]
  gatt impact <function>           transitive callers: what breaks on a signature change [--depth N]
  gatt blast <file-or-function>    blast radius of any node: callers + importers + generated copies [--depth N]
  gatt doc-drift                   docs whose code references broke or went stale (needs git for staleness;
                                      staleness is by commit date, not working-tree mtime)
  gatt grep <pattern>              exhaustive literal search across every file — a proof of absence, not top-N [--regex] [--limit N]
  gatt tree [path]                 directory tree annotated with each file's doc summary [--depth N]
  gatt routes [--file substr]      HTTP routes detected in code (Express JS/TS, Spring, ASP.NET, Go, Flask/FastAPI): method, path, handler,
                                      middleware, ORM models the handler touches, frontend call sites hitting it
  gatt models [--file substr]      ORM models detected in code (Sequelize/TypeORM/gorm/Django/SQLAlchemy +
                                      .gatt/models.json overlay): table, field→column renames, associations
  gatt diff [ref]                  structural diff vs a git ref (default HEAD): added/removed/changed/renamed/moved functions & types, plus current callers [--limit N]
  gatt path <tableA> <tableB>      cheapest FK join path with exact columns
  gatt explain <table|column>      one node in full: attrs + relationships
  gatt overview                    all tables, counts, references

curate it (business knowledge the schema can't express):
  gatt annotate <node> key=value   set entity_note / default_filter (survives re-extract)
  gatt annotate <node> --clear     remove a node's annotations

serve it:
  gatt mcp     [--graph PATH] [--no-semantic] [--qdrant URL] [--source-kind KIND --source SRC]
      --source-kind/--source wire the live source into the check_drift + refresh_graph MCP tools
  gatt install [--graph PATH] [--scope project|user|agy] [--source-kind KIND --source SRC]
      register MCP server in Claude Code or Antigravity (a passed source is stored in the MCP config);
      scope auto-detects between claude/agy when not given, and the gatt binary is added to PATH

all query/serve commands take --graph (default gatt-out/graph.db).
vectors live in-process (vectors.json next to the graph); --qdrant URL opts into Qdrant.
`)
}

func cmdExtract(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: gatt extract sqlite|postgres|openapi|codebase <source> [--out PATH] [--check]")
	}
	kind, source := args[0], args[1]
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	out := fs.String("out", defaultGraph, "output graph path")
	check := fs.Bool("check", false, "re-extract and report drift vs the existing graph without writing it")
	code := fs.String("code", "", "repo root to link endpoints/schemas to their Go source (swaggo)")
	update := fs.Bool("update", false, "codebase only: incremental — re-parse only files changed since last extract")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if *update {
		if kind != "codebase" {
			return fmt.Errorf("--update only supports the codebase connector")
		}
		_, mts, err := graph.LoadCodebaseState(*out)
		if err != nil {
			return fmt.Errorf("--update needs an existing graph at %s — run a full extract first", *out)
		}
		if !codebase.New(source).HasDrift(mts) {
			fmt.Println("no drift; graph unchanged")
			return nil
		}
		raw, err := graph.LoadRaw(*out)
		if err != nil {
			return err
		}
		ng, summary, err := codebase.New(source).Update(ctx, raw)
		if err != nil {
			return err
		}
		if summary == "" {
			fmt.Println("no drift; graph unchanged")
			return nil
		}
		if err := ng.Save(*out); err != nil {
			return err
		}
		fmt.Printf("%s → %s\n", summary, *out)
		return nil
	}
	conn, err := connector.Open(kind, source)
	if err != nil {
		return err
	}
	g, err := conn.Extract(ctx)
	if err != nil {
		return err
	}
	if *code != "" {
		r, err := enrich.Code(g, *code)
		if err != nil {
			return err
		}
		fmt.Printf("linked source: %d endpoints, %d schemas → %s\n", r.Endpoints, r.Schemas, *code)
	}
	g.ExtractedAt = time.Now()

	// Diff against the current graph when one exists: report drift and warn
	// about annotations left pointing at removed nodes. Compare raw graphs so
	// merged annotations don't read as source changes.
	if old, err := graph.LoadRaw(*out); err == nil {
		d := graph.DiffGraphs(old, g)
		printDiff(d, old)
		warnOrphanAnnotations(*out, d.RemovedNodes, g)
		if *check {
			return nil // drift reported; leave the graph untouched
		}
		if d.Empty() {
			// Refresh only the timestamp so freshness reflects this check.
			if err := g.Save(*out); err != nil {
				return err
			}
			fmt.Printf("schema unchanged; refreshed timestamp → %s\n", *out)
			return nil
		}
	} else if *check {
		return fmt.Errorf("no existing graph at %s to check against — run `gatt extract` first", *out)
	}

	if err := g.Save(*out); err != nil {
		return err
	}
	counts := map[string]int{}
	for _, n := range g.Nodes {
		counts[n.Type]++
	}
	fmt.Printf("extracted %s → %s\n", source, *out)
	for t, c := range counts {
		fmt.Printf("  %-10s %d\n", t, c)
	}
	fmt.Printf("  %-10s %d\n", "edges", len(g.Edges))
	return nil
}

// cmdEnrich re-links an existing graph to its Go source without re-extracting
// the spec — the cheap "update" an agent runs after editing code. Operates on
// the raw graph so curated annotations stay in their sidecar.
func cmdEnrich(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gatt enrich <repo-root> [--graph PATH]")
	}
	root := args[0]
	fs := flag.NewFlagSet("enrich", flag.ExitOnError)
	graphPath := fs.String("graph", defaultGraph, "graph file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	g, err := graph.LoadRaw(*graphPath)
	if err != nil {
		return err
	}
	r, err := enrich.Code(g, root)
	if err != nil {
		return err
	}
	if err := g.Save(*graphPath); err != nil {
		return err
	}
	fmt.Printf("linked source: %d endpoints, %d schemas → %s\n", r.Endpoints, r.Schemas, *graphPath)
	return nil
}

// printDiff prints a compact human summary of graph drift. Silent when the
// graph is unchanged so `--check` on a fresh graph stays quiet.
func printDiff(d *graph.Diff, old *graph.Graph) {
	if d.Empty() {
		if !old.ExtractedAt.IsZero() {
			fmt.Printf("no schema drift since %s\n", old.ExtractedAt.Format("2006-01-02 15:04"))
		} else {
			fmt.Println("no schema drift")
		}
		return
	}
	fmt.Print(d.Text())
}

// warnOrphanAnnotations flags curated annotations whose target node no longer
// exists after re-extraction, so the operator can fix or clear them. A
// function-node annotation that will still apply via ng.ResolveRelinkedFunc
// (its line number shifted but the function survives under the same
// file+name) is not an orphan, so it's skipped.
func warnOrphanAnnotations(graphPath string, removed []string, ng *graph.Graph) {
	if len(removed) == 0 {
		return
	}
	annPath := filepath.Join(filepath.Dir(graphPath), graph.AnnotationsFile)
	ann, err := graph.LoadAnnotations(annPath)
	if err != nil || len(ann) == 0 {
		return
	}
	gone := make(map[string]bool, len(removed))
	for _, id := range removed {
		gone[id] = true
	}
	for id := range ann {
		if gone[id] && ng.ResolveRelinkedFunc(id) == nil {
			fmt.Fprintf(os.Stderr, "warning: annotation for removed node %q will be ignored (gatt annotate %s --clear to remove)\n", id, id)
		}
	}
}

func indexFlags(fs *flag.FlagSet) (graphPath, qdURL, coll, embURL, embModel *string) {
	graphPath = fs.String("graph", defaultGraph, "graph file")
	qdURL = fs.String("qdrant", "", "qdrant url (opt-in; default is the in-process vector index)")
	coll = fs.String("collection", "gatt", "qdrant collection")
	embURL = fs.String("embed-url", embed.DefaultURL, "embedding server url (ollama)")
	embModel = fs.String("embed-model", "", "embedding model (default: model recorded in the index, else "+embed.DefaultModel+")")
	return
}

// resolveModel picks the embedding model: explicit flag > model recorded in
// the local index > default. Prevents silent dimension mismatches between
// index time and query time.
func resolveModel(flagModel string, vs store.VectorStore) string {
	if flagModel != "" {
		return flagModel
	}
	if ls, ok := vs.(*local.Store); ok {
		if m := ls.StoredModel(); m != "" {
			return m
		}
	}
	return embed.DefaultModel
}

// openStore picks the vector backend: Qdrant when --qdrant was given,
// otherwise the in-process index stored next to the graph file.
func openStore(graphPath, qdURL, coll string) store.VectorStore {
	if qdURL != "" {
		return qdrant.New(qdURL, coll)
	}
	return local.New(filepath.Join(filepath.Dir(graphPath), "vectors.json"))
}

// autoRefreshCodebase incrementally re-parses changed files of a codebase
// graph before answering, so line numbers and call edges never go stale
// mid-session, then re-embeds just the changed nodes (best-effort — an
// unreachable embedder never blocks the refresh; hybrid Find covers the gap).
// No-op for non-codebase graphs or when nothing changed.
func autoRefreshCodebase(ctx context.Context, graphPath, qdURL, coll, embURL, embModel string) {
	// Cheap precheck first (stat walk + light metadata read): the common
	// no-drift case never materializes the graph.
	source, mts, err := graph.LoadCodebaseState(graphPath)
	if err != nil || !strings.HasPrefix(source, "codebase:") {
		return
	}
	conn := codebase.New(strings.TrimPrefix(source, "codebase:"))
	if !conn.HasDrift(mts) {
		return
	}
	raw, err := graph.LoadRaw(graphPath)
	if err != nil {
		return
	}
	ng, summary, err := conn.Update(ctx, raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "graph auto-refresh skipped: %v\n", err)
		return
	}
	if summary == "" {
		return
	}
	added := ng.JournalAddedNodeIDs() // read before Save: a SQLite save resets the journal
	if err := ng.Save(graphPath); err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "graph auto-refreshed: %s\n", summary)

	if len(added) > 0 {
		vs := openStore(graphPath, qdURL, coll)
		emb := embed.New(embURL, resolveModel(embModel, vs))
		if n, err := indexer.ReindexNodes(ctx, ng, vs, emb, resolveModel(embModel, vs), added); err == nil && n > 0 {
			fmt.Fprintf(os.Stderr, "re-embedded %d changed node(s)\n", n)
		}
	}
}

func openEngine(graphPath, qdURL, coll, embURL, embModel string) (*engine.Engine, error) {
	g, err := graph.Load(graphPath)
	if err != nil {
		return nil, err
	}
	vs := openStore(graphPath, qdURL, coll)
	e := engine.New(g, vs, embed.New(embURL, resolveModel(embModel, vs)))
	if graph.IsSQLitePath(graphPath) {
		e.FTS = func(q, typ string, limit int) []string {
			return graph.FTSQuery(graphPath, q, typ, limit)
		}
	}
	return e, nil
}

func cmdIndex(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	full := fs.Bool("full", false, "re-embed every node, ignoring the incremental cache")
	if err := fs.Parse(args); err != nil {
		return err
	}
	g, err := graph.Load(*graphPath)
	if err != nil {
		return err
	}
	vs := openStore(*graphPath, *qdURL, *coll)
	model := resolveModel(*embModel, vs)
	emb := embed.New(*embURL, model)

	progress := func(done, total int) {
		fmt.Fprintf(os.Stderr, "\rembedding %d/%d nodes...", done, total)
	}
	res, err := indexer.Reindex(ctx, g, vs, emb, model, *full, progress)
	if err != nil {
		return err
	}
	if res.Embedded > 0 {
		fmt.Fprintln(os.Stderr)
		fmt.Printf("embedded %d nodes with %s (%d reused from cache)\n", res.Embedded, model, res.Reused)
	} else {
		fmt.Printf("all %d nodes already current with %s\n", res.Reused, model)
	}
	backend := "in-process index"
	if *qdURL != "" {
		backend = fmt.Sprintf("qdrant collection %q", *coll)
	}
	fmt.Printf("indexed %d nodes into %s\n", res.Total, backend)
	return nil
}

func cmdSearch(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gatt search <query> [flags]")
	}
	query := args[0]
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	typ := fs.String("type", "", "filter by node type")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	e, err := openEngine(*graphPath, *qdURL, *coll, *embURL, *embModel)
	if err != nil {
		return err
	}
	res, err := e.Find(ctx, query, *typ, 10)
	if err != nil {
		return err
	}
	for _, h := range res.Hits {
		fmt.Printf("%.3f  %-8s %s\n       %s\n", h.Score, h.Type, h.ID, h.Text)
	}
	return nil
}

func cmdQuery(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf(`usage: gatt query "<question>" [flags]`)
	}
	question := args[0]
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	limit := fs.Int("limit", 4, "max tables in context")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	e, err := openEngine(*graphPath, *qdURL, *coll, *embURL, *embModel)
	if err != nil {
		return err
	}
	out, err := e.ContextPack(ctx, question, *limit)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func cmdPath(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: gatt path <tableA> <tableB> [flags]")
	}
	from, to := args[0], args[1]
	fs := flag.NewFlagSet("path", flag.ExitOnError)
	graphPath := fs.String("graph", defaultGraph, "graph file")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	g, err := graph.Load(*graphPath)
	if err != nil {
		return err
	}
	jp := engine.New(g, nil, nil).Join(from, to)
	if !jp.Found {
		fmt.Println(jp.Hint)
		return nil
	}
	// For an API graph the hint is already the readable $ref chain; the SQL
	// per-step column form doesn't apply.
	if !g.IsAPI() {
		for _, st := range jp.Steps {
			fmt.Printf("%s.%s → %s.%s\n", st.FromTable, st.FromColumn, st.ToTable, st.ToColumn)
		}
		fmt.Println()
	}
	fmt.Println(jp.Hint)
	return nil
}

func cmdExplain(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gatt explain <table|column|view|index> [flags]")
	}
	id := args[0]
	fs := flag.NewFlagSet("explain", flag.ExitOnError)
	graphPath := fs.String("graph", defaultGraph, "graph file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	g, err := graph.Load(*graphPath)
	if err != nil {
		return err
	}
	e := engine.New(g, nil, nil)
	d, err := e.Describe(id)
	if err != nil {
		return err
	}
	fmt.Printf("%s (%s)\n", d.Name, d.Type)
	for k, v := range d.Attrs {
		if k != "sql" {
			fmt.Printf("  %s: %s\n", k, v)
		}
	}
	for _, ed := range d.Edges {
		arrow := "→"
		if ed.Dir == "in" {
			arrow = "←"
		}
		fmt.Printf("  %s %-12s %s\n", arrow, ed.Type, ed.Other)
	}
	return nil
}

// cmdAnnotate writes curated per-node overrides to the annotations sidecar
// next to the graph. These carry business knowledge the schema can't express
// (what an entity canonically means, the default filter to apply) so the
// agent writes the right query without asking. Re-running `extract` keeps them.
func cmdAnnotate(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf(`usage: gatt annotate <node> key=value ... | <node> --clear [--graph PATH]
  common keys: entity_note (business definition), default_filter (canonical WHERE)
  example: gatt annotate contacts default_filter="enabled = true" entity_note="CRM contacts; enabled=false is a draft"`)
	}
	node := args[0]
	fs := flag.NewFlagSet("annotate", flag.ExitOnError)
	graphPath := fs.String("graph", defaultGraph, "graph file")
	clear := fs.Bool("clear", false, "remove all annotations for the node")
	// Separate key=value pairs from flags so they can appear in any order. An
	// arg is a pair only if it contains '=' and isn't a flag; everything else
	// (flags and their space-separated values, e.g. --graph PATH) goes to Parse.
	var pairs, flags []string
	for _, a := range args[1:] {
		if !strings.HasPrefix(a, "-") && strings.Contains(a, "=") {
			pairs = append(pairs, a)
		} else {
			flags = append(flags, a)
		}
	}
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if !*clear && len(pairs) == 0 {
		return fmt.Errorf("nothing to do: pass key=value pairs or --clear")
	}

	g, err := graph.Load(*graphPath)
	if err != nil {
		return err
	}
	// Resolve bare names ("contacts") to the canonical node id the merge keys on.
	d, err := engine.New(g, nil, nil).Describe(node)
	if err != nil {
		return err
	}
	id := d.ID

	set := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return fmt.Errorf("bad pair %q: expected key=value", p)
		}
		set[k] = v // empty value removes that single annotation
	}

	annPath := filepath.Join(filepath.Dir(*graphPath), graph.AnnotationsFile)
	result, err := graph.SetAnnotation(annPath, id, set, *clear)
	if err != nil {
		return err
	}
	fmt.Printf("annotated %s → %s\n", id, annPath)
	for k, v := range result {
		fmt.Printf("  %s: %s\n", k, v)
	}
	return nil
}

func cmdOverview(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("overview", flag.ExitOnError)
	graphPath := fs.String("graph", defaultGraph, "graph file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	g, err := graph.Load(*graphPath)
	if err != nil {
		return err
	}
	ov := engine.New(g, nil, nil).Overview()
	if fr := g.Freshness(time.Now()); fr != "" {
		fmt.Printf("# %s\n", fr)
	}
	fmt.Printf("source: %s\n", ov.Source)
	if g.IsAPI() {
		for _, n := range g.NodesByType(graph.NodeAPI) {
			if base := n.Attrs["base_url"]; base != "" {
				fmt.Printf("base url: %s\n", base)
			}
		}
	}
	for t, c := range ov.NodeCounts {
		fmt.Printf("  %-10s %d\n", t, c)
	}
	fmt.Println()
	unit := "cols"
	if g.IsAPI() {
		unit = "props"
	}
	for _, t := range ov.Tables {
		line := fmt.Sprintf("%-40s %3d %s", t.Name, t.Columns, unit)
		if t.RowCount != "" {
			line += fmt.Sprintf("  ~%s rows", t.RowCount)
		}
		if len(t.References) > 0 {
			line += "  → " + strings.Join(t.References, ", ")
		}
		fmt.Println(line)
	}
	for _, ep := range ov.Endpoints {
		line := ep.Name
		if len(ep.Schemas) > 0 {
			line += "  → " + strings.Join(ep.Schemas, ", ")
		}
		fmt.Println(line)
	}
	return nil
}

func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	noSemantic := fs.Bool("no-semantic", false, "disable semantic search, keyword only")
	srcKind := fs.String("source-kind", "", "source connector for the check_drift/refresh_graph tools ("+connector.Kinds+")")
	src := fs.String("source", "", "source the connector reads (file/DSN/URL); enables check_drift and refresh_graph")
	code := fs.String("code", "", "repo root to link endpoints/schemas to their Go source on refresh")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*srcKind == "") != (*src == "") {
		return fmt.Errorf("--source-kind and --source must be given together")
	}
	cfg := mcpserver.Config{
		GraphPath:  *graphPath,
		SourceKind: *srcKind,
		Source:     *src,
		CodeRoot:   *code,
	}
	if !*noSemantic {
		// Resolve the store/model up front so the refresh tool re-embeds with
		// the same model; fall back to keyword search if a configured Qdrant is
		// unreachable. OpenStore returns a fresh instance each call so reload
		// picks up newly written vectors.
		vs := openStore(*graphPath, *qdURL, *coll)
		if qd, ok := vs.(*qdrant.Client); ok {
			if err := qd.Ping(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "qdrant unreachable, falling back to keyword search:", err)
				vs = nil
			}
		}
		if vs != nil {
			model := resolveModel(*embModel, vs)
			cfg.OpenStore = func() store.VectorStore { return openStore(*graphPath, *qdURL, *coll) }
			cfg.Embedder = embed.New(*embURL, model)
			cfg.EmbModel = model
		}
	}
	srv, err := mcpserver.New(cfg)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}

// cmdInstall registers gatt as an MCP server for Claude Code (or Antigravity): via the
// `claude` CLI when available, otherwise by merging into ./.mcp.json or mcp_config.json.
func cmdInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	graphPath := fs.String("graph", defaultGraph, "graph file the server will use")
	scope := fs.String("scope", "project", "project (this repo's .mcp.json), user (all projects), or agy (Antigravity CLI)")
	name := fs.String("name", "gatt", "MCP server name")
	srcKind := fs.String("source-kind", "", "source connector to wire in for the check_drift/refresh_graph tools ("+connector.Kinds+")")
	src := fs.String("source", "", "source the connector reads (file/DSN/URL); note a DB DSN is stored in the MCP config")
	code := fs.String("code", "", "repo root to link endpoints/schemas to their Go source on refresh")
	skill := fs.Bool("skill", true, "also install the Claude Code skill into ~/.claude/skills/gatt/")
	addPath := fs.Bool("path", true, "copy the gatt binary to ~/.local/bin and add it to PATH if `gatt` isn't already resolvable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*srcKind == "") != (*src == "") {
		return fmt.Errorf("--source-kind and --source must be given together")
	}

	// --scope wasn't passed explicitly: pick claude when it's on PATH (today's
	// default), otherwise fall back to agy if that's what's actually installed.
	// Without this, a machine with only Antigravity's `agy` CLI (common on a
	// fresh Windows setup where `claude` may not be on PATH yet) would silently
	// fall through to writing a project ./.mcp.json that agy never reads.
	resolvedScope := *scope
	scopeExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "scope" {
			scopeExplicit = true
		}
	})
	if !scopeExplicit {
		if _, err := exec.LookPath("claude"); err != nil {
			if _, err := exec.LookPath("agy"); err == nil {
				resolvedScope = "agy"
			}
		}
	}

	if *skill {
		if err := installSkill(); err != nil {
			// A skill write failure shouldn't abort the MCP registration; the
			// two are independent conveniences.
			fmt.Printf("warning: could not install skill: %v\n", err)
		}
	}

	absGraph, err := filepath.Abs(*graphPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(absGraph); err != nil {
		fmt.Printf("warning: graph %s not found. The agent will need to run refresh_graph to create it.\n", absGraph)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	if strings.Contains(exe, "go-build") {
		return fmt.Errorf("running via `go run`; build a stable binary first: go build -o ~/.local/bin/gatt ./cmd/gatt")
	}

	if *addPath {
		if newExe, err := ensureBinaryOnPath(exe); err != nil {
			fmt.Printf("warning: could not add gatt to PATH: %v\n", err)
		} else {
			exe = newExe
		}
	}

	// The args the registered server launches with; a configured source enables
	// the drift/refresh tools.
	mcpArgs := []string{"mcp", "--graph", absGraph}
	if *srcKind != "" {
		mcpArgs = append(mcpArgs, "--source-kind", *srcKind, "--source", *src)
	}
	if *code != "" {
		absCode, err := filepath.Abs(*code)
		if err != nil {
			return err
		}
		mcpArgs = append(mcpArgs, "--code", absCode)
	}

	if resolvedScope == "agy" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("could not find home dir: %w", err)
		}
		// Confirmed against the `agy` binary's own embedded docs ("Global
		// Configuration: ~/.gemini/config/mcp_config.json (applies to all
		// sessions)") — antigravity-cli/mcp_config.json, used here previously,
		// is never read by the CLI.
		configPath := filepath.Join(home, ".gemini", "config", "mcp_config.json")
		return updateMCPConfig(configPath, *name, exe, mcpArgs)
	}

	if claudePath, err := exec.LookPath("claude"); err == nil {
		claudeArgs := append([]string{"mcp", "add", "--scope", resolvedScope, *name, "--", exe}, mcpArgs...)
		// On Windows a PATH-installed `claude` usually resolves to the
		// claude.cmd shim npm leaves behind; CreateProcess can't launch a
		// .cmd/.bat directly, only cmd.exe knows how to interpret one.
		execName, execArgs := wrapForShellExec(claudePath, claudeArgs)
		cmd := exec.CommandContext(ctx, execName, execArgs...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("claude mcp add failed: %w", err)
		}
		fmt.Printf("registered MCP server %q (scope %s) → %s %s\n", *name, resolvedScope, exe, strings.Join(mcpArgs, " "))
		return nil
	}

	// no claude CLI: merge into ./.mcp.json
	if resolvedScope != "project" {
		return fmt.Errorf("claude CLI not found; only --scope project or agy supported")
	}
	return updateMCPConfig(".mcp.json", *name, exe, mcpArgs)
}

// ensureBinaryOnPath copies exe to ~/.local/bin (creating it if needed) and
// adds that directory to the user's PATH, unless `gatt` already resolves via
// PATH to this same binary. Returns the path the caller should register as
// the MCP server's command — the copy's path when one was made, exe
// unchanged otherwise. Platform-specific PATH mutation (shell rc file on
// Unix, HKCU\Environment on Windows) lives in path_unix.go / path_windows.go.
func ensureBinaryOnPath(exe string) (string, error) {
	if p, err := exec.LookPath(binaryName()); err == nil {
		if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved == exe {
			return exe, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return exe, err
	}
	binDir := filepath.Join(home, ".local", "bin")
	target := filepath.Join(binDir, binaryName())
	if target != exe {
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			return exe, fmt.Errorf("create %s: %w", binDir, err)
		}
		data, err := os.ReadFile(exe)
		if err != nil {
			return exe, fmt.Errorf("read %s: %w", exe, err)
		}
		if err := os.WriteFile(target, data, 0o755); err != nil {
			return exe, fmt.Errorf("write %s: %w", target, err)
		}
		fmt.Printf("copied binary → %s\n", target)
		exe = target
	}
	if err := addToUserPath(binDir); err != nil {
		return exe, fmt.Errorf("add %s to PATH: %w", binDir, err)
	}
	fmt.Printf("%s is on PATH (open a new shell for it to take effect)\n", binDir)
	return exe, nil
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "gatt.exe"
	}
	return "gatt"
}

func updateMCPConfig(configPath, name, exe string, mcpArgs []string) error {
	cfg := map[string]any{}
	if data, err := os.ReadFile(configPath); err == nil {
		// Antigravity/agy leaves this file zero-length as a placeholder before
		// anything's ever registered — treat that the same as "doesn't exist
		// yet" rather than failing on the empty-input JSON parse error.
		if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 {
			if err := json.Unmarshal(trimmed, &cfg); err != nil {
				return fmt.Errorf("%s exists but is invalid JSON: %w", configPath, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if dir := filepath.Dir(configPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers[name] = map[string]any{"command": exe, "args": mcpArgs}
	cfg["mcpServers"] = servers
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s: server %q → %s %s\n", filepath.Base(configPath), name, exe, strings.Join(mcpArgs, " "))
	return nil
}

// installSkill writes the embedded Claude Code skill to
// ~/.claude/skills/gatt/SKILL.md, overwriting any prior copy so the skill
// always matches this binary's commands. It's a user-scoped convenience run
// from cmdInstall; failures are non-fatal to the MCP registration.
func installSkill() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("could not find home dir: %w", err)
	}
	dir := filepath.Join(home, ".claude", "skills", "gatt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(skillMD), 0o644); err != nil {
		return err
	}
	fmt.Printf("installed skill → %s\n", path)
	return nil
}

func cmdInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := ".gatt"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		"gatt.spec.json": "{\n  \"version\": \"1.0\",\n  \"project\": \"\",\n  \"namespaces\": [],\n  \"manifests\": {\n    \"definitions\": \"./definitions.json\",\n    \"relations\": \"./relations.json\",\n    \"contracts\": \"./contracts.json\"\n  }\n}",
		"definitions.json": "{\n  \"entities\": {}\n}",
		"relations.json": "{\n  \"features\": []\n}",
		"contracts.json": "{\n  \"api\": {},\n  \"database\": {}\n}",
		"prompt.md": `# GATT Initialization Directive

You have been invoked to initialize the .gatt/ semantic graph for this repository. 
Your goal is to populate gatt.spec.json, definitions.json, and relations.json with the core architectural domains of this codebase.

**CRITICAL RULES FOR THIS INSPECTION:**
1. **DO NOT rely solely on documentation.** Documentation (like ARCHITECTURE.md) is often obsolete. You MUST inspect the ACTUAL source code, directory structures, and core interfaces to discover the real domains.
2. **This is a Long-Run Inspection.** Take your time. Traverse the main entry points, understand the data flow, and identify the isolated subprojects. You are looking at a forest with many trees; find how the branches intersect.
3. **Map the Semantic Domains:** In definitions.json, define the high-level business domains and document their *critical rules* based on how the code actually behaves.
4. **Map the Relations:** In relations.json, link these semantic domains to their physical entry points and declare their dependencies.
5. **Execute:** Run a deep exploration of the codebase now. When finished, write the final JSONs.
`,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return err
			}
			fmt.Printf("created %s\n", path)
		} else {
			fmt.Printf("skipped %s (already exists)\n", path)
		}
	}
	fmt.Println("\nInitialized .gatt/ workspace.")
	fmt.Println("👉 Pass .gatt/prompt.md to your AI agent to begin the semantic extraction.")
	return nil
}

// cmdCodeQuery is the codebase analogue of cmdQuery: it renders the most
// relevant functions, types, and docs for a natural-language question against
// a codebase graph.
// cmdImpact prints the transitive callers of a function — what breaks if its
// signature changes.
func cmdImpact(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gatt impact <function-name-or-id> [--depth N]")
	}
	target := args[0]
	fs := flag.NewFlagSet("impact", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	depth := fs.Int("depth", 3, "how many caller levels to walk")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	autoRefreshCodebase(ctx, *graphPath, *qdURL, *coll, *embURL, *embModel)
	e, err := openEngine(*graphPath, *qdURL, *coll, *embURL, *embModel)
	if err != nil {
		return err
	}
	out, err := e.Impact(target, *depth)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// cmdBlast prints the blast radius of modifying any node — file, function or
// type: transitive callers, importers, regenerated outputs and diverged copies.
func cmdBlast(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gatt blast <file-path|function-name|node-id> [--depth N]")
	}
	target := args[0]
	fs := flag.NewFlagSet("blast", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	depth := fs.Int("depth", 3, "how many dependency levels to walk")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	autoRefreshCodebase(ctx, *graphPath, *qdURL, *coll, *embURL, *embModel)
	e, err := openEngine(*graphPath, *qdURL, *coll, *embURL, *embModel)
	if err != nil {
		return err
	}
	out, err := e.Blast(target, *depth)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// cmdDocDrift prints markdown docs whose code references no longer resolve or
// point at code that changed after the doc's last commit.
func cmdDocDrift(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doc-drift", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	limit := fs.Int("limit", 15, "max docs to report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	autoRefreshCodebase(ctx, *graphPath, *qdURL, *coll, *embURL, *embModel)
	e, err := openEngine(*graphPath, *qdURL, *coll, *embURL, *embModel)
	if err != nil {
		return err
	}
	out, err := e.DocDrift(*limit)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// cmdGrep prints an exhaustive literal/regex match list across every file in
// the codebase root — unlike search/find (semantic, top-N), a zero-result
// answer here is a reliable proof of absence.
func cmdGrep(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gatt grep <pattern> [--regex] [--limit N]")
	}
	pattern := args[0]
	fs := flag.NewFlagSet("grep", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	useRegex := fs.Bool("regex", false, "treat pattern as a case-insensitive regex")
	limit := fs.Int("limit", 50, "max matches to display (the total count is always exact)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	autoRefreshCodebase(ctx, *graphPath, *qdURL, *coll, *embURL, *embModel)
	e, err := openEngine(*graphPath, *qdURL, *coll, *embURL, *embModel)
	if err != nil {
		return err
	}
	out, err := e.Grep(pattern, *useRegex, *limit)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// cmdTree prints a directory tree of the codebase, one file per line,
// annotated with its doc summary — a scan of repo structure without an
// ls+Read round-trip per file.
func cmdTree(ctx context.Context, args []string) error {
	var path string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("tree", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	depth := fs.Int("depth", 0, "max path segments deep to print, 0 = unlimited")
	if err := fs.Parse(args); err != nil {
		return err
	}
	autoRefreshCodebase(ctx, *graphPath, *qdURL, *coll, *embURL, *embModel)
	e, err := openEngine(*graphPath, *qdURL, *coll, *embURL, *embModel)
	if err != nil {
		return err
	}
	out, err := e.Tree(path, *depth)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// cmdRoutes prints every HTTP route detected in the codebase (Express-style
// JS/TS/JSX router/app.METHOD registrations): method, path, handler, and
// middleware chain.
func cmdRoutes(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("routes", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	file := fs.String("file", "", "only routes in files whose path contains this substring")
	if err := fs.Parse(args); err != nil {
		return err
	}
	autoRefreshCodebase(ctx, *graphPath, *qdURL, *coll, *embURL, *embModel)
	e, err := openEngine(*graphPath, *qdURL, *coll, *embURL, *embModel)
	if err != nil {
		return err
	}
	out, err := e.Routes(*file)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// cmdModels prints the ORM models detected in code: name, table, field →
// column renames, and associations — the data layer without a live database.
func cmdModels(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	file := fs.String("file", "", "only models in files whose path contains this substring")
	if err := fs.Parse(args); err != nil {
		return err
	}
	autoRefreshCodebase(ctx, *graphPath, *qdURL, *coll, *embURL, *embModel)
	e, err := openEngine(*graphPath, *qdURL, *coll, *embURL, *embModel)
	if err != nil {
		return err
	}
	out, err := e.Models(*file)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// cmdDiff prints the structural diff between the working tree and a git ref
// (default HEAD): added/removed/changed/renamed/moved functions and types,
// plus the current callers of anything that changed.
func cmdDiff(ctx context.Context, args []string) error {
	var ref string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ref = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	limit := fs.Int("limit", 30, "max changes to display")
	if err := fs.Parse(args); err != nil {
		return err
	}
	autoRefreshCodebase(ctx, *graphPath, *qdURL, *coll, *embURL, *embModel)
	e, err := openEngine(*graphPath, *qdURL, *coll, *embURL, *embModel)
	if err != nil {
		return err
	}
	out, err := e.CodeDiff(ctx, ref, *limit)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func cmdCodeQuery(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf(`usage: gatt code-query "<question>" [flags]`)
	}
	question := args[0]
	fs := flag.NewFlagSet("code-query", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	limit := fs.Int("limit", 6, "max entities in context")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	autoRefreshCodebase(ctx, *graphPath, *qdURL, *coll, *embURL, *embModel)
	e, err := openEngine(*graphPath, *qdURL, *coll, *embURL, *embModel)
	if err != nil {
		return err
	}
	out, err := e.CodeContextPack(ctx, question, *limit)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

