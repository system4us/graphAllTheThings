---
name: gatt
description: "Query a codebase or document collection (PDF, DOCX, Markdown, TXT) through its gatt semantic graph instead of grep/read exploration. Use whenever (1) the user asks anything about a codebase or a mounted docs corpus, (2) you are about to run multiple Grep/Glob/Read rounds to orient yourself, or (3) you are in planning mode and need to map the code or specs a plan will touch. Trigger check: `gatt-out/graph.db` or `gatt-out/graph.json` exists, `gatt` is on PATH, or the `gatt` MCP server is connected."
---

# gatt — graph-first codebase navigation

One `gatt code-query` call (~3 KB) replaces 20-50k tokens of grep/read exploration
and returns signatures, exact `file:line` locations, call graphs, and doc comments.
The graph auto-refreshes on every query (mtime drift check, ~60ms), so results are
never stale even mid-editing-session.

## When to reach for it

- Any natural-language question about a codebase ("how does X work", "where is Y handled")
- **Before** starting a multi-search exploration — if you are about to chain
  Grep → Glob → Read to find something, run one `code-query` instead
- **Planning phase**: map every function/type a plan will touch before writing the plan;
  quote the `file:line` locations in the plan itself
- **Before changing any function signature or behavior**: run `impact`
- **Before editing shared config/schema/data files** (JSON, YAML, SQL): run `blast`
  — it finds importers, generated copies, and diverged duplicates

## When NOT to use it

- You already know the exact file and line — just Read it
- The repo has no graph and is tiny (< ~30 files) — grep is equivalent there

## Setup check (once per session)

```bash
ls gatt-out/graph.db gatt-out/graph.json 2>/dev/null   # graph exists?
```

- Graph exists → query it directly; freshness is handled automatically.
- No graph but repo is medium/large → offer to build one (do not build unasked
  on huge repos; extraction can take minutes):
  ```bash
  gatt extract codebase .                           # → gatt-out/graph.db (SQLite + FTS5, default)
  gatt index                                        # optional: semantic search (see Embedding setup below)
  ```

### Embedding setup (optional, enables semantic search)

- **Docker central server (recommended):** runs MCP HTTP + local ONNX + embedded Qdrant on `127.0.0.1:7788` with automatic git repo discovery and commit watching:
  ```bash
  gatt docker up --mount /path/to/your/repos
  gatt install --http http://127.0.0.1:7788 --scope user   # global across all repos
  ```
  When using the global HTTP MCP endpoint (`/mcp` without `?repo=`), pass `repo` set to the current repository's absolute path (or unique folder name) to any tool, or call `list_repos`.
- **Local ONNX worker (CPU, no Ollama):**
  ```bash
  (cd tools/gatt-embed && npm install)
  gatt serve --onnx tools/gatt-embed
  export GATT_EMBED_URL=http://127.0.0.1:7801/v1
  ```
- **Ollama (default URL `http://localhost:11434`):** requires an *embedding* model (`ollama pull bge-m3`), not a chat model.

## Core commands

