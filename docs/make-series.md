# make-series

`T | make-series [kind=nonempty] [Name =] Aggregation [default = Value] [, ...] on Axis [from Start] [to End] step Step [by Group, ...]`
follows Microsoft's [make-series operator](https://learn.microsoft.com/en-us/kusto/query/make-series-operator?view=microsoft-fabric).
The alternate `on Axis in range(Start, Stop, Step)` form has an inclusive stop
and `bin()`-aligned start.

- **Bins:** rows are grouped by the `by` expressions and
  `bin_at(Axis, Step, Start)`. Each aggregation becomes a dynamic array with one
  element per bin of `[Start, End)`, in bin order (`json_group_array(... ORDER BY bin)`).
  Absent bins take the default, which is 0 unless given. The last column is
  the array of bin values.
- **Bounds:** without `from`, each series starts at its first bin with data,
  aligned like `bin()` (datetimes from 0001-01-01). Without `to`, it ends
  after its last bin with data.
- **Grouping:** groups match null-safely.
- **Empty input:** produces no rows unless `kind=nonempty` (without `by`, with
  `from` and `to`), which yields default-filled arrays.
- **Axes:** datetime with a timespan step (through the datetime arithmetic
  capability), timespan, or numeric.
- **Limits:** arrays are capped at 2^20 elements, as in Kusto. Aggregations
  must return numbers.

Requires SQLite 3.44 or later for ordered aggregates; other dialects return
`KQLL0394`. Series functions (`series_fill_*`, `series_decompose_*`, ...) are
separate and largely not yet implemented.
