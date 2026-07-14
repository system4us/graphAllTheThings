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
		return nil, err
	}
	var vs store.VectorStore
	if s.cfg.OpenStore != nil {
		vs = s.cfg.OpenStore()
	}
	return engine.New(g, vs, s.cfg.Embedder), nil
}

func (s *Server) engine() *engine.Engine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.e
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

type annotateIn struct {
	Node          string `json:"node" jsonschema:"table/column/schema name or node id to annotate"`
	EntityNote    string `json:"entity_note,omitempty" jsonschema:"free-text business definition of the entity (what it canonically means, edge cases)"`
	DefaultFilter string `json:"default_filter,omitempty" jsonschema:"a canonical WHERE clause always applied for this entity, e.g. 'enabled = true'"`
	Clear         bool   `json:"clear,omitempty" jsonschema:"remove all annotations from the node instead of setting them"`
}

func (s *Server) register() {
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "sql_context",
		Description: "PREFERRED FIRST CALL for any data question. One compact block with the SQL dialect, the relevant tables (columns, types, enums, FKs, soft-delete flags, and curated business notes / default filters) AND the join conditions between them. For an API-spec graph it returns the relevant schemas and endpoints with their $ref relationships instead. Trust the default filter and note lines: they define what the entity means, so apply them instead of guessing or asking. Usually the only call you need before writing SQL.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in contextIn) (*mcp.CallToolResult, any, error) {
		out, err := s.engine().ContextPack(ctx, in.Question, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "find_entities",
		Description: "Semantic search over the graph (tables/columns/views, or API schemas/properties/endpoints) when sql_context missed something specific. One line per hit.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in findIn) (*mcp.CallToolResult, any, error) {
		res, err := s.engine().Find(ctx, in.Query, in.Type, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return text(s.engine().RenderFind(res)), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "describe_entity",
		Description: "One entity in full compact form: for a database, a table/view with columns, types, enums, FKs; for an API spec, a schema (properties, $ref targets) or an endpoint (params, request/response bodies). Use when sql_context didn't include something you need.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in describeIn) (*mcp.CallToolResult, any, error) {
		out, err := s.engine().Render(in.ID)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "join_path",
		Description: "Foreign-key join chain between two tables as a ready JOIN clause (or, for an API spec, the $ref chain between two schemas). Only needed when the entities were not both in sql_context output (its joins section already covers those).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in joinIn) (*mcp.CallToolResult, any, error) {
		e := s.engine()
		return text(e.RenderJoin(e.Join(in.From, in.To))), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "graph_overview",
		Description: "Every table (or API schema) with member count and references, plus API endpoints, one per line. Only for exploring the whole graph; for a specific question use sql_context.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
		return text(s.engine().RenderOverview()), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "annotate_entity",
		Description: "Persist business knowledge you've learned onto a table/column/schema so every later sql_context and describe_entity carries it. entity_note is a free-text definition; default_filter is a canonical WHERE clause (e.g. 'enabled = true') that gets rendered like a soft-delete filter. Survives re-extraction. Use this instead of re-explaining the same caveat each session; use clear=true to remove.",
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

	// Drift/refresh need the live source; only offered when one is configured.
	if s.cfg.SourceKind != "" {
		mcp.AddTool(s.server, &mcp.Tool{
			Name:        "check_drift",
			Description: "Re-read the live source and report how the current graph has drifted from it (added/removed/changed tables & columns, FK deltas) WITHOUT modifying anything. Use to judge whether the snapshot is stale before trusting it, or to decide if refresh_graph is warranted.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
			out, err := s.checkDrift(ctx)
			if err != nil {
				return nil, nil, err
			}
			return text(out), nil, nil
		})

		mcp.AddTool(s.server, &mcp.Tool{
			Name:        "refresh_graph",
			Description: "Re-extract the graph from the live source, re-embed the changed nodes, and reload — bringing the snapshot up to date. WRITES to disk. Reports what changed. Use after check_drift shows meaningful drift.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
			out, err := s.refresh(ctx)
			if err != nil {
				return nil, nil, err
			}
			return text(out), nil, nil
		})
	}
}

func (s *Server) annotate(in annotateIn) (*mcp.CallToolResult, any, error) {
	// Resolve a bare name ("contacts") to the canonical node id the merge keys on.
	d, err := s.engine().Describe(in.Node)
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

func (s *Server) checkDrift(ctx context.Context) (string, error) {
	conn, err := connector.Open(s.cfg.SourceKind, s.cfg.Source)
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

func (s *Server) refresh(ctx context.Context) (string, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	conn, err := connector.Open(s.cfg.SourceKind, s.cfg.Source)
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
	fmt.Fprintf(&b, "refreshed %s → %s\n", s.cfg.Source, s.cfg.GraphPath)
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
		res, err := indexer.Reindex(ctx, g, s.cfg.OpenStore(), s.cfg.Embedder, s.cfg.EmbModel, false)
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
