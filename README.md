# graphAllTheThings (gatt)

Turn any source's metadata into a knowledge graph that your AI agent queries through MCP — instead of re-introspecting a database, or re-grepping a codebase, on every question.

| Source | Extracted | Ask it |
|--------|-----------|--------|
| **Database** (SQLite, PostgreSQL) | tables, columns, enums, FKs, indexes | "which tables hold login events, and how do I join them?" |
| **API spec** (OpenAPI 3.x, Swagger 2.0) | endpoints, schemas, `$ref`s, auth | "what's the full request to create an order?" |
| **Codebase** (Go, TS/JS, Python, Rust, Java, C#, Kotlin, Swift) | functions, call graph, routes, ORM models, docs | "what breaks if I change this signature?" |

Extract once, index semantically, and the agent gets tools that answer *which tables? how do I join them? who calls this?* in a single call. On a real 113-table CRM that is ~2.3k tokens in 1 tool call instead of ~7.8k across 6 introspection queries; on a codebase one `code-query` (~3 KB) replaces a 20–50k-token Grep/Read exploration.

Codebase parsing covers nine languages with uneven depth; see [supported languages and limits](docs/codebase-graphs.md#supported-languages-and-limits).

## Install

Requires Go 1.25+.

```bash
go build -o ~/.local/bin/gatt ./cmd/gatt
```

## Quickstart

```bash
gatt extract codebase .                   # or: extract sqlite|postgres|openapi ...
gatt index                                # embed nodes (optional; keyword search works without)
gatt install                              # register the MCP server in Claude Code
```

Then ask from the terminal (the MCP tools expose the same operations):

```bash
gatt code-query "how does incremental refresh work"   # functions, callers, docs in one pack
gatt impact saveSQLite --depth 3                      # what breaks if this signature changes
gatt grep "TODO(#42)"                                 # exhaustive scan: zero hits = proof of absence
gatt query "how many messages did each client send"   # tables, columns, joins for a data question
```

Codebase graphs refresh themselves: every query re-parses only the files that changed.

## Embeddings and background refresh

Semantic search needs embeddings from **Ollama** (default, `bge-m3`), a **local ONNX worker** that runs on your CPU, or any **OpenAI-compatible** server. To keep the vectors current without slowing queries down:

```bash
(cd tools/gatt-embed && npm install)      # once; Node >= 18
gatt serve --onnx tools/gatt-embed        # refreshes graph + vectors in the background
export GATT_EMBED_URL=http://127.0.0.1:7801/v1
```

and have your agent's hook call `gatt notify` (silent; exits 0 even if nothing is listening):

```json
{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"gatt notify"}]}]}}
```

Only nodes whose text changed are re-embedded: on a 21,830-node graph the first index takes ~5 min, then an edit costs ~1.6 s. Details, backends and tuning: [docs/embeddings.md](docs/embeddings.md).

## Documentation

| | |
|---|---|
| [Command reference](docs/cli.md) | every query command with examples |
| [MCP tools](docs/mcp-tools.md) | what the agent can call |
| [Embeddings and background refresh](docs/embeddings.md) | backends, `serve`, hook setup, performance, env vars |
| [Codebase graphs](docs/codebase-graphs.md) | impact/blast/routes/models/doc-drift, the `.gatt/` overlay |
| [Annotations and freshness](docs/annotations-and-freshness.md) | curated business knowledge, drift checks |
| [Graph model](docs/graph-model.md) | node and edge types per source |
| [Architecture and layout](docs/architecture.md) | how it fits together, adding a connector |
| [Roadmap](docs/roadmap.md) | |

## License

Copyright (C) 2026 Leandro Fusco <leandrofusdev@gmail.com>

Licensed under the [GNU AGPL v3 or later](LICENSE). You may use, modify and redistribute it, including commercially; if you distribute a modified version or let users interact with one over a network (AGPL §13), you must offer them its source under the same license. Dependencies and downloaded embedding models keep their own licenses.
