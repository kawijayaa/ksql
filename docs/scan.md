# scan

`T | scan [with_match_id=Id] [declare (Col:type [= default], ...)] with (step Name [output=all|last|none]: Condition [=> Col = Value, ...]; ...)`
follows Kusto's matching logic over the serialized input: each record is
checked against the steps from last to first; Check 1 promotes the previous
step's active state when the condition holds in that state, and Check 2
extends a step's own active sequence (or, at the first step, starts one and
increments the match id). `Step.Column` reads a step's state; unqualified
declared columns read their defaults. `output=all` emits every match,
`output=last` the last record of each series (when it is replaced, promoted
or the input ends) and `output=none` nothing. The output is the input's
columns, the declared columns and the match id.

The lowering is a recursive query (`WITH RECURSIVE`) that advances one input
record per step, with each step's state in columns: its active flag, match
id, record and the referenced columns of earlier steps. Steps are limited to
16 and state columns to 1024. Records are numbered in the input's serialized
order, or its current order when it has none.

Reference: [scan operator](https://learn.microsoft.com/en-us/kusto/query/scan-operator).

## row_window_session

`extend Name = row_window_session(Expr, MaxDistanceFromFirst, MaxDistanceBetweenNeighbors [, Restart])`
is lowered through a one-step scan: the expression and restart condition are
extended first (so `Restart` may use `prev()`), then the scan starts a new
session when there is no previous session or value, when `Expr` reaches the
session start plus `MaxDistanceFromFirst` or the previous value plus
`MaxDistanceBetweenNeighbors`, or on restart. It must be a named `extend`
item (`KQLB0361`).
