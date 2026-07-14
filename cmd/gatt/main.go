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

	"graphallthethings/internal/connector"
	"graphallthethings/internal/connector/postgres"
	"graphallthethings/internal/connector/sqlite"
	"graphallthethings/internal/embed"
	"graphallthethings/internal/engine"
	"graphallthethings/internal/graph"
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
  gatt extract sqlite <db-file> [--out gatt-out/graph.json]
  gatt extract postgres "postgres://user:pass@host:port/db?sslmode=disable" [--out PATH]
  gatt index  [--graph PATH] [--embed-url URL] [--embed-model NAME] [--qdrant URL]

query it (graphify-style):
  gatt query "<question>"          context pack: relevant tables, columns, joins
  gatt search "<text>"             semantic search over all nodes [--type table|column|...]
  gatt path <tableA> <tableB>      cheapest FK join path with exact columns
  gatt explain <table|column>      one node in full: attrs + relationships
  gatt overview                    all tables, counts, references

serve it:
  gatt mcp     [--graph PATH] [--no-semantic] [--qdrant URL]
  gatt install [--graph PATH] [--scope project|user]   register MCP server in Claude Code

all query/serve commands take --graph (default gatt-out/graph.json).
vectors live in-process (vectors.json next to the graph); --qdrant URL opts into Qdrant.
`)
}

func cmdExtract(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: gatt extract sqlite|postgres <source> [--out PATH]")
	}
	kind, source := args[0], args[1]
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	out := fs.String("out", defaultGraph, "output graph path")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	var conn connector.Connector
	switch kind {
	case "sqlite":
		conn = sqlite.New(source)
	case "postgres":
		conn = postgres.New(source)
	default:
		return fmt.Errorf("unknown connector %q (available: sqlite, postgres)", kind)
	}
	g, err := conn.Extract(ctx)
	if err != nil {
		return err
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	g, err := graph.Load(*graphPath)
	if err != nil {
		return err
	}
	model := *embModel
	if model == "" {
		model = embed.DefaultModel
	}
	emb := embed.New(*embURL, model)
	vs := openStore(*graphPath, *qdURL, *coll)
	if ls, ok := vs.(*local.Store); ok {
		ls.Model = model
	}

	var ids, texts []string
	for id, n := range g.Nodes {
		if n.Type == graph.NodeDatabase {
			continue
		}
		ids = append(ids, id)
		texts = append(texts, g.NodeText(id))
	}
	fmt.Printf("embedding %d nodes with %s...\n", len(ids), model)
	batch := 64
	var points []store.Point
	for i := 0; i < len(ids); i += batch {
		end := min(i+batch, len(ids))
		vecs, err := emb.Embed(ctx, texts[i:end])
		if err != nil {
			return err
		}
		for j, v := range vecs {
			n := g.Nodes[ids[i+j]]
			points = append(points, store.Point{
				NodeID: ids[i+j], Type: n.Type, Name: n.Name, Text: texts[i+j], Vector: v,
			})
		}
	}
	if len(points) == 0 {
		return fmt.Errorf("nothing to index")
	}
	if err := vs.Upsert(ctx, points); err != nil {
		return err
	}
	backend := "in-process index"
	if *qdURL != "" {
		backend = fmt.Sprintf("qdrant collection %q", *coll)
	}
	fmt.Printf("indexed %d nodes into %s\n", len(points), backend)
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
	for _, st := range jp.Steps {
		fmt.Printf("%s.%s → %s.%s\n", st.FromTable, st.FromColumn, st.ToTable, st.ToColumn)
	}
	fmt.Println("\n" + jp.Hint)
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
	fmt.Printf("source: %s\n", ov.Source)
	for t, c := range ov.NodeCounts {
		fmt.Printf("  %-10s %d\n", t, c)
	}
	fmt.Println()
	for _, t := range ov.Tables {
		line := fmt.Sprintf("%-40s %3d cols", t.Name, t.Columns)
		if t.RowCount != "" {
			line += fmt.Sprintf("  ~%s rows", t.RowCount)
		}
		if len(t.References) > 0 {
			line += "  → " + strings.Join(t.References, ", ")
		}
		fmt.Println(line)
	}
	return nil
}

func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	graphPath, qdURL, coll, embURL, embModel := indexFlags(fs)
	noSemantic := fs.Bool("no-semantic", false, "disable semantic search, keyword only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	g, err := graph.Load(*graphPath)
	if err != nil {
		return err
	}
	var vs store.VectorStore
	var emb *embed.Client
	if !*noSemantic {
		vs = openStore(*graphPath, *qdURL, *coll)
		if qd, ok := vs.(*qdrant.Client); ok {
			if err := qd.Ping(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "qdrant unreachable, falling back to keyword search:", err)
				vs = nil
			}
		}
		if vs != nil {
			emb = embed.New(*embURL, resolveModel(*embModel, vs))
		}
	}
	return mcpserver.New(engine.New(g, vs, emb)).Run(ctx)
}

// cmdInstall registers gatt as an MCP server for Claude Code: via the
// `claude` CLI when available, otherwise by merging into ./.mcp.json.
func cmdInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	graphPath := fs.String("graph", defaultGraph, "graph file the server will use")
	scope := fs.String("scope", "project", "project (this repo's .mcp.json) or user (all projects)")
	name := fs.String("name", "gatt", "MCP server name")
	if err := fs.Parse(args); err != nil {
		return err
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

	if claude, err := exec.LookPath("claude"); err == nil {
		cmd := exec.CommandContext(ctx, claude, "mcp", "add", "--scope", *scope, *name, "--",
			exe, "mcp", "--graph", absGraph)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("claude mcp add failed: %w", err)
		}
		fmt.Printf("registered MCP server %q (scope %s) → %s mcp --graph %s\n", *name, *scope, exe, absGraph)
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
	servers[*name] = map[string]any{"command": exe, "args": []string{"mcp", "--graph", absGraph}}
	cfg["mcpServers"] = servers
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(".mcp.json", data, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote .mcp.json: server %q → %s mcp --graph %s\n", *name, exe, absGraph)
	return nil
}
