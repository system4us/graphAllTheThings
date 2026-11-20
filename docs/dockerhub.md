# `system4us/gatt` — Central Knowledge Graph & MCP HTTP Server

All-in-one local container for [**graphAllTheThings (`gatt`)**](https://github.com/system4us/graphAllTheThings): turns your mounted repositories and document collections (PDF, DOCX, Markdown, TXT) into a live, semantically indexed knowledge graph exposed to AI agents over **Streamable HTTP MCP**.

## What's inside

- **`gatt serve`** (Go + tree-sitter for Go, TS/JS, Python, Rust, Java, C#, Kotlin, Swift + native PDF/DOCX/Markdown/TXT document extractor with `poppler-utils`)
- **Internal ONNX embedding worker** (`Xenova/paraphrase-multilingual-MiniLM-L12-v2`, CPU-only, loopback `127.0.0.1:7801` — never exposed)
- **Embedded Qdrant** (`v1.13.6`, one collection per repo, loopback `127.0.0.1:6333` — never exposed)
- **Automatic git commit watcher**: polls `.git/HEAD` (including worktrees and packed refs) and reindexes changed files and vectors incrementally on every commit, pull, merge, or branch switch.
- **Loopback security**: `gatt` rejects non-loopback `Host` headers (DNS rebinding protection) and browser `Origin` headers. Always publish port `7788` on `127.0.0.1` only.

## Quickstart (with the `gatt` CLI)

```bash
npm install -g gatt-graph
gatt docker up --mount /path/to/your/repos
gatt install --http http://127.0.0.1:7788 --scope user
```

## Quickstart (`docker run`)

Mount your repositories **read-only (`:ro`) at the exact same absolute path** inside and outside the container:

```bash
docker run -d \
  --name gatt-server \
  --restart unless-stopped \
  --ulimit nofile=65536:65536 \
  -p 127.0.0.1:7788:7788 \
  -e GATT_SCAN_DIRS=/home/you/projects \
  -v gatt-data:/data \
  -v gatt-cache:/cache/huggingface \
  -v /home/you/projects:/home/you/projects:ro \
  system4us/gatt:latest
```

On **Windows (Docker Desktop)**, map drive paths to `/host/<drive>/...`:

```powershell
docker run -d `
  --name gatt-server `
  --restart unless-stopped `
  -p 127.0.0.1:7788:7788 `
  -e GATT_SCAN_DIRS=/host/c/Users/you/projects `
  -v gatt-data:/data `
  -v gatt-cache:/cache/huggingface `
  -v C:\Users\you\projects:/host/c/Users/you/projects:ro `
  system4us/gatt:latest
```

## HTTP Endpoints (`127.0.0.1:7788`)

| Endpoint | Purpose |
|----------|---------|
| `POST/GET/DELETE /mcp` | Global multi-repo Streamable HTTP MCP (tools take `repo` + `list_repos`) |
| `POST/GET/DELETE /mcp?repo=<abs-path>` | Project-scoped Streamable HTTP MCP (auto-extracts on first access) |
| `POST /refresh?repo=<abs-path>` | Trigger background refresh + embedding (omit `repo` for all) |
| `GET /status` / `GET /repos` | Indexing progress, git `HEAD`, ONNX & Qdrant status |
| `GET /health` | Container healthcheck (`{"ok": true, ...}`) |

## Volumes

| Path | Contents |
|------|----------|
| `/data` | Per-repo SQLite graphs, annotations, and embedded Qdrant storage (`/data/qdrant`) |
| `/cache/huggingface` | Cached ONNX embedding model (~130 MB, downloaded once on first use) |

## Environment Variables

| Variable | Default | Meaning |
|----------|---------|---------|
| `GATT_SCAN_DIRS` | *(empty)* | Comma-separated mounted parent directories to auto-discover git repos in (rescans every 60 s) |
| `GATT_SCAN_DEPTH` | `2` | Max directory depth under `GATT_SCAN_DIRS` to look for `.git` repos |
| `GATT_REPOS` | *(empty)* | Comma-separated specific repo or document folder paths to preload on startup (auto-detects `codebase` vs `docs`) |
| `GATT_WATCH_INTERVAL` | `5s` | Poll interval for git commit detection (`0` disables) |
| `GATT_EMBED_CACHE` | `/cache/huggingface` | ONNX model download cache directory |

## Tags

- `latest` — latest stable release (`linux/amd64`, `linux/arm64`)
- `X.Y.Z`, `X.Y` — pinned releases

## Source & License

- GitHub: https://github.com/system4us/graphAllTheThings
- npm CLI: https://www.npmjs.com/package/gatt-graph
- License: **GNU AGPL v3 or later** (Copyright © 2026 Leandro Fusco)
