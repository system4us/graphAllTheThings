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
  gatt-out/vectors.json    ← semantic layer: node embeddings (Ollama bge-m3, multilingual),
        │                    cosine search in-process; --qdrant swaps in a Qdrant server
        ▼
  MCP stdio server         ← agents call tools; zero live-DB introspection
```

- **Graph structure** (nodes, edges, join paths) lives in `graph.json` — portable, versionable, traversed in memory.
- **Vector index is in-process by default.** Metadata graphs are small (hundreds to a few thousand nodes), so brute-force cosine takes microseconds — no vector database required. Pass `--qdrant URL` to `index`/`search`/`mcp` to opt into a Qdrant server (useful for very large or shared indexes).
- **Semantic search degrades gracefully**: if the index or Ollama is unavailable, `find_entities` falls back to keyword matching.

### Graph model

Two source shapes share one graph so the traversal, search and join machinery is reused across both.

**Database** — Nodes: `database`, `table`, `column`, `view`, `index` — attrs carry data types, enum values, defaults, row counts, DDL.
Edges: `HAS_TABLE`, `HAS_COLUMN`, `HAS_INDEX`, `INDEXES`, `FOREIGN_KEY` (column→column), `REFERENCES` (table→table, with `from_column`/`to_column` for join building).

**API spec (OpenAPI 3.x / Swagger 2.0)** — from FastAPI's `/openapi.json`, a swaggo-generated `swagger.json`, or any spec file. Nodes: `api`, `schema` (a component model, like a table), `property` (like a column), `endpoint` (an HTTP operation).
Edges: `HAS_SCHEMA`, `HAS_PROPERTY`, `HAS_ENDPOINT`, `REFERS_TO` (property→schema, a `$ref` — the FK analogue), `REFERENCES` (schema→schema, derived from `$ref`s, with `from_property`/`cardinality`), `ACCEPTS`/`RESPONDS_WITH` (endpoint→schema request/response bodies). Named enum components are inlined as a property's allowed values rather than modeled as a relationship, and `join_path` returns the `$ref` chain (`User → Order (buyer)`) instead of a SQL `JOIN`.

Each endpoint carries what a request actually needs: the **full URL** (server base — 3.x `servers` with variables substituted, or 2.0 `host`+`basePath` — joined to the path) and the **auth scheme** (`Bearer`, `Basic`, `apiKey header X-API-Key`, resolved from the operation's `security` or the global default, `OAuth2`/public overrides included). So an agent gets a copy-pasteable curl skeleton — method, URL, auth header, and the typed request body with its enums — from one `sql_context` call instead of loading a multi-megabyte spec.

## Quickstart

Requires: Go 1.24+ and Ollama on `:11434` with `bge-m3` pulled (optional — keyword fallback works without it).

Measured on a real 113-table CRM: answering one data question costs **~2.3k tokens in 1 tool call** with gatt vs ~7.8k tokens across 6 introspection queries (list tables + describe candidates) — and the join chain comes out correct on the first try.

```bash
go build -o gatt ./cmd/gatt

go build -o ~/.local/bin/gatt ./cmd/gatt

gatt extract sqlite path/to/db.sqlite     # → gatt-out/graph.json
gatt extract postgres "postgres://user:pass@host:5432/db?sslmode=disable"
gatt extract openapi http://localhost:8000/openapi.json   # live FastAPI spec (or a .json/.yaml file; OpenAPI 3.x or Swagger 2.0)
gatt index                                # embed nodes → gatt-out/vectors.json
gatt install                              # register MCP server in Claude Code
gatt install --scope agy                  # register MCP server in Antigravity CLI
```

`gatt install` uses `claude mcp add` when the CLI is available (`--scope project|user`), otherwise merges into `./.mcp.json` directly. Use `--scope agy` to register in `~/.gemini/antigravity-cli/mcp_config.json`.

Query from the terminal (same operations the MCP tools expose):

```bash
gatt query "how many messages did each client send this month"
                                          # context pack: tables, columns, enums, joins
gatt search "user login timestamps"       # semantic search over all nodes
gatt path clients conversation_messages   # FK join path with exact columns
gatt explain messages                     # one node: attrs + relationships
gatt overview                             # all tables, counts, references
```

### Curated annotations

The schema knows structure, not business meaning: whether `enabled=false` rows count as real contacts, what a "contact" canonically is. Encode that once and every `sql_context`/`describe_entity` response carries it, so the agent writes the right query instead of asking:

```bash
gatt annotate contacts \
  default_filter="enabled = true" \
  entity_note="CRM contacts; enabled=false is a draft; source='google' is imported"
