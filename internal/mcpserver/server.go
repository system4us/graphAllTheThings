// Package mcpserver exposes the semantic graph to agents over MCP (stdio or
// Streamable HTTP).
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
	"net/http"
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
	"graphallthethings/internal/lock"
	"graphallthethings/internal/store"
)

// Config is everything the server needs to load the graph and, when a source
// is configured, rebuild it. OpenStore/Embedder are nil when semantic search
// is disabled; SourceKind/Source are empty when the server can't re-extract
// (only annotate_entity and reload_graph are then available).
type Config struct {
	GraphPath  string
	SourceKind string // "sqlite" | "postgres" | "openapi" | "codebase"; "" disables drift/refresh
	Source     string // file path, DSN, or URL
	CodeRoot   string // repo root for source-code linking (swaggo); "" disables it
	OpenStore  func() store.VectorStore
	Embedder   *embed.Client
	EmbModel   string
	// OnAutoRefresh, when non-nil, is called after autoRefreshCodebase writes a
	// fresh graph instead of re-embedding inline (used by `gatt serve` so its
	// background coalescer catches the vectors up without slowing tool calls).
	OnAutoRefresh func()
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
	s.server = mcp.NewServer(&mcp.Implementation{Name: "graphallthethings", Version: "0.3.2"}, nil)
	s.register()
	s.registerPromptsAndResources()
	return s, nil
}

func (s *Server) Run(ctx context.Context) error {
	return s.server.Run(ctx, &mcp.StdioTransport{})
}

// MCPServer returns the underlying SDK server, e.g. for custom HTTP routing.
func (s *Server) MCPServer() *mcp.Server {
	return s.server
}

// HTTPHandler returns an http.Handler serving this MCP server over Streamable HTTP.
func (s *Server) HTTPHandler(opts *mcp.StreamableHTTPOptions) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return s.server
	}, opts)
}

// Reload reloads the graph and vector store from disk into the live engine.
func (s *Server) Reload() error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	ne, err := s.build()
	if err != nil {
		return err
	}
	if ne != nil {
		s.setEngine(ne)
	}
	return nil
}

// Engine refreshes a codebase graph if files drifted and returns the live
// engine plus any auto-refresh note. Used by the global multi-repo MCP server.
func (s *Server) Engine(ctx context.Context, autoRefresh bool) (*engine.Engine, string, error) {
	var note string
	if autoRefresh {
		note = s.autoRefreshCodebase(ctx)
	}
	e, err := s.requireEngine()
	return e, note, err
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
	Question  string `json:"question" jsonschema:"the user's natural-language question about the data or API"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max tables/schemas to include, default 4"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"optional approximate token budget (e.g. 1500 for compact, 4000 for deep); 0 = default"`
}

type affectedTestsIn struct {
	Target string `json:"target,omitempty" jsonschema:"file path, symbol name, node id, or 'diff:HEAD' (default when omitted: git working-tree changes vs HEAD)"`
	Depth  int    `json:"depth,omitempty" jsonschema:"dependency hops to walk, default 3"`
}

type impactIn struct {
	Function  string `json:"function" jsonschema:"function name (if unique) or full node id, e.g. func:internal/engine/engine.go:Find:129"`
	Depth     int    `json:"depth,omitempty" jsonschema:"caller levels to walk, default 3"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"optional approximate token budget; 0 = unlimited"`
}

type blastIn struct {
	Target    string `json:"target" jsonschema:"file path (e.g. shared/schemas/product.schema.json), function name, or full node id"`
	Depth     int    `json:"depth,omitempty" jsonschema:"dependency levels to walk, default 3"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"optional approximate token budget; 0 = unlimited"`
}

type tracePathIn struct {
	From     string `json:"from" jsonschema:"source node: route (e.g. 'POST /checkout'), function name, file path, model name, or full node id"`
	To       string `json:"to" jsonschema:"target node: function name, model/table name, file path, route, or full node id"`
	MaxDepth int    `json:"max_depth,omitempty" jsonschema:"max hops to walk, default 8 (max 15)"`
}

