# Query parameter declarations

Top-level `declare query_parameters(...)` statements bind typed request values
as scalar symbols. Declarations support Boolean, int, long, real, string,
datetime and dynamic types, with aliases `boolean`, `int32`, `int64`, `double`
and `date`. Quoted names, multiple declaration statements, scalar lets and
lexical capture by query-defined functions use the ordinary binding scopes.

```go
compiler := ksql.New(dialect.SQLite(), ksql.WithQueryParameters(map[string]any{
    "threshold": int64(90),
    "host": "server-01",
}))
result := compiler.Compile(`
declare query_parameters(threshold:long=50, host:string);
Events | where Count > threshold and Host == host
`)
```

Values are **typed JSON values**, not strings encoding KQL literals. In Go,
numbers, Booleans and strings can be supplied directly; `time.Time` encodes an
ISO datetime string. `json.Number` and `json.RawMessage` preserve exact numeric
and dynamic JSON inputs. For example, a long parameter uses the numeric value
`5`, rather than the string `"long(5)"`. The native REST client forwards Kusto's
separate encoded-string parameter format unchanged; local encoded-KQL parameter
parsing is not implemented yet.

Construction snapshots each provided value as JSON, including mutable raw
messages and nested collections. Changing the original map or byte slices does
not affect the compiler. A configured compiler remains safe for concurrent
compilation. Request values always use SQL placeholders, in either compiler
parameterization mode; reachable occurrences are numbered by the final SQL
traversal, and unused bindings are removed. Provided values are not interpolated
into query text or SQL.

## Type and default contracts

Int and long values must be integral JSON number tokens within signed 32-bit
and signed 64-bit bounds. Real values must be finite numbers. Booleans require
JSON Boolean values, and strings require JSON strings. Numeric strings do not
coerce to numeric parameters. JSON null supplies a typed null for nullable
types; string null is rejected. Dynamic values use compact JSON and retain
integer text without conversion to float64. They share the compiler/runtime's
existing dynamic representation limits and do not introduce Kusto type tags.

Datetime inputs accept RFC3339 with optional fractions, a date-only ISO string,
or ISO date/time text with seconds and no timezone. They normalize to UTC within
years 1–9999. Other Kusto date formats and exact 100-nanosecond rounding are not
certified. Applications can normalize date parameters to their storage format;
Striem does so through its existing runtime conversion.

Defaults must be scalar literals of the declared type. Supported typed
constructors are checked as constants, without invoking user functions or
extensions. Int/long bounds and datetime validity also apply to defaults.
Every default is validated even when a request overrides it. Dynamic parameters
cannot have defaults. Nonliteral expressions, caller symbols and function calls
are rejected. Missing required values, duplicate declarations, incompatible
types and malformed syntax produce positioned diagnostics and no SQL. Provided
names are matched exactly; unused valid request values are ignored.

The parser now preserves unquoted `datetime(...)`/`date(...)` and
`timespan(...)`/`time(...)` payloads as literal text, rather than interpreting
date separators as arithmetic. Datetime null literals retain datetime metadata.
This parser correction does not establish full temporal validation or timespan
runtime support for all contexts.

## Bounds and evidence

Each declaration list contains at most 64 parameters. Total declarations and
provided values are bounded to 1,024; provided names are limited to 4 KiB each,
with a combined encoded snapshot budget of 4 MiB. These are compiler policies,
not Kusto limits. JSON marshaling of caller-owned values occurs before measuring
their encoded size; untrusted API inputs need their own body bound. Striem uses
its existing 64 KiB request and 32 KiB query limits.

SQLite execution tests cover documented threshold behavior, defaults/overrides,
numeric extremes, Boolean/dynamic metadata, SQL-like string values, both
parameterization modes, quoted names, lexical capture, cached scalar use,
aggregation, snapshot isolation and unused-value pruning. API tests cover exact
JSON output, date normalization, validation, input errors and duplicate request
names. Parser/fuzz cases cover valid and malformed declarations and temporal
literal payloads. No live Kusto differential certification has been performed.

Remaining work includes encoded KQL value parsing for local requests, declaration
statements inside function bodies, GUID/decimal/timespan and nonfinite values,
complete case-sensitive name resolution, all coercion/temporal/type-tag rules,
editor parameter-value controls and symbol completion, other-dialect runtime
verification, and engine conformance. Compiler-wide null, naming and dynamic
semantics remain subject to the broader implementation status.

Primary reference: [Microsoft query parameter declarations](https://learn.microsoft.com/en-us/kusto/query/query-parameters-statement?view=microsoft-fabric).
