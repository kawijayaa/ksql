# Window functions and serialized row sets

KQL [window functions](https://learn.microsoft.com/en-us/kusto/query/window-functions?view=microsoft-fabric)
need a serialized row set. SQL rows have no implicit order, so KSQL records
serialization on each relation.

## Serialization tracking

| Operator | Effect |
| --- | --- |
| `sort`/`order`, `top` | Serialize. The sort keys become the recorded order. |
| `serialize` | Serializes and keeps an existing order. Without one, it freezes an arbitrary order and warns `KQLL0301`. `serialize Name=Expr, ...` extends like `extend`. |
| `where`, `take`/`limit`, `as`, `extend`, `project`, `project-away`, `project-keep`, `project-reorder`, `project-rename`, `parse`, `parse-where`, `parse-kv` | Preserve serialization, unless they redefine, rename or remove a column the order depends on. |
| All others (`summarize`, `join`, `union`, `distinct`, `mv-expand`, ...) | End serialization. |

Kusto also preserves serialization across `mv-expand`, and across operators
that change an ordering column. KSQL does not, because such a relation has no
SQL expression for its order. A window function in that position returns
`KQLB0390`.

## Lowering

Window calls may appear in `extend`, `project`, `serialize` and `where`. The
operator first adds a hidden ordinal, `ROW_NUMBER() OVER (ORDER BY <recorded
order>)`, which settles ties once. Every later stage orders by it, and the
operator's output is re-sorted by it before the hidden columns are removed.
Calls are lifted innermost first, so restart predicates such as
`prev(Airport) != Airport` work. Each stage is a streaming fenced select.

| Function | SQL |
| --- | --- |
| `prev(e[, offset[, default]])`, `next(...)` | `LAG`/`LEAD`. The offset must be a non-negative integer literal. A string value with no default gets `""` past the edge, because KQL strings have no null. |
| `row_number([start[, restart]])` | `ROW_NUMBER()` plus `start - 1`. The start must be an integer literal. |
| `row_cumsum(term[, restart])` | `SUM` with a `ROWS UNBOUNDED PRECEDING` frame. Int and long terms give long; real gives real. |
| `row_rank_dense(term[, restart])` | A running sum of "first row or term changed from the previous row". Null equals null. |
| `row_rank_min(term[, restart])` | `MIN(row number)` over rows with the same term. |

A restart predicate becomes a segment number, a running count of true values,
which partitions the window. A query function or application rule with a
window function's name keeps precedence, and its calls are not lifted.

## Diagnostics

| Code | Condition |
| --- | --- |
| `KQLB0390` | Input not serialized, or unknown schema |
| `KQLB0391` | Window call outside a supported operator (for example `summarize`) |
| `KQLB0392` | Restart predicate is not bool |
| `KQLB0393` | Wrong argument count |
| `KQLB0394` | Offset or starting index is not an integer literal |
| `KQLB0395` | Unsupported term type |
| `KQLB0396` | More than 64 window calls in one operator |
| `KQLL0390` | `row_window_session` (not yet lowered) |

## Tests and gaps

`tests/compiler/window_functions_test.go` executes Microsoft's `row_number`,
`row_cumsum`, `row_rank_dense` and `row_rank_min` examples on SQLite in both
parameterization modes, adding a tie-break key where the published order of
tied rows is arbitrary. It also covers `prev`/`next` offsets and defaults,
nesting, adjacent-row filtering in `where` and `project`, serialization through
preserving operators, `top`, `serialize`, user overrides and every diagnostic.

Open: `row_window_session`, serialization through `mv-expand` and through
reassigned ordering columns, null handling in `row_cumsum` terms, and the
`next`/`prev` default type rules.
