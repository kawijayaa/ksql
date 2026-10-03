# Core aggregate semantics

`count`, `countif`, `sum`, `sumif`, `avg`, `avgif`, `min`, `minif`, `max` and
`maxif` have validated built-in signatures. They require summarize aggregation
expressions; grouping keys, projections, filters and aggregate arguments cannot
call these aggregates. Scalar wrappers around sibling aggregates are supported,
as are independent summarize stages inside `toscalar`/tabular query functions.
Nested pipelines reset the aggregation scope rather than inheriting permission
from their caller. Bare non-grouped columns in summarize, aggregate helper
functions and aggregation contexts in other operators still require broader
binder work. Other application-defined aggregate mappings currently retain
their own context policies; this is not a complete aggregate binder.

`count()` counts every row. `countif(predicate)` counts only true predicates,
including rows whose other columns are null. The deprecated Boolean
`count(predicate)` overload has the same behavior. `count(*)` and numeric
predicates are invalid. Signatures are checked before argument expansion;
known predicate types must be Boolean. Unknown-schema types retain late binding.
Query-defined functions and explicit function rules retain override precedence.

`sum`/`sumif` and `avg`/`avgif` support represented numeric types. Sum promotes
int input to long; average returns real. Null values are ignored. Conditional
aggregates include only true predicates; false/null predicates yield null input
to the underlying SQL aggregate. `min`/`max` and their conditional variants
retain the represented input type, including string/Boolean/datetime. Dynamic
inputs are not orderable and return diagnostics. Known datetime sum/average
overloads return explicit unsupported temporal-runtime diagnostics.

An ungrouped empty input yields one row: count/countif/sum/sumif are zero;
average/minimum/maximum are null. All-null numeric inputs and predicates that
never qualify follow the same defaults. Real sum defaults use a real zero to
preserve subsequent arithmetic. Grouped empty input yields no rows. Conditional
values/predicates each occur once per call in SQL; SQLite counting-function
tests verify evaluation counts and freshness on repeated execution.

SQLite execution tests cover both parameter modes, null/empty/group behavior,
int/long/real/string/Boolean results, overrides, scalar composition, nested
cached subqueries and sibling coherent-row selectors. Striem HTTP tests verify
typed Boolean output and positioned diagnostics before SQL preparation. The
PostgreSQL renderer uses bool_and/bool_or for Boolean minima/maxima, with
render/parameter checks only; PostgreSQL runtime conformance is unverified.

Timespan/decimal/GUID identity, complete temporal overloads, string collation,
nonfinite/overflow behavior, unknown-schema runtime types, full implicit naming,
all aggregate families and Kusto differential verification remain open. The
current unknown type representation for timespan and decimal/real conflation
do not establish correct overload behavior. Feature-ledger labels describe
the tested mapping, not full specification conformance.

Sources:

- [Summarize defaults](https://learn.microsoft.com/en-us/kusto/query/summarize-operator?view=azure-monitor)
- [Conditional average](https://learn.microsoft.com/en-us/kusto/query/avgif-aggregation-function?view=microsoft-fabric)
- [Conditional sum](https://learn.microsoft.com/en-us/kusto/query/sumif-aggregation-function?view=microsoft-fabric)
- [Pinned aggregate signatures](https://github.com/microsoft/Kusto-Query-Language/blob/c94c9e761b3d095e2b9c21e3ec3e92afa0af4856/src/Kusto.Language/Aggregates.cs)
- [Pinned scalar type flags](https://github.com/microsoft/Kusto-Query-Language/blob/c94c9e761b3d095e2b9c21e3ec3e92afa0af4856/src/Kusto.Language/Symbols/ScalarTypes.cs)
