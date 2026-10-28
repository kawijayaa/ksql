# evaluate bag_unpack

`T | evaluate bag_unpack(Column [, OutputColumnPrefix] [, columnsConflict] [, ignoredProperties]) [: (OutputSchema)]`
follows Microsoft's [bag_unpack plugin](https://learn.microsoft.com/en-us/kusto/query/bag-unpack-plugin?view=microsoft-fabric).
The unpacked column is removed, and each top-level slot becomes a column,
optionally prefixed.

## Schemas

- **With an output schema**, the listed columns are produced with their
  declared types. A leading `*` keeps the other input columns. No data is
  read at compile time.
- **Without a schema**, the slots depend on the data. The application supplies
  `ksql.WithBagKeysResolver`, which receives a discovery query (a SQL AST
  selecting distinct `key`, `type` pairs, at most 8,193 rows) and returns its
  rows. Slots are sorted by name. A slot whose values all have one JSON type
  gets that type (integer → long, real, text → string, Boolean → bool, object
  or array → dynamic); mixed types become dynamic. Null values, non-object
  values and `ignoredProperties` contribute nothing. Discovery runs on SQLite
  and rejects cached (materialized) or correlated inputs. More than 1,024 keys
  requires an explicit schema.

Without a resolver or schema the operator returns `KQLL0392`.

## Conflicts and options

A slot that has an input column's name is an error by default (`KQLB0416`).
`columnsConflict='replace_source'` replaces that column in place, and
`keep_source` keeps the input value. Options can be positional or named.
Slots are read with the KQL conversion for their type, so application
conversion rules apply.

Other `evaluate` plugins return `KQLL0391`.
