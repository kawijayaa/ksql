# ksql

`ksql` is a dependency-free Go library that parses Kusto Query Language (KQL)
and compiles relational KQL pipelines to SQL.

The language inventory is pinned to Microsoft's official
[Kusto-Query-Language](https://github.com/microsoft/Kusto-Query-Language)
source. KQL also contains control-plane, remote-execution, visualization,
graph, and plugin features that have no generic SQL equivalent. The compiler
returns stable diagnostics for those features rather than silently changing
their meaning.

## Status

The implementation is under active construction. See
[`features/README.md`](features/README.md) for the coverage contract and
machine-readable inventory.

## License

Apache-2.0. See [`NOTICE`](NOTICE) for upstream attribution.
