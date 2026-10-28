# evaluate plugins

Besides `bag_unpack` (see bag-unpack.md), `evaluate` supports:

- `narrow()`: one `(Row, Column, Value)` row per input cell, with the
  zero-based input row number and the cell as text.
- `pivot(Column [, Aggregation] [, GroupColumns...]) [: OutputSchema]`: one
  column per distinct value of `Column`, aggregating its rows with the
  conditional form of `count()` (the default), `sum`, `min`, `max`, `avg`,
  `dcount`, `take_any`, `stdev`, `variance`, `make_list`, `make_set` or
  `make_bag`, grouped by the given columns or every other column. Values are
  discovered with the application's `ValuesResolver` (at most 256), or come
  from the output schema's columns after the group columns.
- `ipv4_lookup(LookupTable, SourceKey, LookupKey [, ExtraKeys...] [, return_unmatched])`
  and `ipv6_lookup(...)`: a lookup join matching addresses to the lookup
  table's addresses or prefixes through the application's `ipv4_is_in_range`
  / `ipv6_is_in_range`, plus equal extra keys; `return_unmatched = true`
  keeps unmatched rows with null lookup columns.

- `rows_near(Condition, NumRows [, RowsAfter])`: the rows within `NumRows`
  rows before (or `RowsAfter` after) a matching row, in serialized order,
  with a sliding `ROWS BETWEEN` window; row counts are constants up to
  1048576.
- `sequence_detect(Timeline, MaxStepWindow, MaxSpan, Step1, Step2, ..., Dim1, ...)`:
  every chain of records matching the steps in order, with equal (or both
  null) dimensions. Each step's time is strictly later than the previous
  step's, by at most `MaxStepWindow`, and at most `MaxSpan` after the first
  step. Steps are the leading Boolean arguments and dimensions are the
  arguments after them. The output has the dimensions, a
  `<Step>_<Timeline>` column per step (unnamed steps are `Expr<n>`), and
  `Duration`, ordered by the first step's time. Each step is a filtered
  self-join, at most 16 steps. Wide windows over dense matches produce
  many chains, so applications should bound result sizes.
- `sliding_window_counts(Id, Timeline, Start, End, LookbackWindow, Bin [, Dim...])`:
  `Count` and `dcount(Id)` for each `Bin`-aligned window (from `Start` to
  `End`) over the records in the previous `LookbackWindow`, rounded down to
  whole bins, per dimension combination. This uses Kusto's sliding-window
  technique: each record between `Start` and `End` is expanded with
  `range()` to the windows it falls in, then summarized. Windows with no
  records are omitted. Calendar bins (`"week"`, `"month"`, `"year"`) are not
  supported. Each record expands to `LookbackWindow / Bin` rows, bounded by
  the application's `range()` limit.

Other plugins report `KQLL0391`.
