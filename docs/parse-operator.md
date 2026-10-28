# parse and parse-where

`parse` extends each row with columns extracted from one string expression.
`parse-where` uses the same syntax and keeps only rows that parse successfully.
Both follow Microsoft's
[parse operator](https://learn.microsoft.com/en-us/kusto/query/parse-operator?view=microsoft-fabric)
and
[parse-where operator](https://learn.microsoft.com/en-us/kusto/query/parse-where-operator?view=microsoft-fabric)
syntax:

```kusto
T | parse [kind=simple|regex|relaxed [flags=imsU]] Expression with [*] "constant" Column[:type] [*] ...
```

## Dialect requirement

The pattern becomes one RE2 expression. SQL lowering needs a function that
returns the first match's capture groups as a JSON array, with group one at
index zero and null when nothing matches. On SQLite, configure an application
UDF that receives `(value, pattern)`:

```go
dialect.SQLite(dialect.WithRegexCapturesFunction("kql_regex_captures"))
```

Dialects without this capability return `KQLL0370`. PostgreSQL's regex flavor
differs from RE2 (for example `\z` and the `U` flag), so it has no capture
mapping.

## Pattern translation

The generated expression is anchored at both ends with `\A` and `\z`, so the
`m` flag cannot change anchoring. A leading or trailing `*` skips text, and a
final column consumes the rest of the value.

| Item | `simple` | `regex` | `relaxed` |
| --- | --- | --- | --- |
| String constant | Quoted literal | RE2 expression, grouped `(?:...)` | Quoted literal |
| `*` | `.*?` | `.*?` | `.*?` |
| `string` and other non-numeric columns | `(.*?)` | `(.*)` | `(.*?)` |
| `int`, `long` | `(\-?\d+)` | `(\-?\d+)` | `(\-?\d+\|.*?)` |
| `real`, `double`, `decimal` | Signed decimal or exponent number | Same | Number, else `.*?` |

Relaxed numeric columns try a number first and fall back to lazy text. A
non-numeric value then fails only its own conversion, while a number followed
by `*` still captures the whole number. This reproduces both of Microsoft's
relaxed examples; the engine's exact relaxed expression is not published.

Regex constants may contain their own capture groups. They are counted with
Go's RE2 parser, so later columns still read the correct group. `flags` are
allowed only with `kind=regex` and prefix the expression as `(?flags)`. Greedy
string columns in regex mode, and the effect of `U`, reproduce Microsoft's
published regex-mode examples.

Column types map to the KQL conversion functions `toint`, `tolong`, `toreal`,
`todecimal`, `todatetime`, `totimespan`, `tobool`, `toguid` and `todynamic`.
These are bound like ordinary calls, so application overrides apply and an
unsupported conversion is diagnosed. Non-string source expressions are
converted with `tostring`.

## Failure semantics

- No match: every extracted column is null and `parse-where` drops the row.
- `simple` and `regex` are strict: if any typed conversion fails, every
  extracted column is null, and `parse-where` drops the row.
- `relaxed`: each failed conversion is null and the others keep their values.
  `parse-where` rejects `kind=relaxed`.

Columns with the same name as an input column replace it in place, as `extend`
does. Failed string columns are SQL null, matching the rest of KSQL's string
lowering, where Kusto would display an empty string.

## Evaluation and resource bounds

The capture function runs once per row. The stage that computes it is marked as
an optimization fence, which SQLite renders as `LIMIT -1 OFFSET 0`, so the
query flattener cannot copy the call into every extracted column. Rows still
stream, so `parse ... | take 10` stops after ten rows. The regex pattern is
always a SQL placeholder or quoted literal, never query text.

Patterns have at most 256 items and 128 columns. `Limits.MaxRegexBytes` bounds
the generated expression, and `Limits.MaxParseCaptures` bounds all capture
groups, including those in regex constants. Parse requires a known input
schema, so hidden capture columns can be removed from the output.

## Diagnostics

| Code | Condition |
| --- | --- |
| `KQLP0350` | Malformed syntax: unknown kind or property, missing `with`, bad column or type |
| `KQLB0370` | Flags without `kind=regex`, unknown flags, relaxed `parse-where` |
| `KQLB0371` | `*` directly after a string column |
| `KQLB0372` | Invalid regex constant or generated expression |
| `KQLB0373` | Unsupported column type |
| `KQLB0374` | Duplicate extracted column |
| `KQLB0375` | No columns, more than 128 columns, or `MaxParseCaptures` exceeded |
| `KQLB0376` | `MaxRegexBytes` exceeded |
| `KQLB0377` | Unknown input schema |
| `KQLL0370` | Dialect has no capture function |

## Tests and open gaps

`tests/compiler/parse_test.go` executes Microsoft's simple, email, regex,
regex-flag, newline and relaxed examples on SQLite, using both
parameterization modes. It also covers anchoring, replacement, non-string
sources, quoted names, `parse-where`, SQL-like data, schema output,
placeholders, single evaluation, streaming and every diagnostic.

Still open:

- Kusto's exact internal regexes for `bool`, `datetime`, `timespan` and `guid`
  columns in strict modes. These use `(.*?)` followed by conversion.
- Datetime spellings accepted by conversion depend on the application's
  `todatetime` mapping.
- `parse-kv`.
- Differential conformance against a live Kusto engine.
