# MCP tools

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
