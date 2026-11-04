# Architecture and layout

```
source (sqlite | postgres | openapi | codebase)
        │  extract (once; incremental after that)
        ▼
  gatt-out/graph.db        ← source of truth: typed nodes + edges (SQLite + FTS5,
        │                    default) — or graph.json (portable, versionable, via --out)
        │  index  (or `gatt serve`, in the background)
        ▼
  gatt-out/vectors.json    ← semantic layer: node embeddings from Ollama, a local ONNX worker
  gatt-out/vectors.bin       or any OpenAI-compatible server; cosine search in-process;
        │                    --qdrant swaps in a Qdrant server
        ▼
  MCP stdio server         ← agents call tools; zero live introspection
```

- **Graph structure** (nodes, edges, join paths) lives in `graph.db` by default (SQLite with an FTS5 full-text index, incremental delta writes) or `graph.json` (portable, diff-able) if you pass `--out gatt-out/graph.json` on the first extract. All commands auto-detect whichever exists; `--graph PATH` overrides.
- **Search is hybrid**: FTS5 bm25 (when the graph is SQLite) + semantic embeddings, so results are good even before indexing. If the vector index or Ollama is unavailable, `find_entities` falls back to keyword matching.
- **Vector index is in-process by default.** Metadata graphs are small (hundreds to a few thousand nodes), so brute-force cosine takes microseconds — no vector database required. Pass `--qdrant URL` to `index`/`search`/`mcp` to opt into a Qdrant server (useful for very large or shared indexes).

For the node and edge types of each source (database, API spec, codebase) see **[docs/graph-model.md](graph-model.md)**.

## Adding a connector

Implement `connector.Connector` (`internal/connector/connector.go`):

```go
type Connector interface {
    Name() string
    Extract(ctx context.Context) (*graph.Graph, error)
}
```

Emit nodes/edges with the model in `internal/graph/model.go`, wire into `cmd/gatt/main.go`. The graph, index, search, and MCP layers need no changes.

## Layout

```
cmd/gatt/                 CLI: init | extract | enrich | index | query | code-query | impact |
                          blast | doc-drift | search | grep | tree | routes | models | diff |
                          path | explain | annotate | overview | serve | notify | mcp | install
internal/engine/          query operations shared by CLI and MCP
internal/graph/           graph model, traversal, persistence (JSON + SQLite/FTS5)
internal/connector/       Connector interface + sqlite/, postgres/, openapi/, codebase/
internal/embed/           embedding client: Ollama and OpenAI-compatible (/v1) servers
internal/indexer/         incremental (hash-based) indexing into any VectorStore
internal/lock/            cross-process lock files
internal/store/           VectorStore interface
internal/store/local/     default in-process cosine index (vectors.json + vectors.bin)
internal/store/qdrant/    opt-in Qdrant REST backend (+ qdranttest, an in-memory fake for tests)
internal/mcpserver/       MCP stdio server (official go-sdk)
tools/mkdemo/             generates a demo SQLite DB for testing
tools/gatt-embed/         local ONNX embedding worker (Node), used by `gatt serve --onnx`
docs/                     graph model reference, roadmap
```
