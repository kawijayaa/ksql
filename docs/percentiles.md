# Percentiles

KSQL leaves percentile and sketch aggregates to the application (Striem
implements them with t-digests and HyperLogLog), and handles their shape:

- `summarize percentiles(x, 50, 99.9)` expands to one column per percentile,
  `percentile(x, 50)` and `percentile(x, 99.9)`, named `percentile_x_50` and
  `percentile_x_99_9`. `percentilesw(x, w, ...)` expands to `percentilew`.
  A tuple name, `(p50, p95) = percentiles(x, 50, 95)`, names the columns in
  order and must match the percentile count (`KQLB0514`).
- Unnamed `percentile(x, p)` is `percentile_x_p`, `percentiles_array(x, ...)`
  is `percentiles_x` and `percentilesw_array(x, w, ...)` is `percentile_x`.
- `ksql.WithScalarOverload(name, n)` makes calls of an aggregate with at
  least `n` arguments scalar calls of the same rule, for Kusto functions that
  are aggregates with one argument and scalars with several (`hll_merge`,
  `merge_tdigest`).
