# project-by-names and column_names_of

`T | project-by-names Spec, ...` keeps the columns named by each specifier,
in the order they are specified or matched. A specifier is a string (a
column name, or a pattern where `*` matches any characters), a dynamic array
of such strings, or `column_names_of(T)`. Specifiers are evaluated at compile
time, so they must be constants: literals, `let` values and function
parameters bound to literals. Names that match no column are ignored, as in
Kusto, but a projection that keeps no column is an error (`KQLB0343`).
Other expressions report `KQLB0345`.

`column_names_of(T)` returns the column names of a table, `let` table or
tabular function parameter as a dynamic array. The schema must be known at
compile time.

References: [project-by-names](https://learn.microsoft.com/en-us/kusto/query/project-by-names-operator),
[column_names_of()](https://learn.microsoft.com/en-us/kusto/query/column-names-of-function).
