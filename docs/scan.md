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
