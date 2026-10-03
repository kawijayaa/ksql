# Management commands

The parser now separates dot-prefixed command headers from their result
pipelines. Validated local forms are `.show tables`, `.show tables (T1,T2)`,
`.show table T cslschema`, `.show table T kqlschema` and
`.show table T schema as json`. Table names can be bare or quoted identifiers.
Commands must be standalone roots, with optional query operators after a pipe;
they cannot be embedded in lets, functions, parentheses or scalar subqueries,
or combined with other result statements.

Configure an actual logical metadata snapshot explicitly:

```go
compiler := ksql.New(dialect.SQLite(), ksql.WithParameters(),
    ksql.WithManagementCatalog(ksql.ManagementCatalog{
        DatabaseName: "Investigation",
        Tables: []ksql.ManagementTable{{
            Name: "Events",
            Schema: ksql.Schema{Columns: []ksql.Column{
                {Name: "TimeGenerated", Type: ksql.TypeDateTime},
                {Name: "Host", Type: ksql.TypeString},
            }},
        }},
    }))
result := compiler.Compile(`.show tables | where TableName == "Events"`)
```

The option copies table/column slices when the compiler is constructed. Listing
returns TableName, DatabaseName, Folder and DocString. Schema commands also
return Schema as a string. JSON text contains Name and OrderedColumns with
Name/Type/CslType entries for the represented types. CSL text is a canonical
comma-separated column/type list; unusual column names use quoted identifiers.
Unknown schemas/types produce explicit diagnostics instead of invented types.
Management table lookup is case-sensitive, independently of legacy query
catalog lookup. Selection uses table-set semantics; duplicate selections do not
duplicate output rows. Catalog tables are listed in deterministic name order.

Local resource policies bound a catalog to 4096 tables, 4096 columns per table,
32768 columns overall and 4 MiB of metadata. Emitted result metadata has a
separate 4 MiB budget including repeated database names and JSON encoding.
Oversized results fail with a diagnostic; a parenthesized table selection can
reduce the metadata generated. These are compiler policies, not Kusto quotas.
Normal source/projection/parameter and application output policies still apply.

Other command headers retain an opaque token/span representation and return
explicit management-runtime diagnostics by default. Opaque recognition is not
validation of the complete command grammar. Source extensions may handle a
standalone `management` source, using its Command AST and Raw tokens; extensions
remain responsible for runtime semantics. The compiler's extension hook lowers
relations and is not itself a network client or a mutation executor.

Striem supplies its actual logical catalog: database namespace `striem`, the
aggregate Events view and deployment table views. Folder/docstrings default
to empty because Striem currently has no stored metadata for those properties.
Catalog refresh updates subsequent command compilations. The normal query API,
validation endpoint, row bounds and result pipelines are used. Autocomplete
offers the supported headers, table names and schema formats.

SQLite execution, HTTP and browser tests cover schema/string types, table
selection, quoting, pipelines, empty catalogs, snapshot isolation, catalog
refresh, invalid syntax, scope restrictions, unavailable runtimes and resource
bounds. PostgreSQL rendering/parameter checks do not establish runtime behavior.
Exact service JSON/CSL serialization, duplicate/missing-table behavior and case
rules still need Kusto differential evidence. The pinned command result schema
includes DocString, unlike some current documentation tables that list four
columns; this implementation follows the pinned five-column schema.

This is initial management support. Complete command grammar/inventory,
mutations/transactions, schemas/storage policies, authorization/principals,
service operations, cluster state, external tables, ingestion/export, scripts
and external runtime integration remain incomplete. Full query-language
conformance also remains open.

Sources:

- [Show tables](https://learn.microsoft.com/en-us/kusto/management/show-tables-command?view=microsoft-fabric)
- [Show table schema](https://learn.microsoft.com/en-us/kusto/management/show-table-schema-command?view=microsoft-fabric)
- [Pinned command signatures/results](https://github.com/microsoft/Kusto-Query-Language/blob/c94c9e761b3d095e2b9c21e3ec3e92afa0af4856/src/Kusto.Language.Generators/EngineCommandInfos.cs)
