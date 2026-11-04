# The call graph: what a `CALLS` edge means, and what is missing

`gatt impact`, `gatt blast` and `code-query` follow `CALLS` edges. This page says
how an edge is decided, which calls get one, and which calls silently do not —
so a "no callers found" can be read for what it is.

**Rule of thumb: no edge beats a wrong edge.** A call is linked only when the
graph already contains the evidence for it. When it does not, the call is left
out and `impact` says so (the coverage warning counts files that mention the
name but have no edge).

## How a call becomes an edge

| Call | Resolved to | Edge |
|---|---|---|
| `foo()` | a function `foo` in the same file, else the only function of that name in the repo | plain |
| `this.foo()`, `self.foo()` | a function `foo` in the same file | plain |
| `X.foo()`, `util.Foo()`, `svc.foo()`, `this.svc.foo()` | see below | `inferred=true`, `via=receiver`, `receiver=<name>` |
| `ns.foo()` after `import * as ns` (JS/TS) | the export `foo` of that module | `inferred=true`, `via=namespace-export` |
| anything else (`a.b().c()`, `list.add(x)`, `getFoo().bar()`) | nothing | none |

Outside Go, a call whose receiver is not `this`/`self` is **never** matched to a
same-file function by name alone: `this.http.get()` would otherwise be linked to
an unrelated `get()` in the same class. (Go keeps the same-file match: a method's
receiver there is an arbitrary variable name, not a keyword.)

## Calls with a receiver

The receiver must be a plain name — or `this.<field>` / `self.<field>`, which can
only mean that field. It is turned into a candidate **file** using evidence that
is already in the graph, and an edge is added only when exactly one file remains:

1. **A package the caller imports** (Go): `util.Helper()` → package-level
   function `Helper` in the imported local package `util` (methods are excluded).
