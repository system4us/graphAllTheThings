// Package store defines the vector index over graph nodes. The graph itself
// (structure, traversal) always lives in graph.json; a VectorStore only
// answers "which nodes are semantically close to this vector".
package store

import "context"

type Point struct {
	NodeID string
	Type   string
	Name   string
	Text   string
	Vector []float32
}

type Hit struct {
	NodeID string
	Score  float32
	Type   string
	Name   string
}

type VectorStore interface {
	// Upsert replaces the index contents with the given points.
	Upsert(ctx context.Context, points []Point) error
	// Search returns the nodes closest to the vector, optionally filtered
	// by node type ("table", "column", ...).
	Search(ctx context.Context, vector []float32, limit int, nodeType string) ([]Hit, error)
}
