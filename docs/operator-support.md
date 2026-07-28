# Operator support

Support is recorded in separate dimensions. Syntax recognition does not imply
binding, SQL lowering, or executable semantics. Unsupported forms return a
positioned diagnostic and no partial SQL.

| Construct | Syntax | Typed AST | Bound | SQL lowered | Executed dialects | Semantics |
| --- | --- | --- | --- | --- | --- | --- |
| `where`, `project`, `extend`, `distinct` | Yes | Yes | Yes with catalog | Yes | SQLite; renderer tests for PostgreSQL/MySQL/SQL Server | Equivalent for covered scalar expressions |
| `summarize` core aggregates | Yes | Yes | Yes with catalog | Yes | SQLite core; renderer tests elsewhere | Equivalent subject to database null/numeric behavior |
| `make_list` | Yes | Yes | Yes | Yes | SQLite; PostgreSQL rendering | Lossy: result order is arbitrary without an explicit database order |
| `sort`, `top`, `take`, `count` | Yes | Yes | Yes | Yes | SQLite and renderer tests | Equivalent; configured constant row bounds are enforced |
| `project-away`, `project-keep` | Yes | Expression list | Yes | Yes | SQLite | Equivalent for exact names; wildcard forms unsupported |
| `project-rename` | Yes | Named expressions | Yes | Yes | SQLite | Simultaneous, case-insensitive rename with collision checks |
| `project-reorder` | Yes | Expression list | Yes | Yes | SQLite | Equivalent for exact names; unspecified columns retain input order |
| `search` | Yes | `SearchSpec` | Yes | Capability-dependent | Yes | SQLite with UDF | Equivalent for one alphanumeric literal term; wide inputs can be bounded |
| `join kind=inner` | Yes | `JoinSpec` | Yes | Yes | SQLite | Explicit KQL output aliases; right collisions use deterministic numeric suffixes |
| `leftouter`, `rightouter`, `fullouter` | Yes | `JoinSpec` | Yes | Yes | Renderer-dependent | Database join behavior; SQLite version must support requested join |
| `leftsemi`, `leftanti` | Yes | `JoinSpec` | Yes | `EXISTS`/`NOT EXISTS` | SQLite (`leftanti`) | Equivalent for supported key predicates |
| `rightsemi`, `rightanti`, `innerunique` | Yes | `JoinSpec` | Partial | No | None | Focused unsupported diagnostic |
| `lookup` | Yes | `JoinSpec` | Yes | Yes | Renderer tests | Right same-name columns are omitted; size hints are not enforced |
| `union` | Yes | `UnionSpec` | Ordered-schema validation | Positional `UNION ALL` | SQLite | Equivalent only for equal ordered schemas; KQL outer type/name merging unsupported |
| `mv-expand` | Yes | `MvExpandSpec` | Yes | One JSON value | SQLite JSON1 | Lossy SQLite casts; arrays/objects tested, multiple expansions unsupported |
| `mv-apply` row-wise | Yes | `MvApplySpec` | Yes | `where`, `extend`, `serialize` | SQLite JSON1 | Equivalent for documented row-wise subset |
| `mv-apply` aggregating | Yes | `MvApplySpec` | No | No | None | Unsupported with `KQLL0334` |
| `parse`, `parse-where`, `parse-kv` | Yes | Opaque | No | No | None | Unsupported; no unchecked UDF lowering |
| `evaluate bag_unpack` | Yes | Opaque | No | No | None | Unsupported; schema declaration is not yet typed |
| `dcount` | Yes | Call expression | Diagnostic | No default | None | Approximate KQL semantics require an explicit compatible mapping |
| `arg_min`, `arg_max` | Yes | Call expression | Diagnostic | No | None | Schema-aware window lowering remains unsupported |

## SQLite details

SQLite dynamic string keys lower through `json_each(... WHERE key = ...)`, which
correctly addresses keys containing dots, quotes, and backslashes. Numeric
indexes lower through JSON paths of the form `$[0]`. Nested accesses retain the
input alias inside joins and JSON expansion.

SQLite union operands are rendered without invalid top-level parentheses.
Operands containing `ORDER BY`, `LIMIT`, or nested unions are wrapped as derived
queries and execute inside joins and tabular bindings.

`== null` lowers to `IS NULL`; `!= null` and `<> null` lower to `IS NOT NULL`.
JSON null expands to one null row, while an empty array expands to no rows.

Stock SQLite has no KQL-compatible regular expression implementation. Configure
an application UDF explicitly:

```go
dialect.SQLite(dialect.WithRegexFunction("kql_regex"))
```

The UDF receives `(pattern, value)`. Function names are validated as SQL
identifiers. Without this option, `search`, `has`, and regex operators return a
diagnostic; they are never lowered to `LIKE`.

## Known gaps

- Projection wildcards are not yet typed or bound.
- `innerunique`, right-side semi/anti joins, parse operators, schema-declared
  `bag_unpack`, `arg_min`, `arg_max`, and aggregating `mv-apply` are unsupported.
- Multiple-array expansion is rejected.
- KQL outer-union name/type merging is not implemented.
- SQLite `to typeof(...)` casts do not provide KQL null-on-failure semantics.
- `MaxInputRowsPerStage` and `MaxParseCaptures` are reserved until supported
  operators can enforce them without claiming execution guarantees.