```bash
gatt code-query "<question>"        # FIRST CALL for any code question: relevant
                                    # functions (signature, file:line, callers/callees,
                                    # doc, body if short), types with methods, docs
gatt impact <func-name-or-id>       # transitive callers to depth N (--depth 3 default)
gatt blast <file-or-func-or-id>     # blast radius of ANY node, incl. JSON/YAML/SQL/CSS
                                    # data files: callers + file importers + regenerated
                                    # outputs (GENERATES) + same-basename copies flagged
                                    # [identical]/[diverged] + git co-change companions
                                    # (files that historically ship in the same commits:
                                    # stylesheets, docs, e2e tests, i18n — edges no parser
                                    # can see); warns if target is generated.
                                    # ALWAYS before editing shared config/schema files
                                    # ALWAYS before signature/behavior changes;
                                    # [test] tags show affected tests
gatt search "<text>" [--type function|definition|file]   # locate by name/keyword/semantics
gatt grep <pattern> [--regex] [--limit N] [--include glob] [--exclude glob] [--all]
                                    # exhaustive literal/regex scan of every file — a
                                    # zero-result answer is proof of absence; search/code-query
                                    # above are semantic/top-N and are NOT exhaustive.
                                    # Default cap is 50 (warns on stderr when truncated):
                                    # pass `--limit 0` for unlimited results during audits.
                                    # Lockfiles are skipped by default (`--all` includes them).
gatt tree [path] [--depth N]        # directory tree annotated with each file's doc summary
gatt routes [--file substr]         # HTTP routes found in code (Express, Spring/ASP.NET,
                                    # Go gin/chi/echo/net/http, Flask/FastAPI, and any
                                    # call passing a whole "METHOD /path" literal, so
                                    # custom registration wrappers count):
                                    # method, path, handler (incl. controller.method
                                    # refs), middleware chain, plus per route: the ORM
                                    # models its handler chain touches and the frontend
                                    # call sites that hit it (axios/fetch/template
                                    # paths matched by method + path tail). Full-stack
                                    # chain: frontend fn → route → models; the reverse
                                    # (fn → "Calls backend routes: …", model → "routes
                                    # touching") shows in search/describe/blast
gatt models [--file substr]         # ORM models found in code: name, DB table,
                                    # field→column renames, associations. Detects
                                    # Sequelize init/define, TypeORM decorators, Go
                                    # DB struct tags, Django/SQLAlchemy classes;
                                    # *_id/…Id fields infer REFERENCES edges in any
                                    # language. Unknown ORM base classes: declare in
                                    # .gatt/models.json {"base_classes": [...]}, or
                                    # tag a type via annotate model_table=<table>
gatt diff [ref] [--limit N]         # structural diff vs a git ref (default HEAD):
                                    # added/removed/changed/renamed/moved functions & types,
                                    # plus current callers of anything that changed
gatt trace <from> <to> [--depth N]  # shortest execution/data-flow path between two nodes
                                    # (HTTP routes, functions, event emitters/listeners,
                                    # sqlc queries, ORM models, files); if none, the
                                    # reversed path, else the common caller / shared
                                    # dependency — labelled, never routed through tests
gatt affected-tests [target]        # minimal test files + ready-to-run commands impacted
                                    # by a file, symbol, or current git diff (omit target)
gatt hotspots [--limit N]           # rank files by recent git churn × graph connectivity
gatt preflight [ref]                # composite post-edit gate: risk alerts (generated files,
                                    # hotspots, gotchas/invariants), diff, affected tests, docs
gatt doc-drift                      # docs with broken or stale code references (candidate list by git commit date)
gatt doc-query "<question>"         # FIRST CALL for document Q&A / helpdesk (PDF, DOCX, MD, TXT) —
                                    # returns ranked passages with citations and Confidence: high|medium|low
gatt doc-guide "<topic>"            # FIRST CALL for authoring a user guide — returns top 1-2 full canonical
                                    # documents in reading order, referenced screenshots, dates & related variants
gatt explain <node-id>              # one node in full: attrs + every edge
gatt overview                       # node counts, project shape
```

## Documents (`doc_context`, `doc_guide` & `doc_drift`)

- **Q&A / Helpdesk (`doc_context` / `gatt doc-query`):** use on `[docs]` repos (e.g. `repo: "corpus"`). Returns ranked passages in reading order per file, with `Confidence: high | medium | low`. When `Confidence: low`, it returns 0 passages and lists missing terms — do not fabricate missing facts.
- **Drafting user guides (`doc_guide` / `gatt doc-guide`):** instead of fragmented chunks, returns the top 1–2 canonical documents **in full reading order**, lists referenced screenshot filenames (`Screenshots referenced:`), modification dates, and separates related/variant documents (e.g. Slack `direct` vs. `token`) so procedures don't get mixed.
- **Markdown docs inside a codebase repo:** `code_context` already surfaces relevant repo docs alongside code; calling `doc_context` on a codebase repo falls back to searching the repo's indexed markdown/prose files (`README.md`, `docs/*.md`, etc.).
- **`doc_drift`:** compares git commit dates of referenced code vs. the doc file (not semantic content). Use it as a **candidate checklist** of docs to review; note that uncommitted doc edits still appear stale until committed.

