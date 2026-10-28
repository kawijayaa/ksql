# search

`search` filters rows with Kusto's search predicate syntax, rewritten to an
ordinary `where` predicate:

| Predicate | Meaning |
|--|--|
| `"term"` | any column `has "term"` |
| `"term*"`, `"*term"`, `"*term*"` | any column `hasprefix`, `hassuffix`, `contains` |
| `"te*rm"` | any column `matches regex @"\bte.*rm\b"` |
| `Column:"term"` | `Column has "term"` (with the same `*` rules) |
| `*` | every row |
| `* op value` | `op` applied to every column |
| other expressions | unchanged, combined with `and`, `or` and `not()` |

Columns that are not strings are matched through `tostring`. `kind=case_sensitive`
switches string operators to their `_cs` forms; `default` and
`case_insensitive` keep Kusto's case-insensitive defaults.

A search that starts a query (`search "x"` or `search in (T1, A*) "x"`) unions
the named tables, or every table, with a `$table` column naming each row's
table. Table patterns and table-less searches need a catalog implementing
`ksql.TableLister`; at most 256 tables are searched (`KQLB0356`). `in (...)`
cannot follow a tabular input (`KQLP0356`).

`has` itself accepts terms with punctuation, such as IP addresses: the term
is matched as a phrase bounded by non-alphanumeric characters, with regex
metacharacters escaped.

Differences: Kusto's `project-smart` column selection is not reproduced, so
standalone search returns every column of the union, and a piped search does
not add `$table`.

Reference: [search operator](https://learn.microsoft.com/en-us/kusto/query/search-operator).

## find

`find [withsource=Column] [in (Tables)] where Predicate [project-smart | project Column[:Type], ... [, pack_all()]]`
(or `find Predicate ...`) searches tables like a table-less `search`, using
the same predicate rewriting, and shapes each table's rows before
concatenating them:

- The source column (`source_`, or the `withsource` name) holds the table.
- Tables without any column the predicate uses are skipped; referenced
  columns a table lacks are null.
- `project-smart` (the default) outputs the referenced columns, then the
  columns every table shares with one type, then `pack_`, a bag of each
  row's other columns from its own table.
- `project` outputs the listed columns (converted when a type is given, and
  null where a table lacks them); `pack_all()` adds `pack_` with the columns
  not listed.

`pack_` is built with the application's `bag_pack` (and `bag_merge` for more
than 32 columns); without them only `project` lists work. Columns appearing
with several types are not split into typed columns as in Kusto.