2. **A type visible from the caller** — defined in the same file, in the same
   directory (same package / namespace), or in a directory or file the caller
   imports (Java, Kotlin, C#, Swift, Go, Python, TS/JS): `Helper.hello()` →
   function `hello` in the file that defines `Helper`.
3. **A JS/TS named import**: `import { Foo } from './foo'` binds `Foo` to that file.
4. **A Python module bound by an import statement**: `import pkg.graph as graph`
   binds `graph` to `pkg/graph.py`. `import html` (the stdlib) binds nothing
   local, so `html.escape()` is not linked to some other `html.py`.
5. **A variable, parameter or field with a declared type**: `UserService svc;`
   then `svc.find()` → `find` in the file defining `UserService`, resolved with
   rule 2. In Go, the method is taken from the declared type's `HAS_METHOD` edges,
   so two types sharing a method name in one package do not get mixed up.

Overloads: all functions of that name in the resolved file are linked (up to 6;
more means the name is too ambiguous to be informative).

### What it will not do

- **Names are never guessed.** A repo-wide unique `add()` does not capture every
  `list.add()`; a lower-case name that is not a declared variable gets nothing.
- **Scope is not tracked.** A name is trusted only when *every* declaration of it
  in the file (parameter, local, field, loop variable) has the same type. `val data
  = compute()` next to `data: Store` in another method makes `data` ambiguous —
  neither call is linked. Unusual binding forms (pattern variables, destructuring)
  are not all enumerated, so this is a strong heuristic, not a proof.
- **No inheritance or interface dispatch.** A method inherited from a base class
  is not found on the derived type; a call through an interface links to the
  interface's declared method, not to an implementation.
- **No inference.** `var x = new Foo()`, `x := NewFoo()`, `let x = Foo::new()` have
  no declared type, so `x.bar()` is not linked.
- **Same-name types in several visible places** (two imported packages that both
  define `Helper`) are ambiguous and linked to neither.
- **Other files of the same type** (C# partial classes, Kotlin extension functions,
  Rust `impl` blocks elsewhere) are missed.

### Per language

| Language | `Type.m()` | `pkg.F()` / module | typed variable / param / field | `this.field.m()` |
|---|---|---|---|---|
| Java | yes | – | yes | yes |
| C# | yes | – | yes | yes |
| Kotlin | yes | – | yes (incl. constructor properties) | yes |
| TypeScript | yes (named imports) | `import * as` | yes | yes |
| JavaScript | yes (named imports) | `import * as` | – (no types) | – |
| Go | – | yes (imported local packages) | yes (incl. `pkg.Type`) | – |
| Python | yes (import-bound) | yes (import-bound module) | – | – |
| Swift | static calls on a type in the same directory | – | – | – |
| Rust | – | – | – | – |

## Reading the result

`impact` and `blast` print a coverage warning when a name appears in more files
than the edges account for; that is the signal that calls were left out. Inferred
edges can be told apart and audited:

```sql
-- edges added by receiver resolution, with the name they were resolved from
SELECT efrom, eto, json_extract(attrs, '$.receiver')
FROM edges WHERE etype = 'CALLS' AND json_extract(attrs, '$.via') = 'receiver';
```

## Measured on real repositories

(Receiver-resolved edges are edges that did not exist before; "audit" is a
sample of 30 per repository checked against the source: the call text must exist
in the caller, and the target is read by eye.)

| Repository | `CALLS` before | `CALLS` now | of which by receiver | Semantic audit of sampled receiver edges |
|---|---|---|---|---|
| elastic/elasticsearch (Java) | 420,321 | 630,034 | 284,322 | 400: 255 typed + 144 type-name verified, 1 undeclared, **0 mismatches** |
| microsoft/vscode (TS) | 220,652 | 272,070 | 84,961 | 400: 294 typed + 80 type-name verified, 26 module/undeclared, **0 mismatches** |
| dotnet/aspnetcore (C#) | 62,093 | 82,866 | 33,493 | 600: 420 type-name + 140 typed + 20 extension methods verified, 20 undeclared, **0 mismatches** |
| kubernetes/kubernetes (Go) | 109,667 | 122,492 | 12,827 | 400: 17 type + 381 package-function calls; 2 flagged by the auditor, checked by hand: `scheduler.WithPodMaxBackoffSeconds` on the imported package, the auditor matched a comment |
| ktorio/ktor (Kotlin) | 27,369 | 25,039 | 627 | 400: 304 typed + 96 type-name verified, **0 mismatches** |
| django/django (Python) | 39,638 | 37,496 | 1,709 | all 1,709 checked: module receivers (`timezone`, `transaction`, `management`, ...); the only 34 flagged have receiver `timezone` and the auditor matched the string "Invalid timezone" |
| facebook/react (JS) | 21,388 | 20,574 | 258 | text audit 30/30 |
| swift-package-manager | 23,187 | 21,499 | 571 | text audit 30/30 |
| ripgrep (Rust) | 991 | 612 | 0 | n/a (Rust has no receiver resolution) |

Reading the table:

- **"Before" is gatt without this change.** In five repositories the total went
  *down*: a call whose receiver is not `this`/`self` is no longer matched to a
  same-file function by name alone. Those edges were never verified, and
  `this.http.get()` linked to an unrelated `get()` in the same class is the
  typical wrong one. Where nothing replaces them (Rust, Swift typed receivers,
  untyped Python/JS variables) the repository simply has fewer edges, not more
  correct ones; how many of the dropped edges were in fact right was not measured.
- **The audit is two checks.** (1) For 30 edges per repository the call text must
  exist in the caller's body. (2) For 400-600 edges the receiver's *declared type*
  is found in the caller's source and must be defined in the target's file (or,
  for C# extension methods, appear as `this T` in the target's signature). A
  mismatch was investigated by hand every time it showed up; two real bugs were
  found that way and fixed (a Java package directory capturing a same-named
  variable; a Python parameter shadowing a module). It is a sample, not a proof.
- **Elasticsearch extraction** now takes about 4 minutes (6 GB); the earlier
  8 minutes was measured with other jobs running and is not comparable.
- **Case that motivated this:** `gatt impact UseRouting` on aspnetcore went from
  "no callers found" (166 files mention it) to 19 direct callers.

