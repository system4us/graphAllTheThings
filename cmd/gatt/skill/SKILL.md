---
name: gatt
description: "Query a codebase through its gatt metadata graph instead of grep/read exploration. Use whenever (1) the user asks anything about a codebase, (2) you are about to run multiple Grep/Glob/Read rounds to orient yourself, or (3) you are in planning mode and need to map the code a plan will touch. Trigger check: `gatt-out/graph.db` or `gatt-out/graph.json` exists in the repo root, or `gatt` is on PATH."
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
  gatt index                                        # optional: semantic search (needs local embedder)
  ```

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
gatt grep <pattern> [--regex]       # exhaustive literal/regex scan of every file — a
                                    # zero-result answer is proof of absence; search
                                    # above is semantic/top-N and is NOT exhaustive
gatt tree [path] [--depth N]        # directory tree annotated with each file's doc summary
gatt routes [--file substr]         # HTTP routes found in code (Express JS/TS/JSX):
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
gatt explain <node-id>              # one node in full: attrs + every edge
gatt overview                       # node counts, project shape
```

All accept `--graph PATH` (auto-detects `gatt-out/graph.db` → `graph.json`).

## Workflow

1. `gatt code-query "<the question or feature description>"` — get the map.
2. Read **only** the line ranges the pack points at (`Read` with offset/limit),
   not whole files. Short functions already include their body — skip the Read.
3. Editing? `gatt impact <function>` first; check `[test]` entries.
4. After your edits, nothing to do — the next query auto-refreshes the graph
   and re-embeds changed nodes.

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
`code_context`, `impact`, `blast`, `find_entities`, `describe_entity`, `grep`, `tree`,
`routes`, `models`, `code_diff` mirror the commands above with the engine resident in
memory (faster on huge repos). Prefer whichever is already available; functionality is identical.
