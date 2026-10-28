# Operator support

Support is recorded in separate dimensions. Syntax recognition does not imply
binding, SQL lowering, or executable semantics. Unsupported forms return a
positioned diagnostic and no partial SQL.

| Construct | Syntax | Typed AST | Bound | SQL lowered | Executed dialects | Semantics |
| --- | --- | --- | --- | --- | --- | --- |
| `where`, `project`, `extend`, `distinct` | Yes | Yes | Yes with catalog | Yes | SQLite; renderer tests for PostgreSQL/MySQL/SQL Server | Equivalent for covered scalar expressions |
| `summarize` core aggregates | Yes | Yes | Yes with catalog | Yes | SQLite core; renderer tests elsewhere | Equivalent subject to database null/numeric behavior |
| `make_list` | Yes | Yes | Yes | Yes | SQLite; PostgreSQL rendering | Lossy: result order is arbitrary without an explicit database order |
| `getschema` | Yes | Operator | Bound schema | Inline value table | SQLite; renderer tests elsewhere | Standard four-column schema; rejects unknown types and unsupported parameters |
| `sort`, `top`, `take`, `count` | Yes | Yes | Yes | Yes | SQLite and renderer tests | Equivalent; configured constant row bounds are enforced; `sort` and `top` record serialization |
| `serialize`, window functions | Yes | Expression list | Bound schema | Hidden ordinal and `LAG`/`LEAD`/`ROW_NUMBER`/cumulative windows | SQLite | `prev`, `next`, `row_number`, `row_cumsum`, `row_rank_dense`, `row_rank_min` with restarts; see [window guide](window-functions.md) |
| `project-away`, `project-keep` | Yes | Expression list | Yes | Yes | SQLite | Exact names and case-sensitive `*` patterns; input column order retained |
| `project-rename` | Yes | Named expressions | Yes | Yes | SQLite | Simultaneous, case-insensitive rename with collision checks |
| `project-reorder` | Yes | Expression list | Yes | Yes | SQLite | Exact names and `*` patterns; ordinal and natural numeric sorting; first match wins; unspecified columns retain input order |
| `search` | Yes | `SearchSpec` | Yes | Capability-dependent | Yes | SQLite with UDF | Equivalent: terms with `*` placement (has, hasprefix, hassuffix, contains, regex), `Column:"term"`, `*`, Boolean combinations, `kind=case_sensitive`, and standalone `search [in (Tables)]` over a `TableLister` catalog with `$table`; see docs/search.md |
| `join kind=inner` | Yes | `JoinSpec` | Yes | Yes | SQLite | Explicit KQL output aliases; right collisions use deterministic numeric suffixes |
| `leftouter`, `rightouter`, `fullouter` | Yes | `JoinSpec` | Yes | Yes | Renderer-dependent | Database join behavior; SQLite version must support requested join |
| `leftsemi`, `leftanti` | Yes | `JoinSpec` | Yes | `EXISTS`/`NOT EXISTS` | SQLite (`leftanti`) | Equivalent for supported key predicates |
| `rightsemi`, `rightanti` | Yes | `JoinSpec` | Yes | `EXISTS`/`NOT EXISTS` | SQLite | Right-only schema; right duplicates preserved; null keys do not match |
| `innerunique` (default join) | Yes | `JoinSpec` | Bound left schema | Partitioned `ROW_NUMBER` then inner join | SQLite; other dialect renderer checks | One complete left representative per equality-key tuple; representative is unspecified |
| `lookup` | Yes | `JoinSpec` | Yes | Yes | Renderer tests | Right same-name columns are omitted; size hints are not enforced |
| `union` (pipeline or standalone) | Yes | `UnionSpec` | Bound schemas | Name/type-aligned `UNION ALL` | SQLite | Outer null padding and type suffixes with collision avoidance; inner intersects name/type pairs; preserves duplicates |
| `mv-expand` | Yes | `MvExpandSpec` | Yes | One JSON value, or several in lockstep | SQLite JSON1 | Lossy SQLite casts; several columns expand by index with null padding, bags as single-property bags (or `kind=array` pairs) |
| `mv-apply` row-wise | Yes | `MvApplySpec` | Yes | `where`, `extend`, `serialize`, `mv-expand`, `parse`, `sort`, `project*` | SQLite JSON1 | Equivalent |
| `mv-apply` aggregating | Yes | `MvApplySpec` | Yes | `summarize`, `count`, `distinct`, `take`, `top` per input row | SQLite JSON1 and window functions | Equivalent; input columns are joined back by a hidden row number; other operators report `KQLL0334` |
| `parse`, `parse-where` | Yes | `ParseSpec` | Bound schema | One anchored RE2 capture array per row, then typed conversions | SQLite with capture UDF | `simple`, `regex` (with `i`/`m`/`s`/`U` flags) and `relaxed` kinds; strict conversion gating; see [parse guide](parse-operator.md) |
| `render` | Yes | `RenderSpec` | Final schema | Annotation only; rows unchanged | All | Validated `Result.Visualization`; must be last; see [render guide](render-operator.md) |
| `parse-kv` | Yes | `ParseKVSpec` | Bound schema | One key-value extraction per row via `runtime.KeyValuesJSON`, then typed conversions | SQLite with registered function | Delimited, free and regex modes with quotes, escapes and greedy values; see [parse-kv guide](parse-kv-operator.md) |
| `evaluate bag_unpack` | Yes | Opaque | No | No | None | Unsupported; schema declaration is not yet typed |
| `dcount` | Yes | Call expression | Diagnostic | No default | None | Approximate KQL semantics require an explicit compatible mapping |
| `take_any`, `any`, `take_anyif`, `anyif` | Yes | Call/tuple assignment | Bound schema | Materialized inputs, per-call rank | SQLite; PostgreSQL rendering | Coherent arbitrary record, presence preference for single expressions, conditional eligibility, independent sibling aggregates |
| `arg_min`, `arg_max`, `argmin`, `argmax` | Yes | Call/tuple assignment | Bound schema | Materialized inputs, per-call rank, selected-value aggregation | SQLite; PostgreSQL rendering | Coherent row per selector, null keys last, independent sibling aggregates; see row-selection guide |

