# partition

`T | partition [hint.strategy=legacy|native|shuffle] by Column (SubQuery)`
applies `SubQuery` to the rows of each distinct `Column` value (null is one
value) and concatenates the results. The legacy form
`partition by Column { SubQueryWithSource }` runs a query with its own source
once per value, with `Column` bound to the value.

The distinct values are discovered at compile time with the application's
`ksql.WithValuesResolver`, which runs a `SELECT DISTINCT` over the input,
and each partition becomes one `UNION ALL` branch, so results match Kusto for
any subquery. The legacy strategy allows 64 values, as in Kusto; native and
shuffle allow 1024 here (`KQLB0359`). Without a resolver, or over cached or
correlated input, partition reports `KQLL0394`. Every partition must produce
the same columns. Other hints are accepted and ignored.

Reference: [partition operator](https://learn.microsoft.com/en-us/kusto/query/partition-operator).
