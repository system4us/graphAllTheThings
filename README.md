# graphAllTheThings (gatt)

Turn any source's metadata into a semantic knowledge graph that AI agents query through MCP — instead of re-discovering schemas with live introspection queries (or re-grepping a codebase) on every question.

**Problem:** an agent asked *"average users that logged in 3 months ago"* today burns many round-trips discovering tables, columns, and joins before writing a single query. Asked *"where is auth handled"* in a repo, it burns the same round-trips in Grep → Glob → Read chains.

**Solution:** extract metadata **once** (schema, enums, foreign keys — or functions, call edges, doc comments) into a graph, index it semantically, and give the agent MCP tools that answer "which tables?", "which columns?", "how do I join them?", "who calls this function?" instantly.

| Source | Extracted | Ask it |
|--------|-----------|--------|
| **Database** (SQLite, PostgreSQL) | tables, columns, enums, FKs, indexes, comments | "which tables hold login events, and how do I join them?" |
| **API spec** (OpenAPI 3.x, Swagger 2.0) | endpoints, schemas, `$ref` relationships, auth | "what's the full request to create an order?" |
| **Codebase** (Go, TS/TSX, JS/JSX, Python, Rust, Java, C#, Kotlin, Swift) | functions, types, call graph, HTTP routes, ORM models, docs, git co-change | "what breaks if I change this signature?" / "which routes touch this model?" / "which docs went stale?" |

## Highlights

- **One graph engine, three source shapes** — traversal, search, join paths and MCP tools are shared.
- **Never stale (codebases)** — every query checks file mtimes and re-parses only what changed (~60 ms when nothing did).
- **Hybrid search** — FTS5 bm25 plus semantic embeddings; keyword search still works with no embedding backend at all.
- **Local, incremental embeddings** — `gatt serve --onnx` embeds on your CPU with no external service and re-embeds only nodes whose text changed. A 21,830-node graph indexes in ~5 minutes once; an edit then costs ~1.6 s.
- **Exhaustive when it matters** — `gatt grep` is a proof of absence; `search` is top-N.
- **Everything in one binary + one directory** (`gatt-out/`), versionable and portable.

## Contents

[Quickstart](#quickstart) · [Semantic search and background refresh](#semantic-search-and-background-refresh) · [Codebase graphs](#codebase-graphs) · [Curated annotations](#curated-annotations) · [Keeping it fresh](#keeping-it-fresh) · [MCP tools](#mcp-tools) · [Architecture](#architecture) · [Adding a connector](#adding-a-connector) · [Layout](#layout) · [Roadmap](docs/roadmap.md) · [License](#license)

## Quickstart

**Requires:** Go 1.25+. Semantic search is optional — without an embedding backend, search falls back to keyword/FTS5. For embeddings use Ollama with `bge-m3` pulled (default, `:11434`) or the bundled local ONNX worker (see [Semantic search and background refresh](#semantic-search-and-background-refresh)).

Measured on a real 113-table CRM: answering one data question costs **~2.3k tokens in 1 tool call** with gatt vs ~7.8k tokens across 6 introspection queries (list tables + describe candidates) — and the join chain comes out correct on the first try. On a codebase, one `code-query` (~3 KB) replaces a 20–50k-token Grep/Read exploration.

```bash
go build -o ~/.local/bin/gatt ./cmd/gatt

gatt extract sqlite path/to/db.sqlite     # → gatt-out/graph.db
gatt extract postgres "postgres://user:pass@host:5432/db?sslmode=disable"
gatt extract openapi http://localhost:8000/openapi.json   # live FastAPI spec (or a .json/.yaml file; OpenAPI 3.x or Swagger 2.0)
gatt extract codebase .                                   # parse a repo → gatt-out/graph.db (pass --out ...json for a portable/diffable graph)
gatt index                                # embed nodes → gatt-out/vectors.json + vectors.bin
gatt install                              # register MCP server in Claude Code
gatt install --scope agy                  # register MCP server in Antigravity CLI
```

`gatt install` uses `claude mcp add` when the CLI is available (`--scope project|user`), otherwise merges into `./.mcp.json` directly. Use `--scope agy` to register in `~/.gemini/config/mcp_config.json` (Antigravity's global MCP config, per the `agy` CLI's own embedded docs). `--scope` auto-detects when omitted: it prefers `claude` if that's on PATH, and falls back to `agy` if only that one is — useful on a fresh Windows setup where `claude` may not be on PATH yet. `gatt install` also copies its own binary to `~/.local/bin` and adds that to PATH if `gatt` isn't already resolvable (shell rc file on macOS/Linux, `HKCU\Environment` on Windows); pass `--path=false` to skip that.

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
gatt routes                               # HTTP routes found in code: method, path, handler,
                                          # middleware, models touched, frontend call sites
gatt models                               # ORM models found in code: DB table, field→column
                                          # renames, associations (explicit + FK-name inferred)
gatt doc-drift                            # docs whose code references broke (deleted/renamed
                                          # symbols, moved files) or went stale (code changed
                                          # after the doc's last commit)
gatt diff HEAD~5                          # structural diff vs a git ref: added/removed/
                                          # changed/renamed/moved functions & types
gatt path clients conversation_messages   # FK join path with exact columns
gatt explain messages                     # one node: attrs + relationships
gatt overview                             # all tables (or files/components), counts, references
```


## Semantic search and background refresh

Queries refresh the *graph* (cheap) but never wait on an embedding server. Vectors are built by `gatt index`, or kept up to date in the background by `gatt serve`.

### Choose an embedding backend

| Backend | How | Notes |
|---|---|---|
| **Ollama** (default) | `ollama pull bge-m3`; nothing else to configure | multilingual `bge-m3`, `http://localhost:11434` |
| **Local ONNX worker** | `gatt serve --onnx tools/gatt-embed` | runs on CPU, no external service; model downloaded on first use (~130 MB) |
| **Any OpenAI-compatible server** | `--embed-url http://host:port/v1` | Text Embeddings Inference, llama.cpp, LM Studio, …; optional token in `GATT_EMBED_API_KEY` |

The vectors themselves are stored locally (default) or in Qdrant (`--qdrant URL`). The model name is stored with them; a different model is refused rather than silently replacing the index (`gatt index --full` to switch).

### Set it up (local ONNX)

```sh
(cd tools/gatt-embed && npm install)      # once; Node >= 18; a postinstall prune leaves ~120 MB
gatt serve --onnx tools/gatt-embed        # keeps graph + vectors fresh, starts and stops the worker
export GATT_EMBED_URL=http://127.0.0.1:7801/v1   # so queries embed with the same model
```

Then let your agent tell `serve` when something may have changed. `gatt notify` is silent and always exits 0 — even when nothing is listening — so it can never fail a hook:

```json
{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"gatt notify"}]}]}}
```

`gatt serve --no-embed` refreshes only the graph. `GATT_EMBED_ON_REFRESH=1` makes query commands embed inline again (the old behaviour).

### What it costs (Sagpic, 21,830 nodes, 12 cores)

| | |
|---|---|
| First index | ~5 min (~60 nodes/s); Qdrant 5 min 24 s |
| Hook pass, nothing changed | ~35 ms |
| Hook pass, one file edited | ~1.6 s (Qdrant: the same) |
| Re-extracting an unchanged repo | 0 nodes re-embedded |

### How it works

- **`serve`** listens on `127.0.0.1:7788` (`POST /refresh`, `GET /status` with embedding progress), rejects any non-loopback `Host` and any `Origin` header, and coalesces a burst of hooks into one extra pass.
- **Incremental by content.** A node is re-embedded only if its text hash changed. The text lists neighbours (callers, references, …) sorted, so edge order — which differs between extractions and after an incremental refresh — never causes re-embeds. Nodes that left the graph are removed from the index. Upgrading from vectors built before this change re-embeds the affected nodes once.
- **The worker** (`tools/gatt-embed`: `@huggingface/transformers` + ONNX Runtime) exposes an OpenAI-compatible `/v1/embeddings`. Texts are truncated to 128 tokens (what the default multilingual MiniLM, 384-d, was trained on) and embedded in length-sorted sub-batches, because padding to the longest text made one long node slow its whole batch ~7×.
- **Local index format.** `vectors.json` holds only metadata (3 MB for 21.8k nodes) and `vectors.bin` the raw float32 vectors (33 MB; it used to be one 106 MB JSON). A generation id ties the two files together, so a reader that catches a write in progress retries instead of mixing them. Older all-in-JSON files are still read and upgraded on the next write; older gatt *binaries* cannot read the new layout.
- **Qdrant** is incremental too: a pass reads the stored text hashes (paged), uploads only changed nodes and deletes the ones that left the graph. `gatt index --full` recreates the collection.
- **"Up to date"** is a stamp of the graph file (size + mtime) written when a catch-up finishes, taken before the graph is read — not a comparison of file times — so a changed clock, a restored older copy, or a change during embedding can't make `serve` skip a needed pass.
- **Concurrent processes.** Graph refresh+save and the vector-index write each take a lock file (`<graph>.lock`, `vectors.json.lock`; considered stale after 5 min), so `serve` and query commands can run together without losing writes.

### Environment variables

| Variable | Used by | Meaning |
|---|---|---|
| `GATT_EMBED_URL` | all commands | default for `--embed-url` |
| `GATT_EMBED_API_KEY` | OpenAI-compatible mode | bearer token |
| `GATT_EMBED_ON_REFRESH` | query commands | `1` = embed inline on refresh |
| `GATT_ADDR` | `serve`, `notify` | listen / target address (default `127.0.0.1:7788`) |
| `GATT_EMBED_WORKER` | `serve` | default for `--onnx` |
| `GATT_EMBED_MODEL`, `GATT_EMBED_PORT`, `GATT_EMBED_CACHE` | worker | model, port, download cache |
| `GATT_EMBED_MAX_TOKENS`, `GATT_EMBED_SUB_BATCH` | worker | truncation length (128), sub-batch size (16) |
| `GATT_EMBED_KEEP_GPU`, `GATT_EMBED_KEEP_WEB` | worker install | `1` = keep GPU / browser runtimes the postinstall prune removes |

## Codebase graphs

`gatt extract codebase <dir>` parses the repo with tree-sitter and builds the call graph. When `<dir>` is a git checkout, extraction (and `gatt grep`) additionally respects the repo's own `.gitignore`/`.git/info/exclude` via `git ls-files` — not just a fixed list of common build-output names (`dist`, `build`, `node_modules`, …) — so a project-specific output directory is excluded too, instead of a compiled/minified copy of every function competing with its real source definition as an ambiguous same-named node. Falls back to the fixed skip list alone when `<dir>` isn't a git checkout.

The agent-workflow commands:

- **`gatt code-query "<question>"`** — the code analogue of `query`: the most relevant functions (signature, exact `file:line`, resolved callers/callees, doc comment, body when short), types with their methods, and matching docs, in one compact pack. An agent reads only the line ranges the pack points at instead of whole files.
- **`gatt impact <function>`** — walks `CALLS` edges backwards, transitively (`--depth`, default 3): every caller that breaks if the signature or behavior changes. Run it before refactors; `[test]` tags show which tests cover the blast radius.
- **`gatt blast <file-or-function>`** — blast radius of modifying *any* node, including JSON/YAML/SQL/CSS data files (indexed with a content hash): transitive callers **plus** file importers (relative imports and tsconfig path aliases resolve to local file nodes), regenerated outputs via `GENERATES` edges (declared as `"generates": [{"from": …, "to": …}]` in `.gatt/relations.json`), a warning when the target is itself generated, same-basename copies flagged `[identical]`/`[diverged]` by hash, and **git co-change companions** — files with no static edge that historically ship in the same commits (a component's stylesheet, the doc page of a service, the e2e test of a controller, i18n bundles). For a function target, both `impact` and `blast` also list a `shares mutable state:` section — the *other* functions reading/writing the same tracked singleton property (e.g. `config.session`), a data-flow signal `CALLS` alone can't see (heuristic, JS/TS/JSX only, one hop). Run it before editing shared schema/config files.
- **`gatt grep <pattern>`** — exhaustive literal (or `--regex`) scan of every file under the root, using the same skip rules as extraction. Independent of the indexed extension set and independent of ranking: a zero-result answer is a reliable proof of absence, unlike `search`/`find_entities`, which is semantic/top-N.
- **`gatt tree [path]`** — a directory tree synthesized from file nodes (the graph has no directory nodes), each file annotated with its doc summary: a leading file/package comment, a markdown doc's title, or its earliest function's doc.
- **`gatt routes`** — every HTTP route detected in code: method, path, resolved handler (identifier, inline arrow, or `controller.method` reference), middleware chain, **the ORM models the handler chain touches** (`models:`), and **the client call sites that hit it** (`called from:`). Detects Express-style `router.get/post/…` registrations in JS/TS/JSX; Spring `@GetMapping`/`@RequestMapping(method=…)` (Java) and ASP.NET `[HttpGet]`/`[Route]` (C#) annotations with class prefixes; Go registrations — gin/echo `r.GET`, chi `r.Get`, `mux.HandleFunc("GET /x", h)` (1.22 patterns; no verb = `ANY` wildcard), gorilla `.Methods("POST")` chains — statement-context only, so `resp, err := http.Get(…)` stays a client call; and Python decorators — Flask/FastAPI `@app.get("/x")`, `@app.route("/x", methods=["POST"])`, `<int:id>` → `:param`. Anything else: tag handlers via `annotate route_method=… route_path=…`.
- **`gatt models`** — every ORM model detected in code, grouped by file: DB table, field count, the field→column renames a SQL grep can't see, and associations in both directions. Detection is layered and language-agnostic: Sequelize `Model.init`/`sequelize.define` + explicit associations, TypeORM decorators, Go DB struct tags, Django/SQLAlchemy classes, `*_id`-style FK-name inference, `.gatt/models.json` overlay, `annotate model_table=…` fallback.
- **`gatt doc-drift`** — which documentation lies: markdown docs whose inline-code references no longer resolve (a symbol deleted/renamed, a cited file moved) or point at code whose last git commit postdates the doc's. Run it after refactors to get the list of docs needing an update, and before trusting a doc.
- **`gatt diff [ref]`** — structural diff of the working tree against a git ref (default `HEAD`): added/removed/changed/renamed/moved functions and types, detected by matching signatures across two extractions (not a textual diff), plus the current callers of anything that changed. Reuses git's own rename detection (`git diff -M`) for whole-file renames; function-level renames/moves are a same-file (then cross-file) signature-match heuristic.

**Never stale:** every query command (and the MCP server) checks file mtimes before answering (~60ms), re-parses only the files that changed since the last extract, evicts deleted entities, re-wires cross-file edges (calls, imports, mentions, associations, route models), and re-embeds just the changed nodes — by default only via `gatt serve` / `gatt index` (see [Semantic search and background refresh](#semantic-search-and-background-refresh)), so a query never waits on an embedding server (`GATT_EMBED_ON_REFRESH=1` restores the old inline behaviour). Wrong line numbers are worse than no graph, so you never re-run `extract` by hand mid-session. (Exceptions that refresh only on full re-extract: `CO_CHANGED` edges — git history has no incremental delta worth mining — and `CALLS_ENDPOINT` edges from files that didn't themselves change.)

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
| `routes` | Every HTTP route detected in code: method, path, handler (incl. `controller.method` refs), middleware chain, models touched, frontend call sites. The full-stack intersection in one call. |
| `models` | Every ORM model detected in code: DB table, field→column renames, associations (explicit + FK-name inferred). Language-agnostic layered detection; `.gatt/models.json` + `annotate model_table` for unknown ORMs. |
| `doc_drift` | Docs whose code references broke (deleted/renamed symbols, moved files) or went stale (referenced code committed after the doc). Run after refactors, and before trusting a doc. |
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

## Architecture

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

For the node and edge types of each source (database, API spec, codebase) see **[docs/graph-model.md](docs/graph-model.md)**.

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

## License

gatt is free software, licensed under the **GNU Affero General Public License v3.0 or later** (`AGPL-3.0-or-later`). See [LICENSE](LICENSE).

In short: you may use, study, modify and redistribute it, including commercially; if you distribute a modified version, or let users interact with one over a network (AGPL §13), you must offer them its corresponding source under the same license.

Things gatt uses or downloads keep their own licenses: Go and npm dependencies (see `go.mod` and `tools/gatt-embed/package-lock.json`), and the embedding model weights fetched from Hugging Face on first use — check the model card of the model you configure.
