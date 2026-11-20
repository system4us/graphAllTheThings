# graphAllTheThings (gatt)

Turn any source's metadata into a knowledge graph that your AI agent queries through MCP — instead of re-introspecting a database, or re-grepping a codebase, on every question.

| Source | Extracted | Ask it |
|--------|-----------|--------|
| **Database** (SQLite, PostgreSQL) | tables, columns, enums, FKs, indexes | "which tables hold login events, and how do I join them?" |
| **API spec** (OpenAPI 3.x, Swagger 2.0) | endpoints, schemas, `$ref`s, auth | "what's the full request to create an order?" |
| **Codebase** (Go, TS/JS, Python, Rust, Java, C#, Kotlin, Swift) | functions, call graph, routes, ORM models, docs | "what breaks if I change this signature?" |
| **Documents** (PDF, DOCX, Markdown, TXT) | documents, sections, reading-order chunks, screenshot refs | "how do I configure Okta SSO?" / draft a user guide |

Extract once, index semantically, and the agent gets tools that answer *which tables? how do I join them? who calls this?* in a single call. On a real 113-table CRM that is ~2.3k tokens in 1 tool call instead of ~7.8k across 6 introspection queries; on a codebase one `code-query` (~3 KB) replaces a 20–50k-token Grep/Read exploration.

Codebase parsing covers nine languages with uneven depth; see [supported languages and limits](docs/codebase-graphs.md#supported-languages-and-limits).

## Install

Via npm (Node 18+, prebuilt binaries for Linux, macOS, and Windows):

```bash
npm install -g gatt-graph
```

Or build from source (requires Go 1.25+ and a C compiler for tree-sitter):

```bash
go build -o ~/.local/bin/gatt ./cmd/gatt
```

## Recommended: central Docker server (MCP HTTP + ONNX + Qdrant)

Run one local container (`system4us/gatt:latest`) that serves **all your repositories** over Streamable HTTP MCP, with a shared local **ONNX** embedder (`paraphrase-multilingual-MiniLM-L12-v2`) and an embedded **Qdrant** vector store — both running strictly on loopback inside the container (only `127.0.0.1:7788` is published).

```bash
# 1. Start the container mounting your code root(s) read-only.
#    Every git repo under explicit --mount folders is discovered and indexed
#    sequentially in the background, and reindexed incrementally on every commit.
gatt docker up --mount /path/to/your/repos

# 2a. Register ONCE globally across all repos (Claude Code / Antigravity):
gatt install --http http://127.0.0.1:7788 --scope user

# 2b. Or register per-project inside a specific repo's .mcp.json:
gatt install --http http://127.0.0.1:7788
```

- **Multiple roots & document collections:** pass `--mount` multiple times (`gatt docker up --mount /mnt/code --mount /mnt/docs`) or preload a PDF/DOCX/Markdown folder with `--repos /path/to/corpus`. Directories without `.git` or code manifests are automatically classified as `docs` collections (supporting `.gattignore` exclusions).
- **Windows (Docker Desktop):** drive paths like `C:\Users\you\code` are mapped automatically to `/host/c/Users/you/code` across `gatt docker up`, `gatt install --http`, and `gatt notify`.
- **Manage the container:** `gatt docker status`, `gatt docker logs -f`, `gatt docker pull`, `gatt docker down` (named volumes `gatt-data` and `gatt-cache` are preserved).
- **Local build:** `GATT_MOUNT_ROOT=$HOME docker compose up -d --build`.

## Standalone CLI / stdio MCP (without Docker)

```bash
gatt extract codebase .                   # or: extract docs|sqlite|postgres|openapi ...
gatt index                                # embed nodes (optional; keyword search works without)
gatt install                              # register stdio MCP server in Claude Code / Antigravity
```

Query directly from the terminal (the MCP tools expose the same operations):

```bash
gatt code-query "how does incremental refresh work"       # functions, callers, docs in one pack
gatt doc-query "how to configure Okta SSO"                # hybrid Q&A over PDF/DOCX/MD/TXT with confidence signal
gatt doc-guide "configure Slack subscriber token"         # full canonical doc pack + screenshots + variants for authoring guides
gatt impact saveSQLite --depth 3                          # what breaks if this signature changes
gatt blast shared/schema.json --depth 2                   # callers, importers, co-change companions
gatt grep "TODO(#42)" --limit 0 --exclude '*_test.go'     # exhaustive scan: zero hits = proof of absence
gatt routes                                               # HTTP routes → handlers → ORM models → clients
gatt query "how many messages did each client send"       # tables, columns, joins for a data question
```

Codebase graphs refresh themselves: every query re-parses only the files that changed.

## Local embeddings without Docker

Semantic search works with **Ollama** (default, `ollama pull bge-m3`), the **local ONNX worker**, or any **OpenAI-compatible** server (`--embed-url http://host:port/v1`):

```bash
(cd tools/gatt-embed && npm install)      # once; Node >= 18
gatt serve --onnx tools/gatt-embed        # refreshes graph + vectors in the background
export GATT_EMBED_URL=http://127.0.0.1:7801/v1
```

Optional agent hook to trigger immediate background refresh on edits (silent; exits 0 even if nothing is listening):

```json
{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"gatt notify"}]}]}}
```

Only nodes whose text changed are re-embedded: on a 21,830-node graph the first index takes ~5 min, then an edit costs ~1.6 s. Details, backends and tuning: [docs/embeddings.md](docs/embeddings.md).

## Documentation

| | |
|---|---|
| [Command reference](docs/cli.md) | every query command with examples |
| [MCP tools](docs/mcp-tools.md) | what the agent can call |
| [Embeddings and background refresh](docs/embeddings.md) | Docker, ONNX, Qdrant, Ollama, `serve`, hooks, env vars |
| [The call graph](docs/call-graph.md) | what a `CALLS` edge means, what is left out, measured accuracy |
| [Codebase graphs](docs/codebase-graphs.md) | impact/blast/routes/models/doc-drift, the `.gatt/` overlay |
| [Annotations and freshness](docs/annotations-and-freshness.md) | curated business knowledge, drift checks |
| [Graph model](docs/graph-model.md) | node and edge types per source |
| [Architecture and layout](docs/architecture.md) | how it fits together, adding a connector |
| [Docker Hub guide](docs/dockerhub.md) | container env vars, volumes, endpoints |
| [Roadmap](docs/roadmap.md) | |

## License

Copyright (C) 2026 Leandro Fusco <leandrofusdev@gmail.com>

Licensed under the [GNU AGPL v3 or later](LICENSE). You may use, modify and redistribute it, including commercially; if you distribute a modified version or let users interact with one over a network (AGPL §13), you must offer them its source under the same license. Dependencies and downloaded embedding models keep their own licenses.
