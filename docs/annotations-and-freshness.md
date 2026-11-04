# Annotations and freshness

## Curated annotations

The schema knows structure, not business meaning: whether `enabled=false` rows count as real contacts, what a "contact" canonically is. Encode that once and every `sql_context`/`describe_entity` response carries it, so the agent writes the right query instead of asking:

```bash
gatt annotate contacts \
  default_filter="enabled = true" \
  entity_note="CRM contacts; enabled=false is a draft; source='google' is imported"
gatt annotate contacts --clear            # remove
```

Annotations live in `gatt-out/annotations.json` (a sidecar keyed by node id), merged over the graph on every load — so they survive re-running `extract`. Recognized keys: `default_filter` (a canonical `WHERE` clause, rendered like the auto-detected soft-delete filter) and `entity_note` (free-text definition). Any other key is stored and shown too. `sql_context` also emits the SQL `dialect:` up front so generated SQL is dialect-correct without inference.

## Keeping it fresh

The graph is a snapshot, so agents need to know how old it is and you need a cheap way to tell when the source has drifted:

```bash
gatt extract postgres "$DSN" --check   # re-read the source, print schema drift, DO NOT write
gatt extract postgres "$DSN"           # re-extract; prints the same drift, then writes
gatt index                             # re-embed only the nodes whose text changed (--full to force all)
```

- **Every extraction stamps `extracted_at`.** `sql_context` and `graph_overview` lead with a `# source: postgres:app, extracted 3h ago (2026-07-13)` line, so the agent can judge staleness and re-verify against the live DB when it matters — instead of trusting a snapshot blindly.
- **`--check` is the drift probe.** It hits the source and diffs against the current graph (added/removed tables & columns, changed types, FK count deltas) without touching the graph. Run it on a schedule or in CI; a non-empty drift report means it's time to re-extract and re-index.
- **Codebase graphs refresh themselves.** `code-query`, `impact`, and the MCP server detect changed files by mtime and re-parse only those before answering — no manual re-extract.
- **Re-extraction is non-destructive to curated knowledge.** Annotations live in their sidecar and are re-applied on load; if a re-extract removes a node an annotation targeted, you get a warning naming the orphaned annotation. The `.gatt/` overlay is merged fresh on every extract.
- **Re-indexing is incremental.** `index` reuses cached vectors for nodes whose embedding text is unchanged (content-hashed) and only embeds what actually moved — so re-indexing a 113-table CRM after a one-column migration embeds two nodes, not two thousand.
