// Package mcpserver exposes the semantic graph to agents over MCP (stdio).
// Tools answer schema questions from the pre-built graph so the agent never
// has to introspect the live source at question time. All logic lives in
// internal/engine; this is protocol plumbing only.
package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"graphallthethings/internal/engine"
)

type Server struct {
	e      *engine.Engine
	server *mcp.Server
}

func New(e *engine.Engine) *Server {
	s := &Server{e: e}
	s.server = mcp.NewServer(&mcp.Implementation{Name: "graphallthethings", Version: "0.1.0"}, nil)
	s.register()
	return s
}

func (s *Server) Run(ctx context.Context) error {
	return s.server.Run(ctx, &mcp.StdioTransport{})
}

type emptyIn struct{}

type findIn struct {
	Query string `json:"query" jsonschema:"natural-language description of what to find"`
	Type  string `json:"type,omitempty" jsonschema:"optional node type filter: table, column, view, index"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results, default 8"`
}

type describeIn struct {
	ID string `json:"id" jsonschema:"node id or bare name, e.g. table:users, users, users.email"`
}

type joinIn struct {
	From string `json:"from" jsonschema:"source table name"`
	To   string `json:"to" jsonschema:"target table name"`
}

type contextIn struct {
	Question string `json:"question" jsonschema:"the user's natural-language question about the data"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max tables to include, default 4"`
}

func (s *Server) register() {
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "graph_overview",
		Description: "Summary of the metadata graph: source, node counts, all tables with column counts and references. Call this first to orient yourself.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, engine.Overview, error) {
		return nil, s.e.Overview(), nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "find_entities",
		Description: "Find tables/columns/views semantically related to a natural-language query (e.g. 'user login timestamps'). Use before writing SQL to locate the right entities.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in findIn) (*mcp.CallToolResult, engine.FindResult, error) {
		out, err := s.e.Find(ctx, in.Query, in.Type, in.Limit)
		return nil, out, err
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "describe_entity",
		Description: "Full detail of one node: attributes (types, enums, row counts, comments, DDL) and all relationships (columns, foreign keys, indexes).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in describeIn) (*mcp.CallToolResult, engine.Description, error) {
		out, err := s.e.Describe(in.ID)
		return nil, out, err
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "join_path",
		Description: "Cheapest foreign-key join path between two tables, with the exact columns to join on. Avoids hub tables (tenant-style FKs) that produce semantically wrong joins. Use to build multi-table SQL without guessing.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in joinIn) (*mcp.CallToolResult, engine.JoinPath, error) {
		return nil, s.e.Join(in.From, in.To), nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "sql_context",
		Description: "One-shot context pack for answering a data question: the most relevant tables fully described (columns, types, enums, FKs). Feed this straight into SQL generation.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in contextIn) (*mcp.CallToolResult, engine.Context, error) {
		out, err := s.e.QuestionContext(ctx, in.Question, in.Limit)
		return nil, out, err
	})
}
