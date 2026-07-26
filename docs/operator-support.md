# Operator support

Support means that the compiler emits executable SQL with documented semantic
behavior. Syntax recognition alone is not counted as support. Unsupported forms
return a stable diagnostic and no partial SQL.

## Recently added operators

| KQL construct | SQL targets | Current scope |
| --- | --- | --- |
| `as` | All | Reusable pipeline aliases; `hint.materialized=true` is rejected because SQL targets cannot guarantee one evaluation. |
| `sample` | PostgreSQL, SQLite, MySQL 8, SQL Server | Random ordering followed by the requested row limit. |
| `sample-distinct` | PostgreSQL, SQLite, MySQL 8, SQL Server | Distinct values from one named column, randomly limited. |
| `in~`, `!in~` | All | Case-folded comparison against a non-empty scalar value list. |
| `has` family | PostgreSQL, MySQL 8 | Non-empty alphanumeric literal terms using regex term boundaries. Dynamic terms and punctuation-separated phrases are rejected. |
| `mv-expand` | SQLite | One JSON array expression with a distinct output alias. Supports `with_itemindex`, `limit`, and `to typeof(...)`. |
| `mv-apply` | SQLite | One JSON array expression with a distinct output alias. The inner pipeline may use `where`, `extend`, and `serialize`. |

The legacy `mvexpand` spelling retains its default limit of 128 rows per input
row. Modern `mv-expand` has no implicit practical limit.

## SQLite multi-value expansion

Use a distinct output alias so the original JSON column can remain in the
result:

```kusto
Events
| mv-expand with_itemindex=i item=Items limit 10
```

```kusto
Events
| mv-apply item=Items on (
    where item > 1
    | extend doubled = item * 2
)
```

SQLite has no `SELECT * EXCLUDE` or `SELECT * REPLACE`. Consequently, a bare
replacement such as `mv-expand Items` cannot preserve the input schema without
schema metadata and returns `KQLL0333`. Multiple arrays return `KQLL0331`.
Row-reducing `mv-apply` subqueries such as `top`, `take`, or `summarize` return
`KQLL0334` because applying them independently per source row requires lateral
relation semantics that SQLite does not provide.

JSON null produces one row with a null expanded value. An empty JSON array
produces no rows. A per-row `limit` is implemented with the zero-based array
index. SQLite casts do not reproduce KQL's null-on-failure conversion, so
`to typeof(...)` emits warning `KQLL0336`.

## Term matching

PostgreSQL and MySQL 8 lower literal `has`, `has_cs`, `hasprefix`, `hassuffix`,
their negated forms, `has_any`, and `has_all` through regex boundaries based on
alphanumeric KQL terms. Stock SQLite and SQL Server have no compatible regex
operator and return `KQLL0404`.

A hyphen separates KQL terms. Use `contains` when searching for a literal
hyphenated substring:

```kusto
Events
| where RawData contains "target-string"
```

## Dialect extensions

Random ordering and JSON expansion are optional dialect capabilities. They do
not add methods to the public `dialect.Dialect` interface, so existing custom
dialects remain source-compatible. A custom dialect can opt in by implementing
`Random() (string, bool)` or `JSONEach(value, alias string) (string, bool)`.