type readSymbolsIn struct {
	IDs       []string `json:"ids" jsonschema:"list of function/type/file names or full node ids to read in one batch"`
	MaxTokens int      `json:"max_tokens,omitempty" jsonschema:"approximate token budget, default 4000"`
}

type preflightIn struct {
	Ref       string `json:"ref,omitempty" jsonschema:"git ref to check against, default HEAD"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"optional approximate token budget; 0 = unlimited"`
}

type hotspotsIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"max hotspot files to return, default 20"`
}

type fileHistoryIn struct {
	File  string `json:"file" jsonschema:"repository-relative file path"`
	Limit int    `json:"limit,omitempty" jsonschema:"max recent commits to return, default 8"`
}

type docDriftIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"max docs to report, default 15"`
}

type grepIn struct {
	Pattern string   `json:"pattern" jsonschema:"literal text (default) or regex (with regex=true) to search for"`
	Regex   bool     `json:"regex,omitempty" jsonschema:"treat pattern as a case-insensitive regex instead of a literal substring"`
	Limit   *int     `json:"limit,omitempty" jsonschema:"max matches to display; default 50, 0 = unlimited (the reported total count is always exact)"`
	Include []string `json:"include,omitempty" jsonschema:"optional glob filters to include (e.g. '*.ts', 'src/**')"`
	Exclude []string `json:"exclude,omitempty" jsonschema:"optional glob filters to exclude (e.g. '*_test.go', 'docs/**')"`
	All     bool     `json:"all,omitempty" jsonschema:"include lockfiles that are skipped by default"`
}

type treeIn struct {
	Path  string `json:"path,omitempty" jsonschema:"relative path to scope the tree to, e.g. internal/engine; omit for the whole repo"`
	Depth int    `json:"depth,omitempty" jsonschema:"max path segments deep to print, 0 (default) = unlimited"`
}

type routesIn struct {
	File      string `json:"file,omitempty" jsonschema:"only routes in files whose path contains this substring"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"optional approximate token budget; 0 = unlimited"`
}

type modelsIn struct {
	File string `json:"file,omitempty" jsonschema:"only models in files whose path contains this substring"`
}

type codeDiffIn struct {
	Ref       string `json:"ref,omitempty" jsonschema:"git ref to diff against, default HEAD"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max changes to display, default 30"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"optional approximate token budget; 0 = unlimited"`
}

