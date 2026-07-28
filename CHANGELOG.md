# Changelog

## Unreleased

## v0.3.0 - 2026-07-28

### Added

- Public `ScalarType`, `Column`, `Schema`, `Table`, `Catalog`, and `Relation`
  APIs with ordered result metadata in `Result.Columns`.
- Catalog-backed, case-insensitive binding for tables, columns, scalar/tabular
  `let` values, `as` aliases, dynamic properties, and join-side qualifiers.
- Context-aware source and operator extension rules with expression/pipeline
  lowering, parameter binding, schemas, aliases, diagnostics, dialect, and
  catalog access.
- Secure opt-in literal parameterization through `WithParameters` and ordered
  `Result.Args`.
- Configurable compiler `Limits` and positioned resource-limit diagnostics.
- Typed `search` parsing and schema-aware lowering through explicit regex
  capabilities.
- Exact-name `project-away`, `project-keep`, `project-rename`, and
  `project-reorder` lowering.
- SQLite regex-UDF configuration through `WithRegexFunction`.
- SQLite execution tests using JSON1 and an application-provided regex UDF.

### Changed

- `Result` now exposes the lowered SQL AST in `Query` and ordered typed output
  columns in `Columns`.
- `WithSource` and `WithOperator` rules now accept a `LoweringContext` and
  return typed `Relation` values.
- `extend` replaces existing columns in place, and bound joins emit explicit
  projections with deterministic right-side collision suffixes.
- SQLite string-key JSON access supports dots, quotes, and backslashes; numeric
  access uses `$[n]` paths.
- SQL union rendering now produces executable SQLite syntax for ordered,
  limited, and nested operands.
- KQL null equality and inequality lower to `IS NULL` and `IS NOT NULL`.

## v0.2.0 - 2026-07-26

### Added

- Typed parsing for `mv-expand`, `mv-apply`, `as`, and `sample-distinct`.
- SQLite single-array expansion with item indexes and per-row limits.
- Row-wise SQLite `mv-apply` pipelines using `where`, `extend`, and `serialize`.
- `sample`, `sample-distinct`, `in~`, and `!in~` lowering.
- Literal KQL term matching for regex-capable PostgreSQL and MySQL dialects.
- Optional random-ordering and JSON-expansion dialect capabilities.

### Changed

- The feature ledger now reports the new equivalent and lossy translations.
- Legacy `mvexpand` preserves its 128-row default limit.
- Unsupported multi-value forms return focused diagnostics instead of the
  generic unsupported-operator diagnostic.

## v0.1.0 - 2026-07-26

- Initial tagged release.
