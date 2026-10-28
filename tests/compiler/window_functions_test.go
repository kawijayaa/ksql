package compiler_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
)

func TestWindowFunctionExecution(t *testing.T) {
	db := openInvestigationDB(t)
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		compiler := ksql.New(dialect.SQLite(), ksql.WithParameterization(mode))
		for _, tc := range []struct{ query, want, types string }{
			// Microsoft's row_number examples.
			{`range a from 1 to 5 step 1 | sort by a desc | extend rn=row_number()`, `[[5 1] [4 2] [3 3] [2 4] [1 5]]`, `long long`},
			{`range a from 1 to 3 step 1 | sort by a desc | extend rn=row_number(7)`, `[[3 7] [2 8] [1 9]]`, `long long`},
			{`datatable (Airport:string, Airline:string, Departures:long)["TLV", "LH", 1, "TLV", "LY", 100, "SEA", "LH", 1, "SEA", "BA", 2, "SEA", "LY", 0] | sort by Airport asc, Departures desc | extend Rank=row_number(1, prev(Airport) != Airport)`,
				`[[SEA BA 2 1] [SEA LH 1 2] [SEA LY 0 3] [TLV LY 100 1] [TLV LH 1 2]]`, `string string long long`},
			// row_cumsum, including serialize with an assignment and restarts.
			{`datatable (a:long) [1, 2, 3, 4, 5, 6, 7, 8, 9, 10] | where a%2==0 | sort by a asc | serialize cs=row_cumsum(a)`, `[[2 2] [4 6] [6 12] [8 20] [10 30]]`, `long long`},
			{`datatable (name:string, month:int, salary:long)["Alice", 1, 1000, "Bob", 1, 1000, "Alice", 2, 2000, "Bob", 2, 1950, "Alice", 3, 1400, "Bob", 3, 1450] | order by name asc, month asc | extend total=row_cumsum(salary, name != prev(name))`,
				`[[Alice 1 1000 1000] [Alice 2 2000 3000] [Alice 3 1400 4400] [Bob 1 1000 1000] [Bob 2 1950 2950] [Bob 3 1450 4400]]`, `string int long long`},
			{`datatable (v:real) [1.5, 2.25] | sort by v asc | extend c=row_cumsum(v)`, `[[1.5 1.5] [2.25 3.75]]`, `real real`},
			// row_rank_dense and row_rank_min examples.
			{`datatable (Airport:string, Airline:string, Departures:long)["SEA", "LH", 3, "SEA", "LY", 100, "SEA", "UA", 3, "SEA", "BA", 2, "SEA", "EL", 3] | sort by Departures asc, Airline asc | extend Rank=row_rank_dense(Departures)`,
				`[[SEA BA 2 1] [SEA EL 3 2] [SEA LH 3 2] [SEA UA 3 2] [SEA LY 100 3]]`, `string string long long`},
			{`datatable (Airport:string, Airline:string, Departures:long)["SEA", "LH", 3, "SEA", "LY", 100, "SEA", "UA", 3, "SEA", "BA", 2, "SEA", "EL", 3, "AMS", "EL", 1, "AMS", "BA", 1] | sort by Airport desc, Departures asc, Airline asc | extend Rank=row_rank_dense(Departures, prev(Airport) != Airport)`,
				`[[SEA BA 2 1] [SEA EL 3 2] [SEA LH 3 2] [SEA UA 3 2] [SEA LY 100 3] [AMS BA 1 1] [AMS EL 1 1]]`, `string string long long`},
			{`datatable (Airport:string, Airline:string, Departures:long)["SEA", "LH", 3, "SEA", "LY", 100, "SEA", "UA", 3, "SEA", "BA", 2, "SEA", "EL", 3] | sort by Departures asc, Airline asc | extend Rank=row_rank_min(Departures)`,
				`[[SEA BA 2 1] [SEA EL 3 2] [SEA LH 3 2] [SEA UA 3 2] [SEA LY 100 5]]`, `string string long long`},
			{`datatable(Category:string, Item:string, Value:int)["A", "item1", 10, "A","item2", 10, "A", "item3", 5, "A", "item4", 20, "B", "item2", 5, "B", "item1", 7] | sort by Category asc, Value asc, Item asc | extend rank = row_rank_min(Value, Category != prev(Category))`,
				`[[A item3 5 1] [A item1 10 2] [A item2 10 2] [A item4 20 4] [B item2 5 1] [B item1 7 2]]`, `string string int long`},
			// prev and next with offsets, defaults, nesting, and filtering on adjacent rows.
			{`range a from 1 to 4 step 1 | sort by a asc | extend p=prev(a), p2=prev(a, 2, -1), n=next(a), d=a - prev(a)`, `[[1 <nil> -1 2 <nil>] [2 1 -1 3 1] [3 2 1 4 1] [4 3 2 <nil> 1]]`, `long long long long long`},
			{`datatable(t:long, host:string)[1,"a",2,"a",10,"a",11,"a",30,"a"] | sort by t asc | where t - prev(t) > 5 | project t`, `[[10] [30]]`, `long`},
			{`datatable(t:long)[3,1,2] | sort by t asc | project t, gap=next(t) - t`, `[[1 1] [2 1] [3 <nil>]]`, `long long`},
			{`range a from 1 to 3 step 1 | sort by a desc | extend x=prev(prev(a))`, `[[3 <nil>] [2 <nil>] [1 3]]`, `long long`},
			// Serialization survives where, take, project and extend of other columns.
			{`range a from 1 to 6 step 1 | sort by a desc | where a % 2 == 0 | extend b=a*10 | project a, b | take 3 | extend rn=row_number()`, `[[6 60 1] [4 40 2] [2 20 3]]`, `long long long`},
			{`range a from 1 to 3 step 1 | serialize | extend rn=row_number() | summarize max(rn)`, `[[3]]`, `long`},
			{`range a from 1 to 3 step 1 | top 2 by a desc | extend rn=row_number()`, `[[3 1] [2 2]]`, `long long`},
			// Window functions can be used together; a user function named prev keeps precedence.
			{`range a from 1 to 3 step 1 | sort by a asc | extend rn=row_number(), cs=row_cumsum(a), p=prev(a)`, `[[1 1 1 <nil>] [2 2 3 1] [3 3 6 2]]`, `long long long long`},
			{`datatable(s:string)["a","b"] | sort by s asc | extend p=prev(s), n=next(s), e=strlen(prev(s))`, `[[a  b 0] [b a  1]]`, `string string string long`},
			{`let prev=(x:long){x*100}; range a from 1 to 2 step 1 | extend p=prev(a)`, `[[1 100] [2 200]]`, `long long`},
		} {
			result := compiler.Compile(tc.query)
			if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
				t.Fatalf("mode=%d %s:\n got=%s\nwant=%s\nSQL=%s", mode, tc.query, got, tc.want, result.SQL)
			}
			types := []string{}
			for _, column := range result.Columns {
				types = append(types, string(column.Type))
			}
			if got := strings.Join(types, " "); got != tc.types {
				t.Fatalf("%s: types=%s want=%s", tc.query, got, tc.types)
			}
		}
	}
}

