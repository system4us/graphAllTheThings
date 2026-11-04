# Semantic search and background refresh

Queries refresh the *graph* (cheap) but never wait on an embedding server. Vectors are built by `gatt index`, or kept up to date in the background by `gatt serve`.

## Choose an embedding backend

| Backend | How | Notes |
|---|---|---|
| **Ollama** (default) | `ollama pull bge-m3`; nothing else to configure | multilingual `bge-m3`, `http://localhost:11434` |
| **Local ONNX worker** | `gatt serve --onnx tools/gatt-embed` | runs on CPU, no external service; model downloaded on first use (~130 MB) |
| **Any OpenAI-compatible server** | `--embed-url http://host:port/v1` | Text Embeddings Inference, llama.cpp, LM Studio, …; optional token in `GATT_EMBED_API_KEY` |

The vectors themselves are stored locally (default) or in Qdrant (`--qdrant URL`). The model name is stored with them; a different model is refused rather than silently replacing the index (`gatt index --full` to switch).

## Set it up (local ONNX)

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

## What it costs (Sagpic, 21,830 nodes, 12 cores)

| | |
|---|---|
| First index | ~5 min (~60 nodes/s); Qdrant 5 min 24 s |
| Hook pass, nothing changed | ~35 ms |
| Hook pass, one file edited | ~1.6 s (Qdrant: the same) |
| Re-extracting an unchanged repo | 0 nodes re-embedded |

## How it works

- **`serve`** listens on `127.0.0.1:7788` (`POST /refresh`, `GET /status` with embedding progress), rejects any non-loopback `Host` and any `Origin` header, and coalesces a burst of hooks into one extra pass.
- **Incremental by content.** A node is re-embedded only if its text hash changed. The text lists neighbours (callers, references, …) sorted, so edge order — which differs between extractions and after an incremental refresh — never causes re-embeds. Nodes that left the graph are removed from the index. Upgrading from vectors built before this change re-embeds the affected nodes once.
- **The worker** (`tools/gatt-embed`: `@huggingface/transformers` + ONNX Runtime) exposes an OpenAI-compatible `/v1/embeddings`. Texts are truncated to 128 tokens (what the default multilingual MiniLM, 384-d, was trained on) and embedded in length-sorted sub-batches, because padding to the longest text made one long node slow its whole batch ~7×.
- **Local index format.** `vectors.json` holds only metadata (3 MB for 21.8k nodes) and `vectors.bin` the raw float32 vectors (33 MB; it used to be one 106 MB JSON). A generation id ties the two files together, so a reader that catches a write in progress retries instead of mixing them. Older all-in-JSON files are still read and upgraded on the next write; older gatt *binaries* cannot read the new layout.
- **Qdrant** is incremental too: a pass reads the stored text hashes (paged), uploads only changed nodes and deletes the ones that left the graph. `gatt index --full` recreates the collection.
- **"Up to date"** is a stamp of the graph file (size + mtime) written when a catch-up finishes, taken before the graph is read — not a comparison of file times — so a changed clock, a restored older copy, or a change during embedding can't make `serve` skip a needed pass.
- **Concurrent processes.** Graph refresh+save and the vector-index write each take a lock file (`<graph>.lock`, `vectors.json.lock`; considered stale after 5 min), so `serve` and query commands can run together without losing writes.

## Environment variables

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
