// Package connector defines the interface every metadata source implements.
// A connector inspects a source (database, API spec, ...) once and returns
// a semantic graph; it is never used at query time.
package connector

import (
	"context"

	"graphallthethings/internal/graph"
)

type Connector interface {
	// Name identifies the connector kind, e.g. "sqlite", "postgres", "openapi".
	Name() string
	// Extract collects metadata from the source and builds the graph.
	Extract(ctx context.Context) (*graph.Graph, error)
}
