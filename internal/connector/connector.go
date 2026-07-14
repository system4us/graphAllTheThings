// Package connector defines the interface every metadata source implements.
// A connector inspects a source (database, API spec, ...) once and returns
// a semantic graph; it is never used at query time.
package connector

import (
	"context"
	"fmt"

	"graphallthethings/internal/connector/openapi"
	"graphallthethings/internal/connector/postgres"
	"graphallthethings/internal/connector/sqlite"
	"graphallthethings/internal/graph"
)

type Connector interface {
	// Name identifies the connector kind, e.g. "sqlite", "postgres", "openapi".
	Name() string
	// Extract collects metadata from the source and builds the graph.
	Extract(ctx context.Context) (*graph.Graph, error)
}

// Kinds lists the available connector kinds, for usage and error messages.
const Kinds = "sqlite, postgres, openapi"

// Open builds the connector for a source kind ("sqlite", "postgres",
// "openapi") pointed at source (a file path, DSN, or URL). Shared by the CLI's
// `extract` and the MCP server's refresh/drift tools so both resolve a source
// the same way.
func Open(kind, source string) (Connector, error) {
	switch kind {
	case "sqlite":
		return sqlite.New(source), nil
	case "postgres":
		return postgres.New(source), nil
	case "openapi":
		return openapi.New(source), nil
	default:
		return nil, fmt.Errorf("unknown connector %q (available: %s)", kind, Kinds)
	}
}
