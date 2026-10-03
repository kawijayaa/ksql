# Feature coverage

Coverage has four independent dimensions:

- **Inventory:** query/function catalogs are reconciled against the pinned source;
  management command inventory is currently partial.
- **Syntax:** a feature is validated, recognized, or preserved as opaque.
- **Translation:** compilation emits SQL or a stable diagnostic.
- **Semantics:** support is exact, equivalent, lossy, or unsupported per target.

`exact` and `equivalent` describe the documented represented forms; labels alone
do not prove all overloads or service-level conformance. `lossy`
translation must be explicitly enabled. `unsupported` features never emit
plausible but incorrect SQL.

The ledger is available through `features.All`, `features.Lookup`, and
`features.JSON`. It is built from the function, aggregate, plugin, operator,
statement, and source catalogs at the upstream commit recorded in
`inventory/source.json`, and is checked against compiler tests.

Management has validated table metadata entries and an opaque fallback. The
complete command catalog, command grammar and external runtimes remain open;
see [management commands](../docs/management-commands.md).
