// Package mcpserver exposes the semantic graph to agents over MCP (stdio).
// Tools answer schema questions from the pre-built graph so the agent never
// has to introspect the live source at question time. Responses are compact
// annotated text, not structured JSON — same information, far fewer tokens.
// All logic lives in internal/engine; this is protocol plumbing only.
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

func text(t string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: t}}}
}

type emptyIn struct{}

type findIn struct {
	Query string `json:"query" jsonschema:"natural-language description of what to find"`
	Type  string `json:"type,omitempty" jsonschema:"optional node type filter: table, column, view, index"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results, default 8"`
}

type describeIn struct {
	ID string `json:"id" jsonschema:"table/column name or node id, e.g. users, sales.status, table:public.users"`
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
		Name:        "sql_context",
		Description: "PREFERRED FIRST CALL for any data question. One compact block with the relevant tables (columns, types, enums, FKs, soft-delete flags) AND the join conditions between them. Usually the only call you need before writing SQL.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in contextIn) (*mcp.CallToolResult, any, error) {
		out, err := s.e.ContextPack(ctx, in.Question, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "find_entities",
		Description: "Semantic search over tables/columns/views when sql_context missed something specific. One line per hit.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in findIn) (*mcp.CallToolResult, any, error) {
		res, err := s.e.Find(ctx, in.Query, in.Type, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return text(s.e.RenderFind(res)), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "describe_entity",
		Description: "One table/view in full compact form: columns, types, enums, FKs in and out. Use when sql_context didn't include a table you need.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in describeIn) (*mcp.CallToolResult, any, error) {
		out, err := s.e.RenderTable(in.ID)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "join_path",
		Description: "Foreign-key join chain between two tables as a ready JOIN clause. Only needed when the tables were not both in sql_context output (its ## joins section already covers those).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in joinIn) (*mcp.CallToolResult, any, error) {
		return text(s.e.RenderJoin(s.e.Join(in.From, in.To))), nil, nil
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "graph_overview",
		Description: "Every table with column count and references, one per line. Only for exploring the whole schema; for a specific question use sql_context.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
		return text(s.e.RenderOverview()), nil, nil
	})
}
