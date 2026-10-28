# parse-kv

`parse-kv` extends each row with columns for requested keys extracted from a
string. It follows Microsoft's
[parse-kv operator](https://learn.microsoft.com/en-us/kusto/query/parse-kv-operator?view=microsoft-fabric):

```kusto
T | parse-kv Expression as (Key[:type], ...) [with (Options)]
```

## Dialect requirement

Extraction runs in Go, in `runtime.KeyValuesJSON(value, options)`, which
applications register as a SQL function. On SQLite:

```go
connection.RegisterFunc("kql_parse_kv", runtime.KeyValuesJSON, true)
dialect.SQLite(dialect.WithKeyValuesFunction("kql_parse_kv"))
```

The compiler binds a JSON `runtime.KeyValueOptions` document as a placeholder
or quoted literal. The function returns a JSON object holding the first value of
each requested key. Each column then applies the usual KQL conversion for its
type, as `parse` does. Dialects without the function return `KQLL0371`. The
extraction stage is a streaming fence, so it runs once per row.

## Modes

| Options | Mode | Behavior |
| --- | --- | --- |
| `pair_delimiter`, `kv_delimiter` [, `quote`..., `escape`..., `greedy`] | Delimited | Pairs split at the pair delimiter; each pair splits at its first key delimiter. Pairs without a key delimiter are ignored. With `greedy=true`, an unquoted value continues until a pair delimiter followed by another key and key delimiter, or the end. |
| none, or only `quote`/`escape` | Free | Any non-alphanumeric character is a delimiter. Tokens are letter/digit runs or quoted strings. A requested key token takes the next token as its value. |
| `regex` | Regex | Every match of a regex with exactly two capturing groups gives a key and value. |

`quote` takes one character (same open and close) or two (open, close). It may
be repeated, and it applies to keys and values. `escape` takes one character,
may be repeated, requires `quote`, and makes the next character literal inside
quotes. An unterminated quote runs to the end of the value.

Keys match exactly. Only requested keys are extracted, and the first appearance
wins. Keys and values are trimmed of surrounding whitespace. A missing string
key is an empty string; missing keys of other types and failed conversions are
null, as documented ("null or an empty string, depending on the column type"). Columns with an input column's name replace it in
place. Non-string sources are converted with `tostring`.

## Validation and limits

Diagnostics: `KQLP0351` for malformed syntax. `KQLB0380` for unknown or
duplicate options, a missing or empty delimiter, `greedy` without delimiters,
non-Boolean `greedy`, `regex` combined with other options, a regex without
exactly two groups, a quote that is not one or two characters, a multi-character
escape, or escape without quote. `KQLB0373` for an unsupported type,
`KQLB0374` for a duplicate key, `KQLB0375` for more than 128 keys, and
`KQLB0377` for an unknown input schema.

Inputs are limited to 1 MiB per value. Scanning is linear: greedy lookahead
stops at the next delimiter, and regexes use RE2. Parsed option documents are
cached, up to 256 distinct documents.

## Tests and open gaps

`runtime/keyvalues_test.go` reproduces every documented example in all three
modes. It also covers first-appearance rules, unrequested and malformed pairs,
Unicode, overlapping delimiters, option validation and the size limit, with a
fuzz target. `tests/compiler/parse_kv_test.go` executes the examples through
SQLite, with typed columns, replacement, non-string sources, quoted keys,
SQL-like data and every diagnostic.

Open items, inferred from the documentation and not certified against the
engine:

- Free-mode pairing when a value token equals a requested key.
- Whether `_` counts as alphanumeric in free mode. Here it is a delimiter,
  following the documentation's wording.
- Live-engine differential conformance.
