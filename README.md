# graphAllTheThings (gatt)

Turn any source's metadata into a semantic knowledge graph that AI agents query through MCP — instead of re-discovering schemas with live introspection queries (or re-grepping a codebase) on every question.

**Problem:** an agent asked *"average users that logged in 3 months ago"* today burns many round-trips discovering tables, columns, and joins before writing a single query. Asked *"where is auth handled"* in a repo, it burns the same round-trips in Grep → Glob → Read chains.

**Solution:** extract metadata **once** (schema, enums, foreign keys — or functions, call edges, doc comments) into a graph, index it semantically, and give the agent MCP tools that answer "which tables?", "which columns?", "how do I join them?", "who calls this function?" instantly.

Three source shapes, one graph engine:

| Source | Extracted | Ask it |
|--------|-----------|--------|
| **Database** (SQLite, PostgreSQL) | tables, columns, enums, FKs, indexes, comments | "which tables hold login events, and how do I join them?" |
| **API spec** (OpenAPI 3.x, Swagger 2.0) | endpoints, schemas, `$ref` relationships, auth | "what's the full request to create an order?" |
| **Codebase** (Go, TypeScript, Python, Rust) | functions, types, call graph, doc comments, docs | "how does extraction work?" / "what breaks if I change this signature?" |

## Architecture

```
source (sqlite | postgres | openapi | codebase)
        │  extract (once; incremental after that)
        ▼
  gatt-out/graph.db        ← source of truth: typed nodes + edges (SQLite + FTS5,
        │                    preferred) — or graph.json (portable, versionable)
        │  index
        ▼
  gatt-out/vectors.json    ← semantic layer: node embeddings (Ollama bge-m3, multilingual),
        │                    cosine search in-process; --qdrant swaps in a Qdrant server
        ▼
  MCP stdio server         ← agents call tools; zero live introspection
```

- **Graph structure** (nodes, edges, join paths) lives in `graph.db` (SQLite with an FTS5 full-text index, incremental delta writes) or `graph.json` (portable, diff-able). All commands auto-detect whichever exists; `--graph PATH` overrides.
- **Search is hybrid**: FTS5 bm25 (when the graph is SQLite) + semantic embeddings, so results are good even before indexing. If the vector index or Ollama is unavailable, `find_entities` falls back to keyword matching.
- **Vector index is in-process by default.** Metadata graphs are small (hundreds to a few thousand nodes), so brute-force cosine takes microseconds — no vector database required. Pass `--qdrant URL` to `index`/`search`/`mcp` to opt into a Qdrant server (useful for very large or shared indexes).

### Graph model

Three source shapes share one graph so the traversal, search, and join machinery is reused across all of them.

**Database** — Nodes: `database`, `table`, `column`, `view`, `index` — attrs carry data types, enum values, defaults, row counts, DDL.
Edges: `HAS_TABLE`, `HAS_COLUMN`, `HAS_INDEX`, `INDEXES`, `FOREIGN_KEY` (column→column), `REFERENCES` (table→table, with `from_column`/`to_column` for join building).

**API spec (OpenAPI 3.x / Swagger 2.0)** — from FastAPI's `/openapi.json`, a swaggo-generated `swagger.json`, or any spec file. Nodes: `api`, `schema` (a component model, like a table), `property` (like a column), `endpoint` (an HTTP operation).
Edges: `HAS_SCHEMA`, `HAS_PROPERTY`, `HAS_ENDPOINT`, `REFERS_TO` (property→schema, a `$ref` — the FK analogue), `REFERENCES` (schema→schema, derived from `$ref`s, with `from_property`/`cardinality`), `ACCEPTS`/`RESPONDS_WITH` (endpoint→schema request/response bodies). Named enum components are inlined as a property's allowed values rather than modeled as a relationship, and `join_path` returns the `$ref` chain (`User → Order (buyer)`) instead of a SQL `JOIN`.

