// Package mcpserver exposes the semantic graph to agents over MCP (stdio).
// Read tools answer schema questions from the pre-built graph so the agent
// never has to introspect the live source at question time. Maintenance tools
// let the agent curate business knowledge and, when the server was started
// with a source, detect drift and refresh the graph itself. Responses are
// compact annotated text, not structured JSON — same information, far fewer
// tokens. All query logic lives in internal/engine; this is protocol plumbing.
package mcpserver

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"graphallthethings/internal/connector"
	"graphallthethings/internal/connector/codebase"
	"graphallthethings/internal/embed"
	"graphallthethings/internal/engine"
	"graphallthethings/internal/enrich"
	"graphallthethings/internal/graph"
	"graphallthethings/internal/indexer"
	"graphallthethings/internal/store"
)

// Config is everything the server needs to load the graph and, when a source
// is configured, rebuild it. OpenStore/Embedder are nil when semantic search
// is disabled; SourceKind/Source are empty when the server can't re-extract
// (only annotate_entity and reload_graph are then available).
type Config struct {
	GraphPath  string
	SourceKind string // "sqlite" | "postgres" | "openapi"; "" disables drift/refresh
	Source     string // file path, DSN, or URL
	CodeRoot   string // repo root for source-code linking (swaggo); "" disables it
	OpenStore  func() store.VectorStore
	Embedder   *embed.Client
	EmbModel   string
}

type Server struct {
	cfg       Config
	server    *mcp.Server
	mu        sync.RWMutex // guards e
	e         *engine.Engine
	refreshMu sync.Mutex // serializes refresh/reload rebuilds
}

// New loads the initial graph and wires the tools. It errors if the graph
// can't be loaded.
func New(cfg Config) (*Server, error) {
	s := &Server{cfg: cfg}
	e, err := s.build()
	if err != nil {
		return nil, err
	}
	s.e = e
	s.server = mcp.NewServer(&mcp.Implementation{Name: "graphallthethings", Version: "0.1.0"}, nil)
	s.register()
	return s, nil
}

func (s *Server) Run(ctx context.Context) error {
	return s.server.Run(ctx, &mcp.StdioTransport{})
}

// build loads the graph (with annotations) and opens a fresh vector store, so
// refresh/reload pick up new vectors written since startup.
func (s *Server) build() (*engine.Engine, error) {
	g, err := graph.Load(s.cfg.GraphPath)
	if err != nil {
		// Start with a nil engine; agent must call refresh_graph first.
		return nil, nil
	}
	var vs store.VectorStore
	if s.cfg.OpenStore != nil {
		vs = s.cfg.OpenStore()
	}
	e := engine.New(g, vs, s.cfg.Embedder)
	if graph.IsSQLitePath(s.cfg.GraphPath) {
		gp := s.cfg.GraphPath
		e.FTS = func(q, typ string, limit int) []string {
			return graph.FTSQuery(gp, q, typ, limit)
		}
	}
	return e, nil
}

func (s *Server) requireEngine() (*engine.Engine, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.e == nil {
		return nil, fmt.Errorf("graph not extracted yet; use refresh_graph to extract it from the live source")
	}
	return s.e, nil
}

func (s *Server) setEngine(e *engine.Engine) {
	s.mu.Lock()
	s.e = e
	s.mu.Unlock()
}

func text(t string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: t}}}
}

type emptyIn struct{}

type sourceIn struct {
	SourceKind string `json:"source_kind,omitempty" jsonschema:"optional: connector kind (sqlite, postgres, openapi). Required if the server wasn't started with one."`
	Source     string `json:"source,omitempty" jsonschema:"optional: file path, DSN, or URL. Required if the server wasn't started with one."`
}

type findIn struct {
	Query string `json:"query" jsonschema:"natural-language description of what to find"`
	Type  string `json:"type,omitempty" jsonschema:"optional node type filter: table, column, view, index, schema, property, endpoint"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results, default 8"`
}

type describeIn struct {
	ID string `json:"id" jsonschema:"table/column/schema/endpoint name or node id, e.g. users, sales.status, GET /users/{id}"`
}