All accept `--graph PATH` (auto-detects `gatt-out/graph.db` → `graph.json`).

## Workflow

1. `gatt code-query "<the question or feature description>"` (with `--max-tokens` if needed) — get the map.
2. Use `read_symbols` (MCP) to batch-read full bodies of 1–5 target symbols in one call, or `Read` with offset/limit on the exact line ranges the pack points at.
3. Tracing a flow across routes, events, or sqlc queries? Run `trace_path` (`gatt trace <from> <to>`).
4. Editing? Run `impact` / `blast` (and `file_history` on unfamiliar/hotspot files) first.
5. After edits, run `preflight_check` (`gatt preflight`) or `affected_tests` to verify changes and catch generated-file / doc-drift / invariant alerts.
6. Learned a subtle invariant or pitfall? Persist it with `annotate_entity` (`invariant`, `gotcha`, `entity_note`) so future sessions inherit it.

## Interpreting output

- `called by:` / `calls:` list resolved local calls only; external stdlib calls
  are omitted by design. `… +N more` means a hub — consider `impact` for the full set.
- `# source: codebase:…, extracted <when>` header shows freshness; a
  `graph auto-refreshed: …` stderr line means the graph just caught up with your edits.
- Generated code (protobuf, ANTLR, minified) is excluded from context packs but
  still findable via `gatt search`.
- No hits on `search` ≠ doesn't exist — it's semantic/top-N, not exhaustive. To prove
  a string occurs nowhere in the codebase (or find dynamic-dispatch/reflection names
  invisible to tree-sitter queries), use `gatt grep` instead of an ad hoc Grep call —
  same exclusion rules as extraction (including the repo's own `.gitignore` when it's
  a git checkout), and it walks every remaining file, not just indexed ones.
- In a git checkout, extraction respects the repo's own `.gitignore`, not just a fixed
  list of common build-output names — a project-specific output directory doesn't
  flood the graph with ambiguous bundled/minified copies of every real function.
- Functions carry a `shares mutable state:` section in `impact`/`blast` when they
  read/write a property on a cross-file singleton (e.g. `config.session` set in one
  file, read in another) — a data-flow signal CALLS can't see. Heuristic, JS/TS/JSX
  only, one hop (not transitive).
- Substantive floating comments (the "why" next to a block, not attached to any
  function/type) are their own `comment` node — findable via `search`/`code_context`,
  not just doc comments on declarations.
- `blast` regenerated outputs come from `"generates"` declarations in
  `.gatt/relations.json` (`[{"from": "path", "to": "path"}]`) — when you discover a
  script that copies/generates files, declare the pipeline there so future blasts see it.
- Import edges cover relative specifiers AND tsconfig path aliases
  (`@modules/...` via `compilerOptions.paths`); bare package imports stay external.
- Graphs record an absolute repo root: commands work from any cwd via `--graph`.
  A graph extracted by an older gatt (relative root) refuses to refresh from the
  wrong cwd — re-extract once to upgrade it.

## MCP alternative

If the `gatt` MCP server is installed in the client (`mcp__gatt__*` tools visible),
`code_context`, `read_symbols`, `trace_path`, `impact`, `blast`, `affected_tests`,
`preflight_check`, `hotspots`, `file_history`, `find_entities`, `describe_entity`,
`grep`, `tree`, `routes`, `models`, `code_diff`, `doc_drift`, and `annotate_entity`
mirror the commands above with the engine resident in memory (faster on huge repos).
Prefer whichever is already available; functionality is identical.
