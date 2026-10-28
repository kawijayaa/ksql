# Core aggregate semantics

`count`, `countif`, `sum`, `sumif`, `avg`, `avgif`, `min`, `minif`, `max` and
`maxif` have validated built-in signatures. They require summarize aggregation
expressions; grouping keys, projections, filters and aggregate arguments cannot
call these aggregates. Scalar wrappers around sibling aggregates are supported,
as are independent summarize stages inside `toscalar`/tabular query functions.
Nested pipelines reset the aggregation scope rather than inheriting permission
from their caller. Result expressions cannot reference input columns directly,
including columns also used as grouping keys. Aggregate arguments use the input
row scope. Scalar lets remain available in result expressions even if their
names also match input columns; aggregate arguments prefer the input column.
Qualified/dynamic accesses and wildcard projections obey the same scope rule.
Aggregate helper functions and aggregation contexts in other operators still
require broader binder work.

Ungrouped constant summaries produce one result row on nonempty and empty input.
A hidden COUNT anchors SQL grouping when emitted result expressions contain no
aggregate; an outer projection removes it. Independent cached subquery aggregates
do not count as aggregation of the caller's input. Scalar extensions that discard
aggregate arguments also preserve the single-group cardinality. Result expressions
and bound parameters are not duplicated, and the hidden column is not exposed in
schemas or charged against the user projection limit. Zero-column results remain
explicitly unsupported by this SQL runtime.

## Extension roles

Register aggregate runtime adapters with `WithAggregate` or `WithTypedAggregate`.
Their arguments use row scope; their calls require an aggregate result context
and cannot nest directly inside another aggregate. Typed adapters can validate
predicates and return accurate output types. `WithFunction`/`WithTypedFunction`
register scalar adapters; their arguments inherit the surrounding scalar/aggregate
scope, so a scalar wrapper can combine aggregate results but cannot directly read
input columns in summarize. The last non-nil, non-empty registration wins across
all four APIs, including its role. Compiler-owned special constructs (materialize,
toscalar and datetime/timespan literals) retain their handling. Query-defined
functions retain name precedence.
Existing application SQL aggregates registered with WithFunction must migrate to
an aggregate API to consume row expressions in summarize.

Extension authors remain responsible for signature/resource policies, default
values and actual SQL runtime semantics. An adapter declared aggregate must emit
an actual SQL aggregate rather than a scalar row expression. This role metadata
is not proof of semantic compatibility. Striem's statistics, bag/set, collection
and distinct-count mappings now follow these context rules; approximate distinct
count semantics, complete collection limits/tags and broader runtime fidelity
remain separate open work.

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

Timespans are typed ticks: `sum`, `min`, `max` and `avg` (truncated to whole
ticks) return timespans; see [timespans](timespans.md). Decimal/GUID identity,
datetime `avg`, string collation,
nonfinite/overflow behavior, unknown-schema runtime types, full implicit naming,
all aggregate families and Kusto differential verification remain open. Decimal/real conflation does not establish correct overload behavior. Feature-ledger labels describe
the tested mapping, not full specification conformance.

Sources:

- [Summarize defaults](https://learn.microsoft.com/en-us/kusto/query/summarize-operator?view=azure-monitor)
- [Conditional average](https://learn.microsoft.com/en-us/kusto/query/avgif-aggregation-function?view=microsoft-fabric)
- [Conditional sum](https://learn.microsoft.com/en-us/kusto/query/sumif-aggregation-function?view=microsoft-fabric)
- [Pinned aggregate signatures](https://github.com/microsoft/Kusto-Query-Language/blob/c94c9e761b3d095e2b9c21e3ec3e92afa0af4856/src/Kusto.Language/Aggregates.cs)
- [Pinned scalar type flags](https://github.com/microsoft/Kusto-Query-Language/blob/c94c9e761b3d095e2b9c21e3ec3e92afa0af4856/src/Kusto.Language/Symbols/ScalarTypes.cs)

- [Pinned aggregate name scope](https://github.com/microsoft/Kusto-Query-Language/blob/c94c9e761b3d095e2b9c21e3ec3e92afa0af4856/src/Kusto.Language/Binder/Binder_Names.cs)
- [Pinned summarize binding scope](https://github.com/microsoft/Kusto-Query-Language/blob/c94c9e761b3d095e2b9c21e3ec3e92afa0af4856/src/Kusto.Language/Binder/Binder_TreeBinder.cs)

- [Pinned function argument scope](https://github.com/microsoft/Kusto-Query-Language/blob/c94c9e761b3d095e2b9c21e3ec3e92afa0af4856/src/Kusto.Language/Binder/Binder_Misc.cs)
