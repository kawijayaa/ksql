# Pattern statements

`declare pattern Name = (Arg:string, ...)[Path:string] { (values).[path] = { query }; ... };`
maps string argument values (and a path value) to tabular queries.
`Name("a").Path`, `Name('a').["Path"]` and, without a declared path,
`Name("a")` invoke a pattern wherever a table can appear, including `union`
operands. Values match exactly; `*` is not a wildcard.

`declare pattern Name;` declares an empty pattern. Invoking a pattern without
a matching entry reports Kusto's error naming the reference, for example
``One or more pattern references were not declared. Detected pattern
references: ["map_ip_to_longlat('10.10.10.10')"]`` (`KQLB0109`), which
middle-tier applications use to supply definitions. Redeclaring a pattern is
an error, and nested invocations are bounded.

Reference: [pattern statement](https://learn.microsoft.com/en-us/kusto/query/pattern-statement).
