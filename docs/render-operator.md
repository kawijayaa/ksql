# render

[`render`](https://learn.microsoft.com/en-us/kusto/query/render-operator?view=microsoft-fabric)
annotates a result with visualization instructions and never changes rows.
When the result-producing statement ends with `render`, KSQL compiles the
pipeline without it and sets `Result.Visualization`:

```go
result := compiler.Compile(`T | summarize count() by bin(Time, 1h) | render timechart with (title="Events")`)
// result.Visualization == &ksql.Visualization{Type: "timechart", Properties: map[string]any{"title": "Events"}}
```

## Validation

- **Visualizations:** `anomalychart`, `areachart`, `barchart`, `card`,
  `columnchart`, `ladderchart`, `linechart`, `piechart`, `pivotchart`,
  `scatterchart`, `stackedareachart`, `table`, `timechart`, `timepivot`,
  `treemap`.
- **`kind`:** `default`, `unstacked`, `stacked` or `stacked100` for area, bar
  and column charts; `map` for scatter and pie charts.
- **Enums:** `legend` (`visible`, `hidden`), `xaxis` and `yaxis` (`linear`,
  `log`), `ysplit` (`none`, `axes`, `panels`).
- **Values:** `accumulate` is a bool, `ymin` and `ymax` are numbers, and
  `title`, `xtitle` and `ytitle` are string literals.
- **Columns:** `xcolumn` takes one column. `series`, `ycolumns` and
  `anomalycolumns` take comma-separated column lists. `anomalycolumns` applies
  only to `anomalychart`. With a known schema, each name must exist; the output
  uses the column's declared spelling.

`render` anywhere other than the end of the query, including inside `let`
bodies and subqueries, returns `KQLB0399`. Other diagnostics: `KQLP0352`
(syntax), `KQLB0397` (unknown visualization), `KQLB0398` (invalid, duplicate
or unknown property), `KQLB0401` (unknown column).

## Scope

KSQL only validates and reports the annotation; drawing is up to the
application. Clients may infer unspecified axes, as Kusto user agents do. The
visualization list follows the Fabric and Azure Data Explorer tables. Some
values are client-specific there, and they are accepted here too.
