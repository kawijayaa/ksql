# reduce

`T | reduce [kind = source] by Expr [with [threshold = T] [, characters = C]]`
groups string values into patterns in which `*` stands for varying terms.
Pattern discovery is an aggregate the application supplies, registered with
`WithTypedAggregate`:

- `reduce_patterns(value, threshold, characters)` returns a dynamic array of
  `[Pattern, Count, Representative]` rows. KSQL expands it into the
  `Pattern`, `Count` and `Representative` columns, sorted by `Count`
  descending.
- `reduce_source(value, threshold, characters)` returns `[value, Pattern]`
  rows for `kind = source`. KSQL left-joins them to the input on
  `tostring(Expr)` and appends a `Pattern` column. A `Pattern` column already
  in the input is replaced. This kind needs a bound input schema
  (`KQLB0364`).

`threshold` defaults to `0.1`. `characters` defaults to `""`, which means
every non-alphanumeric character separates terms. Other `kind` values and
unknown `with` parameters are parse errors (`KQLP0364`).

Striem implements both aggregates with Vaarandi's SLCT, which Microsoft cites
as the basis of `reduce`:

- A term at a position is frequent when more than `threshold` of the values
  contain it there.
- Each value's candidate keeps its frequent terms and replaces the rest
  with `*`.
- A candidate supported by more than `threshold` of the values becomes a
  pattern. All other values are counted under `others`, with an empty
  representative.

Microsoft's two `rand()` examples contradict this rule (and each other), so
they are not conformance targets.

Reference: [reduce operator](https://learn.microsoft.com/en-us/kusto/query/reduce-operator).