func TestWindowFunctionDiagnostics(t *testing.T) {
	compiler := ksql.New(dialect.SQLite())
	for _, tc := range []struct{ query, code string }{
		{`range a from 1 to 3 step 1 | extend p=prev(a)`, "KQLB0390"},
		{`range a from 1 to 3 step 1 | sort by a asc | summarize p=prev(a)`, "KQLB0391"},
		{`range a from 1 to 3 step 1 | sort by a asc | summarize count() | extend rn=row_number()`, "KQLB0390"},
		{`range a from 1 to 3 step 1 | sort by a asc | extend a=a+1 | extend p=prev(a)`, "KQLB0390"},
		{`range a from 1 to 3 step 1 | sort by a asc | project b=a | extend p=prev(b)`, "KQLB0390"},
		{`range a from 1 to 3 step 1 | sort by a asc | extend p=prev()`, "KQLB0393"},
		{`range a from 1 to 3 step 1 | sort by a asc | extend p=prev(a, a)`, "KQLB0394"},
		{`range a from 1 to 3 step 1 | sort by a asc | extend p=prev(a, -1)`, "KQLB0394"},
		{`range a from 1 to 3 step 1 | sort by a asc | extend r=row_number(a)`, "KQLB0394"},
		{`range a from 1 to 3 step 1 | sort by a asc | extend r=row_number(1, 5)`, "KQLB0392"},
		{`range a from 1 to 3 step 1 | sort by a asc | extend c=row_cumsum("x")`, "KQLB0395"},
		{`range a from 1 to 3 step 1 | sort by a asc | extend s=row_window_session(a, 1h, 1h)`, "KQLL0390"},
	} {
		result := compiler.Compile(tc.query)
		if result.OK() || result.SQL != "" || result.Diagnostics[0].Code != tc.code {
			t.Fatalf("%s: diagnostics = %#v, want %s", tc.query, result.Diagnostics, tc.code)
		}
	}
}
