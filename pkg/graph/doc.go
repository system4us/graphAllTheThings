// Package graph defines a lightweight, pure-Go semantic graph with typed nodes,
// directed typed edges, lazy adjacency indexing, traversal, snapshot diffing,
// and SQLite (FTS5) + JSON persistence.
//
// This package is MIT-licensed (see ../LICENSE) and requires no CGO
// (CGO_ENABLED=0 builds cleanly via modernc.org/sqlite).
//
// # Data Model and Implicit Contracts
//
// Every node has a unique string ID, a Type, a display Name, and a flat
// map[string]string of Attrs (stored as JSON text in SQLite). Every edge links
// From -> To with a Type and optional Attrs.
//
// Built-in node and edge type constants model databases, OpenAPI specs,
// codebases, and document collections, but consumers are free to use any custom
// node types (e.g. "customer", "contract", "issue", "department") and edge types
// (e.g. "owns", "works_on", "depends_on").
//
// Node ID conventions used by gatt's built-in extractors include:
//   - Database: "table:<schema>.<table>", "column:<schema>.<table>.<col>"
//   - OpenAPI:  "schema:<Name>", "endpoint:<METHOD>:<path>"
//   - Codebase: "file:<relpath>", "func:<relpath>:<name>:<line>", "route:<relpath>:<METHOD>:<line>"
//   - Docs:     "doc:<name>", "sec:<name>:<index>", "chunk:<name>:<sec>:<chunk>"
//
// # Persistence Contracts
//
// Save chooses SQLite when the path ends in .db, .sqlite, or .sqlite3, and JSON
// otherwise.
//
//   - Edge uniqueness in SQLite: the edges table enforces UNIQUE(efrom, eto, etype)
//     via INSERT OR IGNORE. Duplicate (From, To, Type) edges in memory are collapsed
//     to one row when persisted to SQLite.
//   - Incremental delta saves: call StartJournal right after loading a SQLite graph
//     before mutating it so Save writes only removed/added nodes, edges, and FTS5 rows.
//   - Annotations sidecar: Load automatically merges a sibling annotations.json file
//     if present; LoadRaw reads the raw stored graph without merging annotations
//     (use LoadRaw when diffing two snapshots).
//
// # Traversal and Access Control (RBAC)
//
// Use OutEdges, InEdges, EdgesOf, FindPath, Subgraph, and Reachable to query the
// graph. TraverseOptions.AllowNode lets callers enforce per-node access control
// (such as folder-scoped RBAC) during traversal so restricted nodes are never
// returned or traversed through.
//
// # Full-Text Search and Custom Domains
//
// SQLite persistence maintains an FTS5 index populated from Graph.NodeText(id).
// Custom node types automatically index their attributes and connected neighbor
// names deterministically; callers can also set Graph.NodeTextFunc for complete
// control over indexed text.
package graph
