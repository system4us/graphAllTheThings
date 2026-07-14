# graphAllTheThings (gatt)

Turn any source's metadata into a semantic knowledge graph that AI agents query through MCP — instead of re-discovering schemas with live introspection queries on every question.

**Problem:** an agent asked *"average users that logged in 3 months ago"* today burns many round-trips discovering tables, columns, and joins before writing a single query.

**Solution:** extract metadata **once** (schema, enums, foreign keys, indexes, comments) into a graph, index it semantically, and give the agent five MCP tools that answer "which tables?", "which columns?", "how do I join them?" instantly.

## Architecture

```
source (sqlite | postgres | openapi | ...)
        │  extract (once)
        ▼
  gatt-out/graph.json      ← source of truth: typed nodes + edges, traversal in memory
        │  index
        ▼
  gatt-out/vectors.json    ← semantic layer: node embeddings (Ollama nomic-embed-text),
        │                    cosine search in-process; --qdrant swaps in a Qdrant server
        ▼
  MCP stdio server         ← agents call tools; zero live-DB introspection
```

- **Graph structure** (nodes, edges, join paths) lives in `graph.json` — portable, versionable, traversed in memory.
- **Vector index is in-process by default.** Metadata graphs are small (hundreds to a few thousand nodes), so brute-force cosine takes microseconds — no vector database required. Pass `--qdrant URL` to `index`/`search`/`mcp` to opt into a Qdrant server (useful for very large or shared indexes).
- **Semantic search degrades gracefully**: if the index or Ollama is unavailable, `find_entities` falls back to keyword matching.

### Graph model

Nodes: `database`, `table`, `column`, `view`, `index` — attrs carry data types, enum values, defaults, row counts, DDL.
Edges: `HAS_TABLE`, `HAS_COLUMN`, `HAS_INDEX`, `INDEXES`, `FOREIGN_KEY` (column→column), `REFERENCES` (table→table, with `from_column`/`to_column` for join building).

## Quickstart

Requires: Go 1.24+ and Ollama on `:11434` with `nomic-embed-text` pulled (optional — keyword fallback works without it).

```bash
go build -o gatt ./cmd/gatt

./gatt extract sqlite path/to/db.sqlite     # → gatt-out/graph.json
./gatt extract postgres "postgres://user:pass@host:5432/db?sslmode=disable"
./gatt index                                # embed nodes → gatt-out/vectors.json
./gatt search "user login timestamps"       # sanity-check semantic search
./gatt mcp                                  # MCP stdio server
```

Register in Claude Code (`.mcp.json` in any project):

```json
{
  "mcpServers": {
    "gatt": {
      "command": "/path/to/gatt",
      "args": ["mcp", "--graph", "/path/to/gatt-out/graph.json"]
    }
  }
}
```

## MCP tools

| Tool | Purpose |
|------|---------|
| `graph_overview` | Source, node counts, all tables with column counts and references. Orientation call. |
| `find_entities` | Semantic search: "user login timestamps" → `sessions.logged_in_at`. Filter by node type. |
| `describe_entity` | One node in full: attrs (types, enums, row counts, DDL) + all relationships. |
| `join_path` | Cheapest FK path between two tables with exact join columns — returns a ready `JOIN ... ON ...` hint. Routes around hub tables (`tenant_id`-style FKs every table carries), which a naive shortest path would cut through, producing semantically wrong joins. |
| `sql_context` | One-shot context pack for a question: most relevant tables fully described. Feed straight into SQL generation. |

Example — `join_path users → products`:

```
JOIN orders ON users.id = orders.user_id
JOIN order_items ON orders.id = order_items.order_id
JOIN products ON order_items.product_id = products.id
```

## Adding a connector

Implement `connector.Connector` (`internal/connector/connector.go`):

```go
type Connector interface {
    Name() string
    Extract(ctx context.Context) (*graph.Graph, error)
}
```

Emit nodes/edges with the model in `internal/graph/model.go`, wire into `cmd/gatt/main.go`. The graph, index, search, and MCP layers need no changes.

## Roadmap

- [x] PostgreSQL connector (`pg_catalog`: native enums, column comments, multi-column FKs, view dependencies, multi-schema)
- [ ] OpenAPI connector (endpoints, schemas, `$ref` relationships)
- [ ] `--update` incremental re-extraction with change detection
- [ ] Cross-source graphs (DB + API spec merged: which endpoint touches which table)
- [ ] Query-log mining: add edges from JOINs observed in real queries (relationships not declared as FKs)

## Layout

```
cmd/gatt/                 CLI: extract | index | search | mcp
internal/graph/           graph model, traversal, persistence
internal/connector/       Connector interface + sqlite/ and postgres/ implementations
internal/embed/           Ollama embedding client
internal/store/           VectorStore interface
internal/store/local/     default in-process cosine index (vectors.json)
internal/store/qdrant/    opt-in Qdrant REST backend
internal/mcpserver/       MCP stdio server (official go-sdk)
tools/mkdemo/             generates a demo SQLite DB for testing
```
