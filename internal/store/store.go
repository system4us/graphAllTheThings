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
	Hash   string // optional: hash of Text, stored so a later pass can skip unchanged nodes
	Vector []float32
}

type Hit struct {
	NodeID string
	Score  float32
	Type   string
	Name   string
}

type VectorStore interface {
	// Upsert adds or updates the given points and leaves every other point alone.
	Upsert(ctx context.Context, points []Point) error
	// Search returns the nodes closest to the vector, optionally filtered
	// by node type ("table", "column", ...).
	Search(ctx context.Context, vector []float32, limit int, nodeType string) ([]Hit, error)
}

// Incremental is implemented by stores that can report what they already hold,
// so an index pass uploads only the nodes whose text changed and removes the
// ones that left the graph, without ever holding the vectors in memory.
type Incremental interface {
	// Hashes returns node id -> hash of the text the stored vector was made from.
	Hashes(ctx context.Context) (map[string]string, error)
	// Delete removes the given nodes from the index.
	Delete(ctx context.Context, nodeIDs []string) error
}
