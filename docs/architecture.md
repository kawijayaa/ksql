# Architecture

## Compilation stages

1. `kql.Lex` creates source-located tokens. KQL keywords remain contextual.
2. `kql.Parse` creates typed scripts, pipelines, core operator bodies, and
   recognized raw bodies for advanced operators.
3. The root compiler resolves `let` bindings and lowers supported KQL nodes to
   `sqlast` queries and expressions.
4. The `dialect` package renders the SQL AST for a concrete database.
5. The compiler returns SQL only when no error diagnostic exists.

Pipeline stages lower to nested derived tables. This is intentionally verbose:
it preserves KQL evaluation order and projection alias visibility. A future
optimizer may flatten only transformations proven to preserve those semantics.

## Support contract

KQL is larger than relational SQL. It includes remote HTTP/database access,
Python/R/C# execution, AI calls, visualizations, graph traversal, stateful scan
steps, control-plane commands, and Kusto-specific distributed execution hints.
No generic SQL text can reproduce those capabilities.

The library therefore distinguishes:

- **Inventory coverage:** the feature has a stable ledger entry.
- **Parse coverage:** syntax is validated, recognized, or retained as opaque.
- **Translation coverage:** compilation returns SQL or a stable diagnostic.
- **Semantic support:** translation is exact, equivalent, lossy, or unsupported.

Lossy behavior is documented rather than presented as equivalent. For example,
KQL `has` uses term-index semantics and is not silently rewritten to SQL
`LIKE`; approximate `dcount` is not silently changed to exact
`COUNT(DISTINCT ...)`.

## Extension rules

Extensions are explicit compiler options and receive SQL AST nodes, not SQL
fragments:

```go
compiler := ksql.New(
    dialect.PostgreSQL(),
    ksql.WithFunction("hash_sha256", ksql.SQLFunction("digest_sha256")),
)
```

`WithOperator` receives the preceding relation and parsed operator. `WithSource`
receives a parsed source. These adapters are the intended integration point for
installed UDFs, external tables, graph extensions, and plugin runtimes.

## Updating KQL

1. Update `inventory/source.json` to the audited Microsoft commit.
2. Reconcile query syntax against `QueryParser.cs`, `QueryGrammar.cs`, and
   `SyntaxNodeInfos.cs`.
3. Reconcile functions, aggregates, plugins, and parameters against the catalog
   files listed in `inventory/source.json`.
4. Add every new construct to `features` before adding lowering.
5. Add parser, lowering, dialect, and negative tests as appropriate.
