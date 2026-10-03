# Scalar typing and conditional values

The compiler checks known Boolean operands for `and`/`or`, rejects known
string/Boolean operands in binary arithmetic, and rejects nonnumeric unary
arithmetic. Dynamic/temporal unary and dynamic logical overloads need compatible
runtime semantics and currently return explicit unsupported diagnostics.
Unknown-schema expressions retain legacy late-binding behavior; passing static
checks does not establish type correctness for an unknown schema.

Nested signs retain parentheses so `-(-1)` cannot become a SQL line comment.
Direct numeric literals preserve SQLite's exact signed-minimum integer token.
Unary int columns promote to long; literal/cast constants retain int. Unknown
extension calls are treated as nonconstant. Full integer overflow, dynamic
arithmetic, complete constant folding/purity and temporal overloads remain open.

`bool(true)`, `bool(false)`, `bool(null)` and the `boolean` alias retain Boolean
metadata. These literal constructors don't implicitly coerce strings/numbers;
use `tobool` for conversions. Boolean `not()` preserves null. Binary `and`/`or`
still use SQL three-valued logic. Microsoft documentation currently disagrees:
the logical-operator page lists `bool(null) and true` as false, while the more
specific null-value page specifies sticky null except when an operand is false.
Their equality/inequality tables also disagree for two null inputs. No Kusto
engine differential evidence is available here; do not claim those cases are
fully conformant. Equality against literal null still has the older IS/IS NOT
lowering and requires correction and broader conformance work.

## Conditional type inference

`iff`/`iif` and `case` require Boolean predicates for known types. All result
branches participate in inference, including the else branch and branches not
selected at runtime. Numeric branches widen int -> long -> real, and SQL casts
preserve that widening before subsequent arithmetic. Thus
`iff(true, 1, 2.0) / 2` returns 0.5, not SQLite integer division's zero.
Null branches accommodate the represented common type. All-dynamic results
retain dynamic output metadata; all-Boolean results retain Boolean metadata.

The pinned Microsoft binder uses CommonScalarOrDynamic and CommonNonDynamic.
Heterogeneous non-dynamic branches (for example, a number and a string) have a
dynamic result. SQLite/PostgreSQL renderers encode each known scalar with its
actual type: strings are JSON strings, Booleans remain true/false, and long
integers retain precision. SQLite evaluates each scalar encoding once. JSON
validation rejects nonfinite numeric encodings rather than silently retagging
such values. PostgreSQL has render/parameter checks only.

Mixed existing dynamic/non-dynamic conditional branches, temporal values in
heterogeneous dynamic results, and other dialect encodings return explicit
diagnostics until tagged dynamic conversion is available. Nested temporal/GUID
values, decimal identity, extracted dynamic primitives and nonfinite values
still need the full type representation and Kusto differential verification.

## Coalesce

`coalesce` accepts 2–64 arguments. Known scalar arguments must have a common
represented type, with numeric widening and casts as above. String arguments
use NULLIF(value, '') before SQL COALESCE, so empty strings are skipped without
repeating the expression. All-empty string output is characterized as the
empty string, consistent with KQL strings having no null value, but still
requires differential verification. All-null non-string arguments remain null.

Dynamic plus string arguments retain legacy VARCHAR conversion. This is not
complete tagged dynamic-to-string semantics: JSON string literals versus raw
strings extracted from dynamic columns remain ambiguous and require correction.
Unknown-schema argument types remain unknown in output metadata.

## Resource and extension policies

The compiler bounds `case` to 64 predicate/result pairs (129 arguments);
this is a local policy, not a Kusto service quota. Coalesce uses the documented
64-argument maximum. Signature bounds are checked before lowering arguments.
Existing projection, source, function-expansion and SQL-work limits still apply.
Query-defined functions and explicit function rules retain override precedence.

SQLite execution and HTTP tests cover types, numeric/string/null/Boolean values,
heterogeneous encoded values, exact long limits, NUL strings, parameter reuse,
argument boundaries and selected-expression evaluation counts. Rendering alone
does not prove PostgreSQL runtime semantics. Full null behavior, dynamic tags,
constant folding, evaluation order, all scalar overloads and engine-level
conformance remain incomplete.

Sources:

- [Boolean literals](https://learn.microsoft.com/en-us/kusto/query/scalar-data-types/bool)
- [Coalesce](https://learn.microsoft.com/en-us/kusto/query/coalesce-function)
- [Null values](https://learn.microsoft.com/en-us/kusto/query/scalar-data-types/null-values)
- [Logical operators](https://learn.microsoft.com/en-us/kusto/query/logical-operators)
- [Pinned function signatures](https://github.com/microsoft/Kusto-Query-Language/blob/c94c9e761b3d095e2b9c21e3ec3e92afa0af4856/src/Kusto.Language/Functions.cs)
- [Pinned common-type rules](https://github.com/microsoft/Kusto-Query-Language/blob/c94c9e761b3d095e2b9c21e3ec3e92afa0af4856/src/Kusto.Language/Symbols/TypeFacts.cs)