type AnnotateIn struct {
	Node           string `json:"node" jsonschema:"table/column/schema/function/file name or node id to annotate"`
	EntityNote     string `json:"entity_note,omitempty" jsonschema:"free-text business or architectural note (canonical meaning, design rationale)"`
	DefaultFilter  string `json:"default_filter,omitempty" jsonschema:"a canonical WHERE clause always applied for this DB entity, e.g. 'enabled = true'"`
	Invariant      string `json:"invariant,omitempty" jsonschema:"code invariant that must always hold (surfaced in impact, blast, read_symbols, preflight_check)"`
	Gotcha         string `json:"gotcha,omitempty" jsonschema:"non-obvious pitfall or edge case to remember before editing this node"`
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
		out, err := e.ContextPackWithBudget(ctx, in.Question, in.Limit, in.MaxTokens)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "code_context",
		Description: "PREFERRED FIRST CALL for any code question on a codebase graph. Returns the most relevant functions (with signatures, file:line locations, callers, and callees), types/structs (with all their methods), and relevant docs/files — all in one compact block. Use this before reading source files directly; it tells you exactly what exists and where so you can navigate with precision. Supports optional max_tokens budget.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in contextIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.CodeContextPackWithBudget(ctx, in.Question, in.Limit, in.MaxTokens)
		if err != nil {
			return nil, nil, err
		}
		return text(note + out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "affected_tests",
		Description: "Minimal set of test files, direct test functions, and ready-to-run test commands (go test, vitest, pytest, cargo test) impacted by modifying a file, function, or the current git diff (omit target for working-tree changes vs HEAD). Run after edits to verify changes without running the entire suite.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in affectedTestsIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		res, err := e.AffectedTests(ctx, in.Target, in.Depth)
		if err != nil {
			return nil, nil, err
		}
		return text(note + res.Text), nil, nil
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
		return text(note + engine.TrimToTokenBudget(out, in.MaxTokens)), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "trace_path",
		Description: "Shortest execution/data-flow path between any two codebase nodes (HTTP route, function, event emitter/listener, sqlc query, ORM model, or file). Walks CALLS_ENDPOINT, HANDLED_BY, CALLS (including string-keyed event dispatch with event names), USES_MODEL, sqlc DEPENDS_ON/GENERATES, IMPORTS, and shared state. Use to trace end-to-end flows in one call.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in tracePathIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.TracePath(in.From, in.To, in.MaxDepth)
		if err != nil {
			return nil, nil, err
		}
		return text(note + out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "read_symbols",
		Description: "Batch-read the full source body (with line numbers) and any persisted annotations (entity_note, invariant, gotcha) of 1..N functions, types, or files in a single tool call, bounded by max_tokens (default 4000). Replaces multiple follow-up Read calls after code_context.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in readSymbolsIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.ReadSymbols(in.IDs, in.MaxTokens)
		if err != nil {
			return nil, nil, err
		}
		return text(note + out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "preflight_check",
		Description: "Single composite post-edit verification gate against git diff (default HEAD): risk alerts (edited generated files, touched hotspots, gotchas/invariants), structural code_diff, affected_tests with ready-to-run commands, and docs mentioning changed code. Run before finishing a task.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in preflightIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		out, err := e.PreflightCheck(ctx, in.Ref, in.MaxTokens)
		if err != nil {
			return nil, nil, err
		}
		return text(note + out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "hotspots",
		Description: "Rank codebase files by recent git churn × graph connectivity (degree), surfacing high-risk architectural hubs that change frequently. Useful during planning and code review.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in hotspotsIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		entries, err := e.Hotspots(ctx, limit)
		if err != nil {
			return nil, nil, err
		}
		return text(note + engine.RenderHotspots(entries)), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "file_history",
		Description: "Recent git commit history for a specific file, including extracted issue references (#42, PROJ-123). Use before editing unfamiliar or fragile code to understand why it changed recently.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in fileHistoryIn) (*mcp.CallToolResult, any, error) {
		note := s.autoRefreshCodebase(ctx)
		e, err := s.requireEngine()
		if err != nil {
			return nil, nil, err
		}
		entries := e.FileHistory(ctx, in.File, in.Limit)
		return text(note + engine.RenderFileHistory(in.File, entries)), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "blast",
		Description: "Blast radius of modifying ANY node — file (incl. JSON/YAML/SQL/CSS data files), function or type. Superset of impact: transitive callers, file importers, REFERENCES, outputs regenerated from it (GENERATES; warns if the target is itself generated), same-basename copies [identical]/[diverged], git co-change companions (files that ship in the same commits with no static edge: styles, docs, e2e, i18n), UI files using a stylesheet's selectors, and shared mutable state (JS/TS). A coverage note lists unwired mentions, call-shaped lines first. Call before editing shared config/schema files or widely-imported modules. Codebase graphs only.",
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
		return text(note + engine.TrimToTokenBudget(out, in.MaxTokens)), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "doc_drift",
		Description: "Which docs lie: markdown docs whose inline code references (`funcName`, `path/file.ts`) no longer resolve (broken) or point at code changed after the doc's last commit (stale — by commit date, so an uncommitted doc rewrite still looks stale until committed). `broken` may include prose in backticks (env vars, table names) never meant to resolve. Run before trusting a doc and after refactors. Codebase graphs only; staleness needs git.",
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
		limit := 50
		if in.Limit != nil {
			limit = *in.Limit
		}
		res, err := e.GrepWithOptions(in.Pattern, engine.GrepOptions{
			Regex:            in.Regex,
			Limit:            limit,
			Include:          in.Include,
			Exclude:          in.Exclude,
			IncludeLockfiles: in.All,
		})
		if err != nil {
			return nil, nil, err
		}
		return text(note + res.Text), nil, nil
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
		Description: "Every HTTP route detected in the codebase: method, path, handler (file:line), middleware chain, the ORM models the handler chain touches, and the client call sites hitting it (axios/fetch, requests, net/http, RestTemplate, HttpClient, declared wrappers, template files) matched by method + path tail — client function → route → models in one call. Detects Express-style JS/TS, Spring/ASP.NET annotations, Go gin/chi/echo/net/http/gorilla, Flask/FastAPI decorators, any call passing a whole \"METHOD /path\" literal (custom registration wrappers in any language), and routes tagged via annotate_entity. Codebase graphs only.",
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
		return text(note + engine.TrimToTokenBudget(out, in.MaxTokens)), nil, nil
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
		return text(note + engine.TrimToTokenBudget(out, in.MaxTokens)), nil, nil
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
		Description: "Persist business or architectural knowledge onto a node (table/column/schema/function/file) so every later query carries it. Supports entity_note, default_filter (DB WHERE clause), invariant (code invariant surfaced in impact/blast/read_symbols/preflight_check), gotcha (non-obvious pitfall before editing), and route_method/route_path/route_framework for manual HTTP route tagging. Survives re-extraction and line-number drift. Use clear=true to remove.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in AnnotateIn) (*mcp.CallToolResult, any, error) {
		return s.Annotate(in)
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

func (s *Server) Annotate(in AnnotateIn) (*mcp.CallToolResult, any, error) {
	e, err := s.requireEngine()
	if err != nil {
		return nil, nil, err
	}
	// Resolve a bare name ("contacts" or a function name) to the canonical node id.
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
	if in.Invariant != "" {
		set["invariant"] = in.Invariant
	}
	if in.Gotcha != "" {
		set["gotcha"] = in.Gotcha
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
		return nil, nil, fmt.Errorf("nothing to do: set entity_note/default_filter/invariant/gotcha/route_* or pass clear=true")
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
	release, err := lock.Acquire(s.cfg.GraphPath+".lock", 2*time.Minute)
	if err != nil {
		return fmt.Sprintf("graph auto-refresh skipped: %v\n", err)
	}
	ng, summary, added, err := func() (*graph.Graph, string, []string, error) {
		defer release()
		if _, mts, err := graph.LoadCodebaseState(s.cfg.GraphPath); err != nil || !conn.HasDrift(mts) {
			return nil, "", nil, nil
		}
		raw, err := graph.LoadRaw(s.cfg.GraphPath)
		if err != nil {
			return nil, "", nil, nil
		}
		ng, summary, err := conn.Update(ctx, raw)
		if err != nil || summary == "" {
			return nil, "", nil, err
		}
		added := ng.JournalAddedNodeIDs() // read before Save: a SQLite save resets the journal
		if err := ng.Save(s.cfg.GraphPath); err != nil {
			return nil, "", nil, err
		}
		return ng, summary, added, nil
	}()
	if err != nil {
		return fmt.Sprintf("graph auto-refresh skipped: %v\n", err)
	}
	if summary == "" {
		return ""
	}
	if ne, err := s.build(); err == nil && ne != nil {
		s.setEngine(ne)
	}
	if s.cfg.OnAutoRefresh != nil {
		s.cfg.OnAutoRefresh()
	} else if len(added) > 0 && s.cfg.OpenStore != nil && s.cfg.Embedder != nil {
		// Re-embed just the changed nodes, best-effort: an unreachable embedder
		// never blocks the refresh (hybrid Find covers the gap until it's back).
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
