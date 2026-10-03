# Row-selection aggregates

[`arg_max()`](https://learn.microsoft.com/en-us/kusto/query/arg-max-aggregation-function?view=microsoft-fabric)
and [`arg_min()`](https://learn.microsoft.com/en-us/kusto/query/arg-min-aggregation-function?view=microsoft-fabric)
select a complete row with the greatest or least ordering key in each group.
The key and requested expressions are returned from that same row:

```kql
Events
| summarize arg_max(TimeGenerated, *) by Host
```

`*` returns the remaining input columns, excluding group columns and columns
already selected by the call. Explicit return expressions are also supported:

```kql
Events
| summarize (last_seen, last_user) = arg_max(TimeGenerated, User),
            (first_seen, first_user) = arg_min(TimeGenerated, User),
            events = count()
  by Host
```

A single assignment renames only the first result column. A tuple assignment
renames successive result columns; omitted trailing names retain inferred
names. Deprecated aliases `argmax` and `argmin` retain their legacy `max_` and
`min_` output prefixes. Implicit name collisions receive numeric suffixes.
Explicit duplicate declarations return a positioned diagnostic.

## Execution behavior

Non-null keys precede null keys for both functions. If all keys are null,
one row is selected. Equal-key representatives are unspecified, but all
returned values for a call come from the same selected row. Each selector
in a summarize has its own rank; sibling ordinary aggregates read the entire
group rather than only the selected row. Empty grouped inputs produce no rows;
empty ungrouped inputs produce one row with null selected values.

The input and selection expressions are materialized before ranking. This
prevents SQL flattening from reevaluating nondeterministic source columns or
using a different key when returning a ranked row. SQLite uses conditional
aggregation of the unique first-ranked row. PostgreSQL uses a filtered array
aggregate to support Boolean/JSON payloads without requiring an orderable
payload type.

SQLite execution tests cover documented null examples, row coherence,
multiple selectors, sibling aggregates, table functions, materialized inputs,
empty inputs, exact long values, typed payloads, column naming and both compiler
parameter modes. PostgreSQL has SQL/parameter rendering checks only.

## Limits and open conformance

- A known input schema and known orderable key type are required. Dynamic keys
  need a scalar cast. Query-wide materialization rejects correlated inputs.
- SQLite 3.35+ and PostgreSQL 12+ supply the required cache syntax; other
  dialects currently return explicit unsupported diagnostics.
- Output projection limits, materialized binding count and expanded SQL work
  limits apply. Each call has a compiler policy cap of 128 arguments.
- General nested aggregate expressions, complete tuple behavior in other
  operators, wildcard function-parameter declaration inference, conversion
  identity/name rules, explicit-name replacement, case-sensitive symbol
  resolution, string/non-finite ordering and Kusto engine differential checks
  remain open. The library does not yet implement the complete specification.

Column naming follows Microsoft's
[aggregate catalog](https://github.com/microsoft/Kusto-Query-Language/blob/master/src/Kusto.Language/Aggregates.cs)
and [tuple projection binder](https://github.com/microsoft/Kusto-Query-Language/blob/master/src/Kusto.Language/Binder/Binder_Projection.cs)
for the tested forms. Empty-input semantics are specified by the
[summarize reference](https://learn.microsoft.com/en-us/kusto/query/summarize-operator?view=microsoft-fabric).

## Arbitrary row selection

`take_any(expr, ...)` returns expressions from one coherent arbitrary input
record per group. `take_any(*)` returns input columns except group keys;
explicit columns combined with `*` are not repeated. A single scalar argument
prefers a present value, excluding null and empty strings when available.
Multiple arguments share one rank and do not independently minimize columns.
Each separate selector can choose a different record.

```kql
Events
| summarize (sample_time, sample_user) = take_any(TimeGenerated, User),
            matching_user = take_anyif(User, Source == "audit"),
            events = count()
  by Host
```

`take_anyif(expr, predicate)` accepts a known Boolean predicate, ranks true
records first, and prefers non-null/non-empty eligible values. False or null
predicates cannot contribute a selected value. With no eligible records it
returns null without removing the group or changing sibling aggregates.
Deprecated `any` and `anyif` retain `any_` and `anyif_` result prefixes;
`any` accepts either scalar arguments or a single wildcard. Tuple assignments,
Boolean/dynamic payloads, caches, function composition, and both parameter
modes use the same row-selection infrastructure. Unknown payload types and
zero-column wildcard expansion return diagnostics. Explicit function mappings
retain precedence. There is no promise of randomness or a stable representative.

SQLite execution tests exercise coherence, presence, predicates, empty groups,
resource limits, output naming/types and one evaluation per input expression.
PostgreSQL rendering is checked, but execution and Kusto differential tests
remain open. Tagged dynamic null/string values and complete naming/case rules
remain subject to the type-model limitations above. The known-schema,
materialization/dialect and resource policies apply to these selectors too.

References: [take_any](https://learn.microsoft.com/en-us/kusto/query/take-any-aggregation-function),
[take_anyif](https://learn.microsoft.com/en-us/kusto/query/take-anyif-aggregation-function),
and the pinned Microsoft aggregate catalog (notably `anyif_` naming).
