# Codebase graphs

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

**Never stale:** every query command (and the MCP server) checks file mtimes before answering (~60ms), re-parses only the files that changed since the last extract, evicts deleted entities, re-wires cross-file edges (calls, imports, mentions, associations, route models), and re-embeds just the changed nodes — by default only via `gatt serve` / `gatt index` (see [Semantic search and background refresh](embeddings.md)), so a query never waits on an embedding server (`GATT_EMBED_ON_REFRESH=1` restores the old inline behaviour). Wrong line numbers are worse than no graph, so you never re-run `extract` by hand mid-session. (Exceptions that refresh only on full re-extract: `CO_CHANGED` edges — git history has no incremental delta worth mining — and `CALLS_ENDPOINT` edges from files that didn't themselves change.)

## Semantic overlay (`.gatt/`)

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

## Linking API specs to source

For a swaggo/Go service, the spec and the code describe the same thing — gatt can wire them together:

```bash
gatt extract openapi swagger.json --code .   # endpoints/schemas link to their Go source:
                                             # describe_entity shows source: file:line
gatt enrich .                                # re-link an existing graph after code edits,
                                             # without re-extracting the spec
```
