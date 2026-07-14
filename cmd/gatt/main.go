// gatt — graph all the things. Extract metadata from a source into a
// semantic graph, index it for semantic search, query it from the terminal,
// and serve it to agents over MCP.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"graphallthethings/internal/connector"
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

const defaultGraph = "gatt-out/graph.json"

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
  gatt extract sqlite <db-file> [--out gatt-out/graph.json] [--check]
  gatt extract postgres "postgres://user:pass@host:port/db?sslmode=disable" [--out PATH] [--check]
  gatt extract openapi <spec.json|spec.yaml|http://host:8000/openapi.json> [--out PATH] [--check] [--code REPO_ROOT]
      re-extract reports schema drift vs the existing graph; --check reports it without writing
      --code links endpoints/schemas to their Go source (swaggo): describe_entity shows source: file:line
  gatt enrich <repo-root> [--graph PATH]
      re-link an existing graph to its Go source without re-extracting (the cheap post-code-edit update)
  gatt index  [--graph PATH] [--embed-url URL] [--embed-model NAME] [--qdrant URL] [--full]
      only re-embeds nodes whose text changed since the last index; --full forces a full re-embed

query it (graphify-style):
  gatt query "<question>"          context pack: relevant tables, columns, joins
  gatt search "<text>"             semantic search over all nodes [--type table|column|...]
  gatt path <tableA> <tableB>      cheapest FK join path with exact columns
  gatt explain <table|column>      one node in full: attrs + relationships
  gatt overview                    all tables, counts, references

curate it (business knowledge the schema can't express):
  gatt annotate <node> key=value   set entity_note / default_filter (survives re-extract)
  gatt annotate <node> --clear     remove a node's annotations

serve it:
  gatt mcp     [--graph PATH] [--no-semantic] [--qdrant URL] [--source-kind KIND --source SRC]
      --source-kind/--source wire the live source into the check_drift + refresh_graph MCP tools
  gatt install [--graph PATH] [--scope project|user] [--source-kind KIND --source SRC]
      register MCP server in Claude Code (a passed source is stored in the MCP config)

all query/serve commands take --graph (default gatt-out/graph.json).
vectors live in-process (vectors.json next to the graph); --qdrant URL opts into Qdrant.
`)
}

func cmdExtract(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: gatt extract sqlite|postgres|openapi <source> [--out PATH] [--check]")
	}
	kind, source := args[0], args[1]
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	out := fs.String("out", defaultGraph, "output graph path")
	check := fs.Bool("check", false, "re-extract and report drift vs the existing graph without writing it")
	code := fs.String("code", "", "repo root to link endpoints/schemas to their Go source (swaggo)")
	if err := fs.Parse(args[2:]); err != nil {
		return err
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
		warnOrphanAnnotations(*out, d.RemovedNodes)
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
// exists after re-extraction, so the operator can fix or clear them.
func warnOrphanAnnotations(graphPath string, removed []string) {
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
		if gone[id] {
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

// openEngine loads the graph and wires the vector store + embedder.
func openEngine(graphPath, qdURL, coll, embURL, embModel string) (*engine.Engine, error) {
	g, err := graph.Load(graphPath)
	if err != nil {
		return nil, err
	}
	vs := openStore(graphPath, qdURL, coll)
	return engine.New(g, vs, embed.New(embURL, resolveModel(embModel, vs))), nil
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

	res, err := indexer.Reindex(ctx, g, vs, emb, model, *full)
	if err != nil {
		return err
	}
	if res.Embedded > 0 {
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

// cmdInstall registers gatt as an MCP server for Claude Code: via the
// `claude` CLI when available, otherwise by merging into ./.mcp.json.
func cmdInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	graphPath := fs.String("graph", defaultGraph, "graph file the server will use")
	scope := fs.String("scope", "project", "project (this repo's .mcp.json) or user (all projects)")
	name := fs.String("name", "gatt", "MCP server name")
	srcKind := fs.String("source-kind", "", "source connector to wire in for the check_drift/refresh_graph tools ("+connector.Kinds+")")
	src := fs.String("source", "", "source the connector reads (file/DSN/URL); note a DB DSN is stored in the MCP config")
	code := fs.String("code", "", "repo root to link endpoints/schemas to their Go source on refresh")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*srcKind == "") != (*src == "") {
		return fmt.Errorf("--source-kind and --source must be given together")
	}

	absGraph, err := filepath.Abs(*graphPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(absGraph); err != nil {
		return fmt.Errorf("graph %s not found — run `gatt extract` first", absGraph)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	if strings.Contains(exe, "go-build") {
		return fmt.Errorf("running via `go run`; build a stable binary first: go build -o ~/.local/bin/gatt ./cmd/gatt")
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

	if claude, err := exec.LookPath("claude"); err == nil {
		cmd := exec.CommandContext(ctx, claude, append([]string{"mcp", "add", "--scope", *scope, *name, "--", exe}, mcpArgs...)...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("claude mcp add failed: %w", err)
		}
		fmt.Printf("registered MCP server %q (scope %s) → %s %s\n", *name, *scope, exe, strings.Join(mcpArgs, " "))
		return nil
	}

	// no claude CLI: merge into ./.mcp.json
	if *scope != "project" {
		return fmt.Errorf("claude CLI not found; only --scope project supported (writes ./.mcp.json)")
	}
	cfg := map[string]any{}
	if data, err := os.ReadFile(".mcp.json"); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf(".mcp.json exists but is invalid JSON: %w", err)
		}
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers[*name] = map[string]any{"command": exe, "args": mcpArgs}
	cfg["mcpServers"] = servers
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(".mcp.json", data, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote .mcp.json: server %q → %s %s\n", *name, exe, strings.Join(mcpArgs, " "))
	return nil
}