type joinIn struct {
	From string `json:"from" jsonschema:"source table or schema name"`
	To   string `json:"to" jsonschema:"target table or schema name"`
}

type contextIn struct {
	Question string `json:"question" jsonschema:"the user's natural-language question about the data or API"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max tables/schemas to include, default 4"`
}

type impactIn struct {
	Function string `json:"function" jsonschema:"function name (if unique) or full node id, e.g. func:internal/engine/engine.go:Find:129"`
	Depth    int    `json:"depth,omitempty" jsonschema:"caller levels to walk, default 3"`
}

type blastIn struct {
	Target string `json:"target" jsonschema:"file path (e.g. shared/schemas/product.schema.json), function name, or full node id"`
	Depth  int    `json:"depth,omitempty" jsonschema:"dependency levels to walk, default 3"`
}

type docDriftIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"max docs to report, default 15"`
}

type grepIn struct {
	Pattern string `json:"pattern" jsonschema:"literal text (default) or regex (with regex=true) to search for"`
	Regex   bool   `json:"regex,omitempty" jsonschema:"treat pattern as a case-insensitive regex instead of a literal substring"`
	Limit   int    `json:"limit,omitempty" jsonschema:"max matches to display, default 50 (the reported total count is always exact)"`
}

type treeIn struct {
	Path  string `json:"path,omitempty" jsonschema:"relative path to scope the tree to, e.g. internal/engine; omit for the whole repo"`
	Depth int    `json:"depth,omitempty" jsonschema:"max path segments deep to print, 0 (default) = unlimited"`
}

type routesIn struct {
	File string `json:"file,omitempty" jsonschema:"only routes in files whose path contains this substring"`
}

type modelsIn struct {
	File string `json:"file,omitempty" jsonschema:"only models in files whose path contains this substring"`
}

type codeDiffIn struct {
	Ref   string `json:"ref,omitempty" jsonschema:"git ref to diff against, default HEAD"`
	Limit int    `json:"limit,omitempty" jsonschema:"max changes to display, default 30"`
}

type annotateIn struct {
	Node           string `json:"node" jsonschema:"table/column/schema name or node id to annotate"`
	EntityNote     string `json:"entity_note,omitempty" jsonschema:"free-text business definition of the entity (what it canonically means, edge cases)"`
	DefaultFilter  string `json:"default_filter,omitempty" jsonschema:"a canonical WHERE clause always applied for this entity, e.g. 'enabled = true'"`
	RouteMethod    string `json:"route_method,omitempty" jsonschema:"HTTP method (GET, POST, ...) — tag a function node as an HTTP route handler the static detector didn't recognize (any language/framework/style)"`
	RoutePath      string `json:"route_path,omitempty" jsonschema:"route path, e.g. '/users/{id}' — pairs with route_method"`
	RouteFramework string `json:"route_framework,omitempty" jsonschema:"web framework the route belongs to, e.g. flask, fastapi, spring, gin — pairs with route_method"`
	Clear          bool   `json:"clear,omitempty" jsonschema:"remove all annotations from the node instead of setting them"`
}

func (s *Server) register() {
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "sql_context",
		Description: "PREFERRED FIRST CALL for any data question. One compact block with the SQL dialect, the relevant tables (columns, types, enums, FKs, soft-delete flags, and curated business notes / default filters) AND the join conditions between them. For an API-spec graph it returns the relevant schemas and endpoints with their $ref relationships instead. Trust the default filter and note lines: they define what the entity means, so apply them instead of guessing or asking. Usually the only call you need before writing SQL.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in contextIn) (*mcp.CallToolResult, any, error) {
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.ContextPack(ctx, in.Question, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "code_context",
		Description: "PREFERRED FIRST CALL for any code question on a codebase graph. Returns the most relevant functions (with signatures, file:line locations, callers, and callees), types/structs (with all their methods), and relevant docs/files — all in one compact block. Use this before reading source files directly; it tells you exactly what exists and where so you can navigate with precision. Works with find_entities and describe_entity for follow-up detail.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in contextIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.CodeContextPack(ctx, in.Question, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return text(note + out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "impact",
		Description: "Transitive callers of a function up to N levels — call BEFORE changing any function signature or behavior to see everything that breaks. Codebase graphs only. Direct callers first, then each depth level, test callers tagged [test]. Also lists any shared mutable state (a singleton property this function reads/writes, e.g. config.session) and the other functions touching it — a data-flow signal CALLS alone can't see (heuristic, JS/TS/JSX only, v1).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in impactIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.Impact(in.Function, in.Depth)
		if err != nil {
			return nil, nil, err
		}
		return text(note + out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "blast",
		Description: "Blast radius of modifying ANY node — a file (including JSON/YAML/SQL/CSS data files), function, or type. Superset of impact: walks transitive callers, file importers, REFERENCES, and forward GENERATES edges (outputs regenerated from the target); warns when the target is itself generated; lists same-basename copies flagged [identical]/[diverged] by content hash; shows git co-change companions — files with no static edge that historically ship in the same commits (stylesheets, docs, e2e tests, i18n); for a stylesheet target, lists the UI files (templates/JSX/other stylesheets) using its selectors — .class via class=/className/classList, #id via id=/getElementById/'#x' queries, [data-*] via attributes/dataset.*, --var via var(--x) (USES_STYLE, repo-defined tokens only); and, for a function target, any shared mutable state it reads/writes and who else touches it (heuristic, JS/TS/JSX only, v1). Call before editing shared config/schema files or any widely-imported module. Codebase graphs only.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in blastIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.Blast(in.Target, in.Depth)
		if err != nil {
			return nil, nil, err
		}
		return text(note + out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "doc_drift",
		Description: "Which documentation lies: markdown docs whose inline code references (`funcName`, `path/file.ts`) either no longer resolve in the graph (broken) or point at code that changed after the doc's last commit (stale). Run before trusting a doc, and after refactors to list docs needing an update. Staleness compares git COMMIT dates, not working-tree mtimes — an uncommitted rewrite of the doc itself still looks stale against committed code until you commit it. `broken` can include non-symbol prose in backticks (table/queue names, env vars, HTTP verbs, external packages) that was never meant to resolve — use judgment. Codebase graphs only; staleness needs git history.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in docDriftIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.DocDrift(in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return text(note + out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "grep",
		Description: "Exhaustive literal (or regex) search across EVERY file in the codebase root, using the same skip rules as extraction (.git, node_modules, vendor, hidden dirs). Unlike find_entities, which is fuzzy/semantic and only ranks top-N nodes already in the graph, this is a full walk: a zero-match answer is a reliable proof that a string does not occur anywhere in the codebase. Use it to confirm absence, or to find literal text find_entities might rank low (raw strings, log messages, config keys). Codebase graphs only.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in grepIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.Grep(in.Pattern, in.Regex, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return text(note + out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "tree",
		Description: "Directory tree of the codebase, one file per line, annotated with each file's doc summary (its leading file/package comment, a markdown doc's title, or its earliest function's doc). The graph has no directory nodes, so this is synthesized from file paths. Use before ls+Read-ing a directory by hand to get oriented on structure. Codebase graphs only.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in treeIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.Tree(in.Path, in.Depth)
		if err != nil {
			return nil, nil, err
		}
		return text(note + out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "routes",
		Description: "Every HTTP route detected in the codebase: method, path, handler (with file:line; controller.method references resolve too), middleware chain, the ORM models the handler chain touches (models: line), and the client call sites that hit the route (called from: line — any parsed language: axios/fetch, Python requests, Go net/http, Java RestTemplate, C# HttpClient, verb-string wrappers, .gatt/clients.json-declared wrappers, plus template files sniffed by content — .vue/.html/.cshtml/.svelte inline <script> blocks, htmx attributes, <form action>; matched by method + path tail, template/format paths normalized). The full-stack intersection in one call: client function → route → models. Statically detects Express-style JS/TS/JSX registrations; Spring/ASP.NET annotations (class prefixes included; Retrofit @GET on Java/Kotlin interfaces counts as a client call); Go registrations (gin/chi/echo, net/http HandleFunc with 1.22 \"GET /x\" patterns — no verb = ANY wildcard, gorilla .Methods chains; statement-context only so resp := http.Get(...) stays a client call); Python Flask/FastAPI decorators (methods=[...], <int:id> params); plus any function tagged via annotate_entity's route_method/route_path/route_framework fields. Codebase graphs only.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in routesIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.Routes(in.File)
		if err != nil {
			return nil, nil, err
		}
		return text(note + out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "models",
		Description: "Every ORM model detected in the codebase: name, DB table, field→column renames (the mapping SQL greps miss), and the association graph (hasMany/belongsTo/hasOne/belongsToMany with as/foreignKey). Statically detects Sequelize-style Model.init / sequelize.define in JS/TS/JSX, associations resolved even when declared in a central setupAssociations file. The data layer without a live database — pair with join-style questions or blast on a model file. Codebase graphs only.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in modelsIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.Models(in.File)
		if err != nil {
			return nil, nil, err
		}
		return text(note + out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "code_diff",
		Description: "Structural diff of the working tree against a git ref (default HEAD): added/removed/changed/renamed/moved functions and types, detected by matching signatures across two extractions — NOT a textual diff. For anything changed/renamed/moved, also lists its current callers so you know who needs to review the change. Use to answer 'what changed structurally since HEAD/a commit' without reasoning through `git log -p` by hand. Codebase graphs only; needs a git checkout.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in codeDiffIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.CodeDiff(ctx, in.Ref, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return text(note + out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "find_entities",
		Description: "Semantic search over the graph (tables/columns/views, or API schemas/properties/endpoints) when sql_context missed something specific. One line per hit.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in findIn) (*mcp.CallToolResult, any, error) {
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		res, err := e.Find(ctx, in.Query, in.Type, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return text(e.RenderFind(res)), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "describe_entity",
		Description: "One entity in full compact form: for a database, a table/view with columns, types, enums, FKs; for an API spec, a schema (properties, $ref targets) or an endpoint (params, request/response bodies). Use when sql_context didn't include something you need.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in describeIn) (*mcp.CallToolResult, any, error) {
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.Render(in.ID)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "join_path",
		Description: "Foreign-key join chain between two tables as a ready JOIN clause (or, for an API spec, the $ref chain between two schemas). Only needed when the entities were not both in sql_context output (its joins section already covers those).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in joinIn) (*mcp.CallToolResult, any, error) {
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		return text(e.RenderJoin(e.Join(in.From, in.To))), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "graph_overview",
		Description: "Every table (or API schema) with member count and references, plus API endpoints, one per line. Only for exploring the whole graph; for a specific question use sql_context.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		return text(e.RenderOverview()), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "annotate_entity",
		Description: "Persist business knowledge you've learned onto a table/column/schema so every later sql_context and describe_entity carries it. entity_note is a free-text definition; default_filter is a canonical WHERE clause (e.g. 'enabled = true') that gets rendered like a soft-delete filter. On a codebase graph, also use route_method/route_path/route_framework to tag any function node as an HTTP route handler the static detector missed — it only recognizes Express-style JS/TS calls, so this is the way to record routes for any other language, framework, or coding style; tagged routes then show up in the routes tool. Survives re-extraction and line-number drift. Use this instead of re-explaining the same caveat each session; use clear=true to remove.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in annotateIn) (*mcp.CallToolResult, any, error) {
		return s.annotate(in)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "reload_graph",
		Description: "Reload the graph and vector index from disk, picking up an extraction or annotation made outside this server. Cheap; no source access.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
		out, err := s.reload()
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "check_drift",
		Description: "Re-read the live source and report how the current graph has drifted from it (added/removed/changed tables & columns, FK deltas) WITHOUT modifying anything. Use to judge whether the snapshot is stale before trusting it, or to decide if refresh_graph is warranted.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in sourceIn) (*mcp.CallToolResult, any, error) {
		out, err := s.checkDrift(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "refresh_graph",
		Description: "Re-extract the graph from the live source, re-embed the changed nodes, and reload — bringing the snapshot up to date. WRITES to disk. Reports what changed. Use after check_drift shows meaningful drift, or to create the initial graph.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in sourceIn) (*mcp.CallToolResult, any, error) {
		out, err := s.refresh(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})
}

func (s *Server) annotate(in annotateIn) (*mcp.CallToolResult, any, error) {
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
	// Resolve a bare name ("contacts") to the canonical node id the merge keys on.
	d, err := e.Describe(in.Node)
	if err != nil {
		return nil, nil, err
	}
	set := map[string]string{}
	if in.EntityNote != "" {
		set["entity_note"] = in.EntityNote
	}
	if in.DefaultFilter != "" {
		set["default_filter"] = in.DefaultFilter
	}
	if in.RouteMethod != "" {
		set["route_method"] = in.RouteMethod
	}
	if in.RoutePath != "" {
		set["route_path"] = in.RoutePath
	}
	if in.RouteFramework != "" {
		set["route_framework"] = in.RouteFramework
	}
	if !in.Clear && len(set) == 0 {
		return nil, nil, fmt.Errorf("nothing to do: set entity_note/default_filter or pass clear=true")
	}
	annPath := filepath.Join(filepath.Dir(s.cfg.GraphPath), graph.AnnotationsFile)
	result, err := graph.SetAnnotation(annPath, d.ID, set, in.Clear)
	if err != nil {
		return nil, nil, err
	}
	// Reload so the annotation is reflected in this session immediately.
	if _, err := s.reload(); err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "annotated %s\n", graph.ShortID(d.ID))
	if len(result) == 0 {
		b.WriteString("(cleared)\n")
	}
	for _, k := range sortedKeys(result) {
		fmt.Fprintf(&b, "  %s: %s\n", k, result[k])
	}
	return text(b.String()), nil, nil
}

// autoRefreshCodebase incrementally re-parses files that changed since the
// last extract, saves the graph, and reloads the engine, so code_context never
// answers from a stale graph (wrong line numbers are worse than no graph).
// Returns a one-line note for the tool output, or "" when nothing changed.
func (s *Server) autoRefreshCodebase(ctx context.Context) string {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	// Cheap precheck first (stat walk + light metadata read): the common
	// no-drift case never materializes the graph.
	source, mts, err := graph.LoadCodebaseState(s.cfg.GraphPath)
	if err != nil || !strings.HasPrefix(source, "codebase:") {
		return ""
	}
	conn := codebase.New(strings.TrimPrefix(source, "codebase:"))
	if !conn.HasDrift(mts) {
		return ""
	}
	raw, err := graph.LoadRaw(s.cfg.GraphPath)
	if err != nil {
		return ""
	}
	ng, summary, err := conn.Update(ctx, raw)
	if err != nil {
		return fmt.Sprintf("graph auto-refresh skipped: %v\n", err)
	}
	if summary == "" {
		return ""
	}
	added := ng.JournalAddedNodeIDs() // read before Save: a SQLite save resets the journal
	if err := ng.Save(s.cfg.GraphPath); err != nil {
		return ""
	}
	if ne, err := s.build(); err == nil {
		s.setEngine(ne)
	}
	// Re-embed just the changed nodes, best-effort: an unreachable embedder
	// never blocks the refresh (hybrid Find covers the gap until it's back).
	if len(added) > 0 && s.cfg.OpenStore != nil && s.cfg.Embedder != nil {
		if n, err := indexer.ReindexNodes(ctx, ng, s.cfg.OpenStore(), s.cfg.Embedder, s.cfg.EmbModel, added); err == nil && n > 0 {
			summary += fmt.Sprintf("; re-embedded %d node(s)", n)
		}
	}
	return "note: graph auto-refreshed (" + summary + ")\n\n"
}

func (s *Server) reload() (string, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	ne, err := s.build()
	if err != nil {
		return "", err
	}
	s.setEngine(ne)
	return "reloaded\n" + statusLine(ne), nil
}

func (s *Server) checkDrift(ctx context.Context, in sourceIn) (string, error) {
	kind, src := in.SourceKind, in.Source
	if kind == "" {
		kind = s.cfg.SourceKind
	}
	if src == "" {
		src = s.cfg.Source
	}
	if kind == "" || src == "" {
		return "", fmt.Errorf("source_kind and source are required (either via tool args or server config)")
	}
	conn, err := connector.Open(kind, src)
	if err != nil {
		return "", err
	}
	ng, err := conn.Extract(ctx)
	if err != nil {
		return "", err
	}
	// Enrich the fresh graph too, so the diff compares like with like (and
	// surfaces handlers that moved even when the contract didn't change).
	if s.cfg.CodeRoot != "" {
		if _, err := enrich.Code(ng, s.cfg.CodeRoot); err != nil {
			return "", err
		}
	}
	old, err := graph.LoadRaw(s.cfg.GraphPath)
	if err != nil {
		return "", fmt.Errorf("no current graph to compare against: %w", err)
	}
	d := graph.DiffGraphs(old, ng)
	if d.Empty() {
		if !old.ExtractedAt.IsZero() {
			return fmt.Sprintf("no schema drift since %s\n", old.ExtractedAt.Format("2006-01-02 15:04")), nil
		}
		return "no schema drift\n", nil
	}
	return d.Text(), nil
}

func (s *Server) refresh(ctx context.Context, in sourceIn) (string, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	kind, src := in.SourceKind, in.Source
	if kind == "" {
		kind = s.cfg.SourceKind
	}
	if src == "" {
		src = s.cfg.Source
	}
	if kind == "" || src == "" {
		return "", fmt.Errorf("source_kind and source are required (either via tool args or server config)")
	}
	conn, err := connector.Open(kind, src)
	if err != nil {
		return "", err
	}
	old, _ := graph.LoadRaw(s.cfg.GraphPath) // may not exist yet
	ng, err := conn.Extract(ctx)
	if err != nil {
		return "", err
	}
	// Recompute the code links on every refresh — the handler may have moved
	// without the contract changing, which a spec-only diff would never see.
	var linked enrich.Result
	if s.cfg.CodeRoot != "" {
		if linked, err = enrich.Code(ng, s.cfg.CodeRoot); err != nil {
			return "", err
		}
	}
	ng.ExtractedAt = time.Now()
	if err := ng.Save(s.cfg.GraphPath); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "refreshed %s → %s\n", src, s.cfg.GraphPath)
	if s.cfg.CodeRoot != "" {
		fmt.Fprintf(&b, "linked source: %d endpoints, %d schemas\n", linked.Endpoints, linked.Schemas)
	}
	if old != nil {
		if d := graph.DiffGraphs(old, ng); d.Empty() {
			b.WriteString("no schema drift\n")
		} else {
			b.WriteString(d.Text())
		}
	}

	if s.cfg.Embedder != nil && s.cfg.OpenStore != nil {
		g, err := graph.Load(s.cfg.GraphPath) // annotated graph, so embeddings include notes
		if err != nil {
			return "", err
		}
		res, err := indexer.Reindex(ctx, g, s.cfg.OpenStore(), s.cfg.Embedder, s.cfg.EmbModel, false, nil)
		if err != nil {
			return "", fmt.Errorf("reindex: %w", err)
		}
		fmt.Fprintf(&b, "reindexed %d nodes (%d embedded, %d reused)\n", res.Total, res.Embedded, res.Reused)
	} else {
		b.WriteString("semantic search disabled; skipped reindex\n")
	}

	ne, err := s.build()
	if err != nil {
		return "", err
	}
	s.setEngine(ne)
	b.WriteString(statusLine(ne))
	return b.String(), nil
}

// statusLine summarizes a freshly built engine: freshness plus node counts.
func statusLine(e *engine.Engine) string {
	ov := e.Overview()
	var parts []string
	for _, k := range sortedKeys(ov.NodeCounts) {
		parts = append(parts, fmt.Sprintf("%s %d", k, ov.NodeCounts[k]))
	}
	line := strings.Join(parts, ", ")
	if fr := e.G.Freshness(time.Now()); fr != "" {
		return fr + "\n" + line + "\n"
	}
	return line + "\n"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
