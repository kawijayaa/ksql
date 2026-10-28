package compiler_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
)

func TestRenderAnnotatesWithoutChangingRows(t *testing.T) {
	db := openInvestigationDB(t)
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters())
	for _, tc := range []struct{ query, rows, visualization string }{
		{`datatable(Computer:string, TimeGenerated:datetime, Val:real)["dc", datetime(2024-01-01T00:10:00Z), 2.0, "dc", datetime(2024-01-01T00:20:00Z), 4.0] | summarize avg(Val) by Computer | render timechart`,
			`[[dc 3]]`, `{"type":"timechart"}`},
		{`range x from 1 to 3 step 1 | extend y=x*2, s="a" | render columnchart with (kind=stacked100, series=s, ycolumns=y, x, xcolumn=x, title="Requests", xtitle='X', ytitle="Y", legend=hidden, accumulate=true, ymin=-1.5, ymax=10, xaxis=linear, yaxis=log, ysplit=panels)`,
			`[[1 2 a] [2 4 a] [3 6 a]]`, `{"type":"columnchart","properties":{"accumulate":true,"kind":"stacked100","legend":"hidden","series":["s"],"title":"Requests","xaxis":"linear","xcolumn":"x","xtitle":"X","yaxis":"log","ycolumns":["y","x"],"ymax":10,"ymin":-1.5,"ysplit":"panels","ytitle":"Y"}}`},
		{`print ['a b']=1 | render piechart with (kind=map, series=['a b'])`, `[[1]]`, `{"type":"piechart","properties":{"kind":"map","series":["a b"]}}`},
		{`print A=1, X=2 | render anomalychart with (anomalycolumns=a, xcolumn=x)`, `[[1 2]]`, `{"type":"anomalychart","properties":{"anomalycolumns":["A"],"xcolumn":"X"}}`},
		{`print n=1 | render table`, `[[1]]`, `{"type":"table"}`},
	} {
		result := compiler.Compile(tc.query)
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.rows {
			t.Fatalf("%s rows = %s, want %s", tc.query, got, tc.rows)
		}
		encoded, _ := json.Marshal(result.Visualization)
		if string(encoded) != tc.visualization {
			t.Fatalf("%s visualization = %s, want %s", tc.query, encoded, tc.visualization)
		}
	}
	if result := compiler.Compile(`print n=1`); result.Visualization != nil {
		t.Fatalf("visualization without render: %#v", result.Visualization)
	}
}

func TestRenderDiagnostics(t *testing.T) {
	compiler := ksql.New(dialect.SQLite())
	for _, tc := range []struct{ query, code string }{
		{`print n=1 | render`, "KQLP0352"},
		{`print n=1 | render timechart (title="x")`, "KQLP0352"},
		{`print n=1 | render timechart with (title="x",)`, "KQLP0352"},
		{`print n=1 | render timechart with (n)`, "KQLP0352"},
		{`print n=1 | render sparkline`, "KQLB0397"},
		{`print n=1 | render timechart with (kind=stacked)`, "KQLB0398"},
		{`print n=1 | render columnchart with (kind=map)`, "KQLB0398"},
		{`print n=1 | render timechart with (legend=shown)`, "KQLB0398"},
		{`print n=1 | render timechart with (title=x)`, "KQLB0398"},
		{`print n=1 | render timechart with (ymin="a")`, "KQLB0398"},
		{`print n=1 | render timechart with (accumulate=yes)`, "KQLB0398"},
		{`print n=1 | render timechart with (color=red)`, "KQLB0398"},
		{`print n=1 | render timechart with (title="a", title="b")`, "KQLB0398"},
		{`print n=1 | render timechart with (anomalycolumns=n)`, "KQLB0398"},
		{`print n=1 | render timechart with (xcolumn=n, m)`, "KQLB0398"},
		{`print n=1 | render timechart with (series=missing)`, "KQLB0401"},
		{`print n=1 | render timechart | take 1`, "KQLB0399"},
		{`let t = print n=1 | render table; t`, "KQLB0399"},
	} {
		result := compiler.Compile(tc.query)
		if result.OK() || result.SQL != "" || result.Visualization != nil || result.Diagnostics[0].Code != tc.code {
			t.Fatalf("%s: diagnostics = %#v, want %s", tc.query, result.Diagnostics, tc.code)
		}
	}
}
