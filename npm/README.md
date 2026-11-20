# gatt-graph (`gatt`)

Turn codebases, document collections (PDF, DOCX, Markdown, TXT), databases (SQLite, PostgreSQL), and OpenAPI specs into a knowledge graph that AI agents query via **MCP** (Streamable HTTP or stdio) or the CLI — replacing 20–50k-token grep/read loops with a single compact call.

## Install

```bash
npm install -g gatt-graph
```

Installs the `gatt` command by downloading the prebuilt native binary for your platform (`linux`, `darwin`, `win32`; `x64`/`arm64`) from the matching GitHub release and verifying its SHA-256 checksum.

## Quickstart with Docker (Recommended)

Run one local container ([`system4us/gatt`](https://hub.docker.com/r/system4us/gatt)) that holds graphs and semantic vectors for **all your repositories** using a shared local **ONNX** embedder and an embedded **Qdrant** vector store — strictly bound to `127.0.0.1:7788`:

```bash
# Start the central server and auto-discover all git repos under your code folder(s)
gatt docker up --mount /path/to/your/repos

# Register ONCE globally in Claude Code / Antigravity for every repo:
gatt install --http http://127.0.0.1:7788 --scope user

# Or register per-project inside the current repo's .mcp.json:
gatt install --http http://127.0.0.1:7788
```

- Multiple roots & document collections: `gatt docker up --mount /path/a --mount /path/docs` (or `--repos /path/to/corpus` for PDF/DOCX/MD/TXT folders; supports `.gattignore`).
- Automatic commit watcher: detects `git commit`, `pull`, `merge`, `rebase`, and `checkout` and reindexes incrementally in the background.
- Windows (Docker Desktop): drive paths (`C:\Users\you\code`) are mapped automatically to `/host/c/Users/you/code`.
- Container management: `gatt docker status`, `gatt docker logs -f`, `gatt docker pull`, `gatt docker down`.

## Standalone CLI / stdio MCP (no Docker)

```bash
gatt extract codebase .                   # or: gatt extract docs /path/to/pdfs-and-docx
gatt install                              # register stdio MCP server
gatt code-query "how does auth work"      # functions, callers, types, docs in one pack
gatt doc-query "how to configure SSO"     # hybrid RAG over PDF/DOCX/MD/TXT with Confidence: high|medium|low
gatt doc-guide "configure Slack token"    # full canonical doc pack (reading order, screenshots, variants) for user guides
gatt impact MyFunc --depth 3              # transitive callers before changing a signature
gatt blast shared/schema.json             # callers, importers, co-change companions
gatt grep "TODO" --limit 0                # exhaustive literal/regex scan (--include/--exclude)
gatt routes                               # HTTP routes → handlers → ORM models → clients
```

## Documentation & Source

Full documentation, supported languages, and source code:
- Repository: https://github.com/system4us/graphAllTheThings
- Docker image: https://hub.docker.com/r/system4us/gatt
- Embeddings & Docker setup: https://github.com/system4us/graphAllTheThings/blob/main/docs/embeddings.md
- MCP tools reference: https://github.com/system4us/graphAllTheThings/blob/main/docs/mcp-tools.md
- CLI reference: https://github.com/system4us/graphAllTheThings/blob/main/docs/cli.md

## License

GNU AGPL v3 or later — Copyright (C) 2026 Leandro Fusco.
