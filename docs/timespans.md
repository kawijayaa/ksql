# Timespans and datetime arithmetic

KSQL represents [timespans](https://learn.microsoft.com/en-us/kusto/query/scalar-data-types/timespan?view=microsoft-fabric)
as 100-nanosecond ticks, Kusto's own resolution, stored as SQL integers with
the `timespan` type (`System.TimeSpan` in `getschema`).

## Literals

- Units: `2d`, `1.5h`, `30m`, `10s`, `0.1s`, `100ms`, `10microsecond`,
  `1tick`. Spelled-out units such as `time(15 seconds)` are accepted, as are
  nanoseconds, which round to the nearest tick.
- `time(2)` and `timespan(2)`: days.
- `time(0.12:34:56.7)`: `[-][d.]hh:mm[:ss[.fffffff]]`.
- `timespan(null)`: a typed null.

Invalid literals return `KQLB0411`.

## Operators

| Expression | Result |
| --- | --- |
| `datetime ± timespan`, `timespan + datetime` | datetime |
| `datetime - datetime` | timespan |
| `timespan ± timespan`, `timespan % timespan` | timespan |
| `timespan / timespan` | real |
| `timespan * number`, `number * timespan`, `timespan / number` | timespan, truncated to whole ticks |
| `-timespan`, comparisons, `between` | as for numbers |

Other combinations, such as `datetime + datetime` or `timespan + 1`, return
`KQLB0412`. An operand of unknown type keeps plain SQL arithmetic.

Datetime arithmetic depends on the application's datetime storage, so it is
a dialect capability: `dialect.WithDatetimeArithmeticFunctions(add, diff)` on
SQLite, and built-in interval arithmetic on PostgreSQL. Without it, these
expressions return `KQLL0412`. `tostring(timespan)` formats ticks as
`[-][d.]hh:mm:ss[.fffffff]` (SQLite and other dialects with a timespan text
capability).

`sum`, `min`, `max` and `avg` over timespans return timespans.

## Open

`totimespan` of non-literal strings, `format_timespan`, `make_timespan`,
timespan query parameters, `bin` over timespans in the core compiler, and
datetime `avg` remain application or future work.