gatt annotate contacts --clear            # remove
```

Annotations live in `gatt-out/annotations.json` (a sidecar keyed by node id), merged over the graph on every load — so they survive re-running `extract`. Recognized keys: `default_filter` (a canonical `WHERE` clause, rendered like the auto-detected soft-delete filter) and `entity_note` (free-text definition). Any other key is stored and shown too. `sql_context` also emits the SQL `dialect:` up front so generated SQL is dialect-correct without inference.

### Keeping it fresh

The graph is a snapshot, so agents need to know how old it is and you need a cheap way to tell when the source has drifted:

```bash
gatt extract postgres "$DSN" --check   # re-read the source, print schema drift, DO NOT write
gatt extract postgres "$DSN"           # re-extract; prints the same drift, then writes
gatt index                             # re-embed only the nodes whose text changed (--full to force all)
```

- **Every extraction stamps `extracted_at`.** `sql_context` and `graph_overview` lead with a `# source: postgres:app, extracted 3h ago (2026-07-13)` line, so the agent can judge staleness and re-verify against the live DB when it matters — instead of trusting a snapshot blindly.
- **`--check` is the drift probe.** It hits the source and diffs against the current graph (added/removed tables & columns, changed types, FK count deltas) without touching `graph.json`. Run it on a schedule or in CI; a non-empty drift report means it's time to re-extract and re-index.
- **Re-extraction is non-destructive to curated knowledge.** Annotations live in their sidecar and are re-applied on load; if a re-extract removes a node an annotation targeted, you get a warning naming the orphaned annotation.
- **Re-indexing is incremental.** `index` reuses cached vectors for nodes whose embedding text is unchanged (content-hashed) and only embeds what actually moved — so re-indexing a 113-table CRM after a one-column migration embeds two nodes, not two thousand.

## MCP tools

**Read** — answer schema questions from the pre-built graph:

| Tool | Purpose |
|------|---------|
| `graph_overview` | Source, node counts, all tables (or API schemas) with member counts and references, plus endpoints. Orientation call. |
| `find_entities` | Semantic search: "user login timestamps" → `sessions.logged_in_at`. Filter by node type. |
| `describe_entity` | One node in full: a table (types, enums, row counts, DDL) or an API schema/endpoint (properties, `$ref`s, request/response bodies) + all relationships. |
| `join_path` | Cheapest FK path between two tables with exact join columns — a ready `JOIN ... ON ...` hint (or the `$ref` chain between two API schemas). Routes around hub tables (`tenant_id`-style FKs every table carries), which a naive shortest path would cut through, producing semantically wrong joins. |
| `sql_context` | One-shot context pack for a question: most relevant tables/schemas fully described. Feed straight into SQL generation. |

**Maintain** — the agent curates and refreshes the graph itself:

| Tool | Purpose |
|------|---------|
| `annotate_entity` | Persist business knowledge learned mid-session (`entity_note`, `default_filter`) onto a node, so every later `sql_context`/`describe_entity` carries it. Written to the annotations sidecar; survives re-extraction. |
| `reload_graph` | Reload graph + vectors from disk to pick up an extraction/annotation made outside the server. Cheap; no source access. |
| `check_drift` | Re-read the live source and report how the snapshot has drifted — **without writing**. Needs a source configured. |
| `refresh_graph` | Re-extract from the live source, re-embed changed nodes, reload. **Writes.** Needs a source configured. |

`check_drift` and `refresh_graph` only appear when the server was started with the source wired in:

```bash
gatt install --source-kind postgres --source "$DSN"     # or: gatt mcp --source-kind postgres --source "$DSN"
```

The source (DSN/URL) is then stored in the MCP config so the server can re-extract on request — keep that in mind for DB credentials.

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
- [ ] Sample values for low-cardinality string columns (`status`, `type`, ...) that carry no declared enum — see `TODO(#2)` in `internal/connector/postgres/postgres.go`
- [x] OpenAPI connector (endpoints, schemas, `$ref` relationships) — OpenAPI 3.x + Swagger 2.0, from a `.json`/`.yaml` file or a live `http(s)://.../openapi.json` (FastAPI, swaggo)
- [x] Incremental re-extraction with change detection (`extract --check` reports drift; `index` re-embeds only changed nodes)
- [ ] Cross-source graphs (DB + API spec merged: which endpoint touches which table)
- [ ] Query-log mining: add edges from JOINs observed in real queries (relationships not declared as FKs)

## Layout

```
cmd/gatt/                 CLI: extract | index | query | search | path | explain | annotate | overview | mcp | install
internal/engine/          query operations shared by CLI and MCP
internal/graph/           graph model, traversal, persistence
internal/connector/       Connector interface + sqlite/, postgres/ and openapi/ implementations
internal/embed/           Ollama embedding client
internal/store/           VectorStore interface
internal/store/local/     default in-process cosine index (vectors.json)
internal/store/qdrant/    opt-in Qdrant REST backend
internal/mcpserver/       MCP stdio server (official go-sdk)
tools/mkdemo/             generates a demo SQLite DB for testing
```
