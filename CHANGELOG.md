# Changelog

## Unreleased

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