## SQLite details

SQLite dynamic string literals without double quotes or backslashes lower
through `json_extract` with quoted labels such as `$."a.b"` and
`$."space key"`. Consecutive safe properties collapse into one path. Keys with
double quotes or backslashes, and indexes rendered as parameters, retain the
`json_each(... WHERE key = ...)` fallback. Numeric indexes lower through JSON
paths of the form `$[0]`. Nested accesses retain the input alias inside joins
and JSON expansion.

SQLite union operands are rendered without invalid top-level parentheses.
Operands containing `ORDER BY`, `LIMIT`, or nested unions are wrapped as derived
queries and execute inside joins and tabular bindings.

`== null` lowers to `IS NULL`; `!= null` and `<> null` lower to `IS NOT NULL`.
JSON null expands to one null row, while an empty array expands to no rows.

Stock SQLite has no KQL-compatible regular expression implementation. Configure
an application UDF explicitly:

```go
dialect.SQLite(
    dialect.WithRegexFunction("kql_regex"),
    dialect.WithRegexCaseInsensitiveFlag("(?i)"),
)
```

The UDF receives `(pattern, value)`. Function names are validated as SQL
identifiers. The case-insensitive flag is optional and must be supported by the
configured regex engine. When set, it prefixes case-insensitive patterns rather
than lowercasing either operand; callers that omit it retain operand
lowercasing. Without `WithRegexFunction`, `search`, `has`, and regex operators
return a diagnostic; they are never lowered to `LIKE`.

## Known gaps

- Column-selection patterns are supported by `project-away`, `project-keep`, and `project-reorder`. Zero-column outputs remain unsupported; exact-name binding remains case-insensitive. Numeric sorting is verified for ASCII digit runs; Unicode digit collation still needs conformance coverage.
- Schema-declared `bag_unpack` is unsupported; aggregating `mv-apply` needs a bound input schema (`KQLB0339`).
- Union wildcard sources, `withsource`, fuzzy resolution, and zero-column results remain unsupported. SQL backends cannot represent columns differing only in case. The existing type model still conflates decimal/real and guid/string; those distinctions remain pending.
- SQLite `to typeof(...)` casts do not provide KQL null-on-failure semantics.
- `MaxInputRowsPerStage` is reserved until supported operators can enforce it
  without claiming execution guarantees. `MaxParseCaptures` bounds the
  capture groups of one `parse` pattern, including groups in regex constants.

SQLite inline `datatable` values use a derived VALUES relation with explicit column aliases. Empty value tables preserve their declared columns and return no rows. `getschema` reports the bound schema without reading input rows; `kind=csl` remains unsupported.

## Join keys and lookup columns

Join `on` conditions bind common column names or `$left.column ==
$right.column` pairs, combined with commas or `and`. Bracketed quoted columns
work in both forms. Explicit operands must reference actual columns on the
specified side; computed expressions, reversed sides, and dynamic explicit
keys return diagnostics. Project a scalar expression before using it as a key.
Legacy Boolean `join where` conditions remain supported for the join flavors
that can lower them. Lookup supports inner/leftouter and omits only its
right-side matching key columns, preserving other same-name columns with
generated names. Lookup currently requires known schemas for correct column
omission. Key-count limits include conjunctions, and join output width is
checked against the configured projection limit. Complete hints/parameters,
case resolution, unknown schemas, dynamic shorthand keys, and engine
differential conformance remain open.

## Cached pipeline aliases

`as hint.materialized=true Name` caches its input once for later references,
using the same binding mechanism as `materialize()` and `toscalar()`. Without
the hint, or with `false`, references recalculate the input. Guaranteed caching
requires SQLite 3.35+ or PostgreSQL 12+; PostgreSQL currently has rendering
checks only. See [materialized tables](materialized-tables.md).

See [row-selection aggregates](row-selection.md) for examples, tuple naming,
execution requirements and remaining conformance work.
