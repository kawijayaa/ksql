# top-nested and top-hitters

`T | top-nested [N] of [Name =] Expr by [Name =] Aggregation [asc|desc], top-nested ...`
returns, for each level, the top `N` values (all values without `N`) of
`Expr` by `Aggregation` within each kept value of the previous levels. Each
level summarizes by the values so far, ranks rows within their parent group
with `row_number()`, and keeps rows whose parent the previous level kept.
Output columns are each level's value and aggregation, named after the
expression and `aggregated_<value>` unless named. `with others` is not
supported (`KQLB0362`): it needs an aggregate over the records outside the
top values, which non-decomposable aggregations cannot derive from groups.

`T | top-hitters N of Value [by Sum]` is computed exactly as
`summarize approximate_count_Value = count()` (or `approximate_sum_Sum =
sum(Sum)`) `by Value | top N by that desc`, which Kusto approximates.

References: [top-nested](https://learn.microsoft.com/en-us/kusto/query/top-nested-operator),
[top-hitters](https://learn.microsoft.com/en-us/kusto/query/top-hitters-operator).