Each endpoint carries what a request actually needs: the **full URL** (server base — 3.x `servers` with variables substituted, or 2.0 `host`+`basePath` — joined to the path) and the **auth scheme** (`Bearer`, `Basic`, `apiKey header X-API-Key`, resolved from the operation's `security` or the global default, `OAuth2`/public overrides included). So an agent gets a copy-pasteable curl skeleton — method, URL, auth header, and the typed request body with its enums — from one `sql_context` call instead of loading a multi-megabyte spec.

**Codebase** — tree-sitter parses Go, TypeScript/TSX, JavaScript/JSX, Python, and Rust (plus Markdown docs). Nodes: `project`, `file`, `function` (with signature, `file:line` range, doc comment, body for short functions), `definition` (types and their methods — plus, for JS/TS/JSX, module-scope `const`/exported bindings like config objects and lookup tables, so `queueDefinitions`-style entities are queryable and doc-drift-checkable too), `component`, `feature`, `doc`, `comment` (a substantive floating comment not attached to any declaration), and — JS/TS/JSX only — `route` (an Express-style HTTP route registration) and `state` (a tracked property on a cross-file singleton, e.g. `config.session`). `model` nodes capture ORM models across languages — Sequelize `Model.init`/`sequelize.define` (JS/TS), TypeORM `@Entity`/`@Column` decorators, Go structs with `gorm:`/`db:`/`bun:`/`xorm:` tags, Django/SQLAlchemy classes — with the DB table, the field→column renames a SQL grep can't see, and associations: explicit (`A.hasMany(B)`, resolved even when declared in a central `setupAssociations` file) plus `*_id`/`…Id`/`…ID` foreign-key-name inference in any language (`inferred=true` on the edge). Unknown ORMs: declare base classes in `.gatt/models.json` (`{"base_classes": [...]}`) or tag a type via `annotate model_table=<table>`; `gatt models` lists them all. Two cross-layer edges close the full-stack chain: `USES_MODEL` (route → every model its handler chain touches, via resolved CALLS + file imports — graph-level, so language-agnostic) and `CALLS_ENDPOINT` (frontend call site → route: axios-style `x.get('/path')`, `fetch(...)`, and template paths `` `/x/${id}` `` normalized to `:param`, matched by method + path tail with ambiguous matches skipped). `gatt routes` shows both per route (`models:` / `called from:`); `blast` on a model lists the API surface it backs; `search`/`describe` on a frontend function shows `Calls backend routes: …`.
Edges: `CALLS` (resolved local calls — the call graph), `HAS_METHOD`, `BELONGS_TO`, `IMPORTS`, `CO_CHANGED` (git history), `MENTIONS` (doc → code it references), and — JS/TS/JSX only — `HANDLED_BY`/`USES_MIDDLEWARE` (route → function) and `READS`/`WRITES` (function → state). Generated code (`.pb.go`, `_gen.go`, `.min.js`, …) is excluded from context packs but still findable via search.

## Quickstart

Requires: Go 1.24+ and Ollama on `:11434` with `bge-m3` pulled (optional — keyword/FTS fallback works without it).

Measured on a real 113-table CRM: answering one data question costs **~2.3k tokens in 1 tool call** with gatt vs ~7.8k tokens across 6 introspection queries (list tables + describe candidates) — and the join chain comes out correct on the first try. On a codebase, one `code-query` (~3 KB) replaces a 20–50k-token Grep/Read exploration.

```bash
go build -o ~/.local/bin/gatt ./cmd/gatt

gatt extract sqlite path/to/db.sqlite     # → gatt-out/graph.json
gatt extract postgres "postgres://user:pass@host:5432/db?sslmode=disable"
gatt extract openapi http://localhost:8000/openapi.json   # live FastAPI spec (or a .json/.yaml file; OpenAPI 3.x or Swagger 2.0)
gatt extract codebase . --out gatt-out/graph.db           # parse a repo (SQLite graph preferred for code)
gatt index                                # embed nodes → gatt-out/vectors.json
gatt install                              # register MCP server in Claude Code
gatt install --scope agy                  # register MCP server in Antigravity CLI
```

`gatt install` uses `claude mcp add` when the CLI is available (`--scope project|user`), otherwise merges into `./.mcp.json` directly. Use `--scope agy` to register in `~/.gemini/antigravity-cli/mcp_config.json`.

Query from the terminal (same operations the MCP tools expose):

```bash
gatt query "how many messages did each client send this month"
                                          # context pack: tables, columns, enums, joins
gatt code-query "how does incremental refresh work"
                                          # context pack: functions (signature, file:line,
                                          # callers/callees, doc), types, docs
gatt impact saveSQLite --depth 3          # transitive callers: what breaks on a signature
                                          # change; test callers tagged [test]
gatt blast shared/schemas/product.json    # blast radius of a file: callers + importers +
                                          # generated copies + diverged duplicates
gatt search "user login timestamps"       # hybrid search over all nodes
gatt grep "TODO(#42)" --regex             # exhaustive literal/regex scan of every file —
                                          # a zero-result answer proves absence; search
                                          # above is semantic/top-N and is NOT exhaustive
gatt tree internal/engine --depth 2       # directory tree annotated with each file's doc
gatt routes                               # HTTP routes found in code (Express JS/TS/JSX):
                                          # method, path, handler, middleware
gatt diff HEAD~5                          # structural diff vs a git ref: added/removed/
                                          # changed/renamed/moved functions & types
gatt path clients conversation_messages   # FK join path with exact columns
gatt explain messages                     # one node: attrs + relationships
gatt overview                             # all tables (or files/components), counts, references
```

## Codebase graphs

`gatt extract codebase <dir>` parses the repo with tree-sitter and builds the call graph. When `<dir>` is a git checkout, extraction (and `gatt grep`) additionally respects the repo's own `.gitignore`/`.git/info/exclude` via `git ls-files` — not just a fixed list of common build-output names (`dist`, `build`, `node_modules`, …) — so a project-specific output directory is excluded too, instead of a compiled/minified copy of every function competing with its real source definition as an ambiguous same-named node. Falls back to the fixed skip list alone when `<dir>` isn't a git checkout.

Two commands are built for agent workflows:

- **`gatt code-query "<question>"`** — the code analogue of `query`: the most relevant functions (signature, exact `file:line`, resolved callers/callees, doc comment, body when short), types with their methods, and matching docs, in one compact pack. An agent reads only the line ranges the pack points at instead of whole files.
- **`gatt impact <function>`** — walks `CALLS` edges backwards, transitively (`--depth`, default 3): every caller that breaks if the signature or behavior changes. Run it before refactors; `[test]` tags show which tests cover the blast radius.
- **`gatt blast <file-or-function>`** — blast radius of modifying *any* node, including JSON/YAML/SQL/CSS data files (indexed with a content hash): transitive callers **plus** file importers (relative imports and tsconfig path aliases resolve to local file nodes), regenerated outputs via `GENERATES` edges (declared as `"generates": [{"from": …, "to": …}]` in `.gatt/relations.json`), a warning when the target is itself generated, same-basename copies flagged `[identical]`/`[diverged]` by hash, and **git co-change companions** — files with no static edge that historically ship in the same commits (a component's stylesheet, the doc page of a service, the e2e test of a controller, i18n bundles). For a function target, both `impact` and `blast` also list a `shares mutable state:` section — the *other* functions reading/writing the same tracked singleton property (e.g. `config.session`), a data-flow signal `CALLS` alone can't see (heuristic, JS/TS/JSX only, one hop). Run it before editing shared schema/config files.
- **`gatt grep <pattern>`** — exhaustive literal (or `--regex`) scan of every file under the root, using the same skip rules as extraction. Independent of the indexed extension set and independent of ranking: a zero-result answer is a reliable proof of absence, unlike `search`/`find_entities`, which is semantic/top-N.
- **`gatt tree [path]`** — a directory tree synthesized from file nodes (the graph has no directory nodes), each file annotated with its doc summary: a leading file/package comment, a markdown doc's title, or its earliest function's doc.
- **`gatt routes`** — every HTTP route detected in code: method, path, resolved handler, and middleware chain. Detects Express-style `router.get/post/put/delete/patch/all/use(path, ...)` registrations in JS/TS/JSX — v1 scope, no Go/Python route frameworks yet.
- **`gatt diff [ref]`** — structural diff of the working tree against a git ref (default `HEAD`): added/removed/changed/renamed/moved functions and types, detected by matching signatures across two extractions (not a textual diff), plus the current callers of anything that changed. Reuses git's own rename detection (`git diff -M`) for whole-file renames; function-level renames/moves are a same-file (then cross-file) signature-match heuristic.

**Never stale:** both commands (and the MCP server) check file mtimes before answering (~60ms), re-parse only the files that changed since the last extract, evict deleted entities, and re-embed just the changed nodes. Wrong line numbers are worse than no graph, so you never re-run `extract` by hand mid-session. (Exception: `CO_CHANGED` edges are mined from git history at full extract only — incremental refresh preserves them but new commits are folded in on the next re-extract.)

### Semantic overlay (`.gatt/`)

Structure comes from parsing; *meaning* comes from a curated overlay. `gatt init` scaffolds a `.gatt/` workspace:

```
.gatt/
  gatt.spec.json      project name, namespaces, manifest paths
  definitions.json    high-level business/architectural domains + their critical rules
  relations.json      features linked to their physical entry points and dependencies
  contracts.json      API/database contracts
  prompt.md           a directive you hand to an AI agent to populate the above
```

Point your agent at `.gatt/prompt.md` once: it explores the codebase and writes the domain definitions. Extraction then merges the overlay into the graph as `component`/`feature` nodes wired to real files — so `code-query` answers carry architecture context, not just symbols. The overlay lives in git and survives every re-extract.

### Linking API specs to source

For a swaggo/Go service, the spec and the code describe the same thing — gatt can wire them together:

```bash
gatt extract openapi swagger.json --code .   # endpoints/schemas link to their Go source:
                                             # describe_entity shows source: file:line
gatt enrich .                                # re-link an existing graph after code edits,
                                             # without re-extracting the spec
```

## Curated annotations

The schema knows structure, not business meaning: whether `enabled=false` rows count as real contacts, what a "contact" canonically is. Encode that once and every `sql_context`/`describe_entity` response carries it, so the agent writes the right query instead of asking:

```bash
gatt annotate contacts \
  default_filter="enabled = true" \
  entity_note="CRM contacts; enabled=false is a draft; source='google' is imported"
gatt annotate contacts --clear            # remove
```

Annotations live in `gatt-out/annotations.json` (a sidecar keyed by node id), merged over the graph on every load — so they survive re-running `extract`. Recognized keys: `default_filter` (a canonical `WHERE` clause, rendered like the auto-detected soft-delete filter) and `entity_note` (free-text definition). Any other key is stored and shown too. `sql_context` also emits the SQL `dialect:` up front so generated SQL is dialect-correct without inference.

## Keeping it fresh

The graph is a snapshot, so agents need to know how old it is and you need a cheap way to tell when the source has drifted:

```bash
gatt extract postgres "$DSN" --check   # re-read the source, print schema drift, DO NOT write
gatt extract postgres "$DSN"           # re-extract; prints the same drift, then writes
gatt index                             # re-embed only the nodes whose text changed (--full to force all)
```

- **Every extraction stamps `extracted_at`.** `sql_context` and `graph_overview` lead with a `# source: postgres:app, extracted 3h ago (2026-07-13)` line, so the agent can judge staleness and re-verify against the live DB when it matters — instead of trusting a snapshot blindly.
- **`--check` is the drift probe.** It hits the source and diffs against the current graph (added/removed tables & columns, changed types, FK count deltas) without touching the graph. Run it on a schedule or in CI; a non-empty drift report means it's time to re-extract and re-index.
- **Codebase graphs refresh themselves.** `code-query`, `impact`, and the MCP server detect changed files by mtime and re-parse only those before answering — no manual re-extract.
- **Re-extraction is non-destructive to curated knowledge.** Annotations live in their sidecar and are re-applied on load; if a re-extract removes a node an annotation targeted, you get a warning naming the orphaned annotation. The `.gatt/` overlay is merged fresh on every extract.
- **Re-indexing is incremental.** `index` reuses cached vectors for nodes whose embedding text is unchanged (content-hashed) and only embeds what actually moved — so re-indexing a 113-table CRM after a one-column migration embeds two nodes, not two thousand.

## MCP tools

**Read** — answer schema/code questions from the pre-built graph:

| Tool | Purpose |
|------|---------|
| `graph_overview` | Source, node counts, all tables (or API schemas, or files/components) with member counts and references. Orientation call. |
| `find_entities` | Hybrid search: "user login timestamps" → `sessions.logged_in_at`. Filter by node type. |
| `describe_entity` | One node in full: a table (types, enums, row counts, DDL), an API schema/endpoint (properties, `$ref`s, bodies), or a function/type (signature, doc, call edges) + all relationships. |
| `join_path` | Cheapest FK path between two tables with exact join columns — a ready `JOIN ... ON ...` hint (or the `$ref` chain between two API schemas). Routes around hub tables (`tenant_id`-style FKs every table carries), which a naive shortest path would cut through, producing semantically wrong joins. |
| `sql_context` | One-shot context pack for a data question: most relevant tables/schemas fully described. Feed straight into SQL generation. |
| `code_context` | One-shot context pack for a code question: relevant functions, types, docs with `file:line` and call graph. The `code-query` CLI, as a tool. |
| `impact` | Transitive callers of a function to depth N — what breaks if it changes. Run before refactors; `[test]` tags included. Also lists shared mutable state (JS/TS/JSX). |
| `blast` | Blast radius of any node — file (incl. data files), function, or type: callers + importers + regenerated outputs + diverged copies. Run before editing shared config/schema files. |
| `grep` | Exhaustive literal/regex search across every file — a zero-result answer is proof of absence, unlike the semantic/top-N `find_entities`. |
| `tree` | Directory tree synthesized from file nodes, each annotated with its doc summary. |
| `routes` | Every HTTP route detected in code (Express-style JS/TS/JSX): method, path, handler, middleware chain. |
| `code_diff` | Structural diff vs a git ref (default `HEAD`): added/removed/changed/renamed/moved functions & types, plus current callers of anything changed. |

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
- [x] OpenAPI connector (endpoints, schemas, `$ref` relationships) — OpenAPI 3.x + Swagger 2.0, from a `.json`/`.yaml` file or a live `http(s)://.../openapi.json` (FastAPI, swaggo)
- [x] Codebase connector (tree-sitter: Go, TS/TSX, JS/JSX, Python, Rust) with call graph, `.gatt/` semantic overlay, and mtime-based incremental refresh
- [x] SQLite graph storage (`graph.db`): FTS5 full-text index, delta writes
- [x] Incremental re-extraction with change detection (`extract --check` reports drift; `index` re-embeds only changed nodes)
- [x] Spec↔code linking (`extract openapi --code`, `gatt enrich`): endpoints/schemas point at their Go source
- [x] HTTP route entities in code (Express-style JS/TS/JSX): `gatt routes`
- [x] Shared mutable-state data flow (JS/TS/JSX named-import singletons): `impact`/`blast` shared-state section
- [x] Exhaustive literal/regex search (`gatt grep`) as a proof-of-absence complement to semantic `search`
- [x] Annotated directory tree (`gatt tree`) and floating "why" comments as queryable `comment` nodes
- [x] Structural diff against a git ref (`gatt diff`): rename/move-aware function/type changes + current callers
- [ ] Sample values for low-cardinality string columns (`status`, `type`, ...) that carry no declared enum — see `TODO(#2)` in `internal/connector/postgres/postgres.go`
- [ ] Cross-source graphs (DB + API spec merged: which endpoint touches which table)
- [ ] Query-log mining: add edges from JOINs observed in real queries (relationships not declared as FKs)
- [ ] Route detection for Go (net/http, gin, chi) and Python (Flask/FastAPI) — `gatt routes` is JS/TS/JSX only today

## Layout

```
cmd/gatt/                 CLI: init | extract | enrich | index | query | code-query | impact |
                          blast | search | grep | tree | routes | diff | path | explain |
                          annotate | overview | mcp | install
internal/engine/          query operations shared by CLI and MCP
internal/graph/           graph model, traversal, persistence (JSON + SQLite/FTS5)
internal/connector/       Connector interface + sqlite/, postgres/, openapi/, codebase/
internal/embed/           Ollama embedding client
internal/store/           VectorStore interface
internal/store/local/     default in-process cosine index (vectors.json)
internal/store/qdrant/    opt-in Qdrant REST backend
internal/mcpserver/       MCP stdio server (official go-sdk)
tools/mkdemo/             generates a demo SQLite DB for testing
```
