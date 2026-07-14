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
  gatt extract codebase . --out gatt-out/graph.db   # .db = SQLite + FTS5, preferred
  gatt index                                        # optional: semantic search (needs local embedder)
  ```

## Core commands

```bash
gatt code-query "<question>"        # FIRST CALL for any code question: relevant
                                    # functions (signature, file:line, callers/callees,
                                    # doc, body if short), types with methods, docs
gatt impact <func-name-or-id>       # transitive callers to depth N (--depth 3 default)
                                    # ALWAYS before signature/behavior changes;
                                    # [test] tags show affected tests
gatt search "<text>" [--type function|definition|file]   # locate by name/keyword/semantics
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
- No hits ≠ doesn't exist: names invisible to tree-sitter queries (dynamic dispatch,
  reflection) won't be in the call graph. Fall back to Grep for those.

## MCP alternative

If the `gatt` MCP server is installed in the client (`mcp__gatt__*` tools visible),
`code_context`, `impact`, `find_entities`, `describe_entity` mirror the commands
above with the engine resident in memory (faster on huge repos). Prefer whichever
is already available; functionality is identical.
