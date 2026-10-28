package compiler_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
)

const seriesData = `let data=datatable(timestamp:datetime, metric: real)[
datetime(2016-12-31T06:00), 50, datetime(2017-01-01), 4, datetime(2017-01-02), 3, datetime(2017-01-03), 4,
datetime(2017-01-03T03:00), 6, datetime(2017-01-05), 8, datetime(2017-01-05T13:40), 13, datetime(2017-01-06), 4,
datetime(2017-01-07), 3, datetime(2017-01-08), 8, datetime(2017-01-08T21:00), 8, datetime(2017-01-09), 2,
datetime(2017-01-09T12:00), 11, datetime(2017-01-10T05:00), 5];
let interval = 1d; let stime = datetime(2017-01-01); let etime = datetime(2017-01-10);
`

// Microsoft's make-series examples.
func TestMakeSeries(t *testing.T) {
	db, err := sql.Open("ksql_timespan_sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	days := `["2017-01-01T00:00:00Z","2017-01-02T00:00:00Z","2017-01-03T00:00:00Z","2017-01-04T00:00:00Z","2017-01-05T00:00:00Z","2017-01-06T00:00:00Z","2017-01-07T00:00:00Z","2017-01-08T00:00:00Z","2017-01-09T00:00:00Z"]`
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		compiler := ksql.New(dialect.SQLite(dialect.WithDatetimeArithmeticFunctions("test_dt_add", "test_dt_diff")), ksql.WithParameterization(mode))
		if mode == ksql.LiteralParameters {
			continue // SQLite cannot evaluate TIMESTAMP literals without an application rewrite.
		}
		for _, tc := range []struct{ query, names, want string }{
			{seriesData + `data | make-series avg(metric) on timestamp from stime to etime step interval`, `avg_metric,timestamp`,
				`[[[4.0,3.0,5.0,0,10.5,4.0,3.0,8.0,6.5] ` + days + `]]`},
			{seriesData + `data | take 0 | make-series avg(metric) default=1.0 on timestamp from stime to etime step interval | count`, `Count`, `[[0]]`},
			{seriesData + `data | take 0 | make-series kind=nonempty avg(metric) default=1.0 on timestamp from stime to etime step interval`, `avg_metric,timestamp`,
				`[[[1.0,1.0,1.0,1.0,1.0,1.0,1.0,1.0,1.0] ` + days + `]]`},
			{`let startDate = datetime(2025-01-06); let endDate = datetime(2025-02-09); datatable(Time: datetime, Value: int, other:int)[datetime(2025-01-07), 10, 11, datetime(2025-01-16), 20, 21, datetime(2025-02-01), 30, 5] | make-series Value=sum(Value), other=countif(other < 0) default=-2 on Time from startDate to endDate step 7d`,
				`Value,other,Time`, `[[[10,20,0,30,0] [0,0,-2,0,-2] ["2025-01-06T00:00:00Z","2025-01-13T00:00:00Z","2025-01-20T00:00:00Z","2025-01-27T00:00:00Z","2025-02-03T00:00:00Z"]]]`},
			// Grouping, per-series bounds without from/to, and numeric axes.
			{`datatable(g:string, x:long, v:long)["a", 1, 10, "a", 4, 40, "b", 7, 70, "b", 8, 80] | make-series s=sum(v) on x step 2 by g | order by g asc`, `g,s,x`,
				`[[a [10,0,40] [0,2,4]] [b [70,80] [6,8]]]`},
			{`datatable(g:string, x:real)["a", 0.5, "a", 1.7, string(null), 1.1] | make-series n=count() on x from 0 to 2 step 0.5 by g | order by g asc`, `g,n,x`,
				`[[<nil> [0,0,1,0] [0.0,0.5,1.0,1.5]] [a [0,1,0,1] [0.0,0.5,1.0,1.5]]]`},
			{`datatable(x:long)[3, 9] | make-series n=count() on x in range(0, 10, 5)`, `n,x`, `[[[1,1,0] [0,5,10]]]`},
		} {
			result := compiler.Compile(tc.query)
			if !result.OK() {
				t.Fatalf("%s: %#v", tc.query, result.Diagnostics)
			}
			names := []string{}
			for _, column := range result.Columns {
				names = append(names, column.Name)
			}
			if got := strings.Join(names, ","); got != tc.names {
				t.Fatalf("%s: names %s want %s", tc.query, got, tc.names)
			}
			if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
				t.Fatalf("%s:\n got %s\nwant %s\nSQL=%s", tc.query, got, tc.want, result.SQL)
			}
		}
	}
	compiler := ksql.New(dialect.SQLite(dialect.WithDatetimeArithmeticFunctions("test_dt_add", "test_dt_diff")), ksql.WithParameters())
	for _, query := range []string{
		`datatable(t:datetime, v:long)[] | make-series sum(v) on t step 5`,
		`datatable(t:string, v:long)[] | make-series sum(v) on t step 1`,
		`datatable(t:long, v:string)["a"] | make-series make_list(v) on t step 1`,
		`datatable(t:long, v:long)[] | make-series sum(v) on t`,
	} {
		if result := compiler.Compile(query); result.OK() {
			t.Fatalf("%s accepted", query)
		}
	}
}
