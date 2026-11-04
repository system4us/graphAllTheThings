# Command reference

Query from the terminal (same operations the MCP tools expose):

```bash
gatt query "how many messages did each client send this month"
                                          # context pack: tables, columns, enums, joins
gatt code-query "how does incremental refresh work"
                                          # context pack: functions (signature, file:line,
                                          # callers/callees, doc), types, docs
gatt impact saveSQLite --depth 3          # transitive callers: what breaks on a signature
                                          # change; test callers tagged [test]
gatt blast shared/schemas/product.json    # blast radius of a file: callers + importers +
                                          # generated copies + diverged duplicates
gatt search "user login timestamps"       # hybrid search over all nodes
gatt grep "TODO(#42)" --regex             # exhaustive literal/regex scan of every file —
                                          # a zero-result answer proves absence; search
                                          # above is semantic/top-N and is NOT exhaustive
gatt tree internal/engine --depth 2       # directory tree annotated with each file's doc
gatt routes                               # HTTP routes found in code: method, path, handler,
                                          # middleware, models touched, frontend call sites
gatt models                               # ORM models found in code: DB table, field→column
                                          # renames, associations (explicit + FK-name inferred)
gatt doc-drift                            # docs whose code references broke (deleted/renamed
                                          # symbols, moved files) or went stale (code changed
                                          # after the doc's last commit)
gatt diff HEAD~5                          # structural diff vs a git ref: added/removed/
                                          # changed/renamed/moved functions & types
gatt path clients conversation_messages   # FK join path with exact columns
gatt explain messages                     # one node: attrs + relationships
gatt overview                             # all tables (or files/components), counts, references
```
