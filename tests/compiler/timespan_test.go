package compiler_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/mattn/go-sqlite3"
)

func init() {
	parse := func(value any) (time.Time, bool) {
		text, ok := value.(string)
		if !ok {
			return time.Time{}, false
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
			if parsed, err := time.Parse(layout, text); err == nil {
				return parsed.UTC(), true
			}
		}
		return time.Time{}, false
	}
	sql.Register("ksql_timespan_sqlite3", &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		if err := c.RegisterFunc("test_dt_add", func(value any, ticks int64) any {
			parsed, ok := parse(value)
			if !ok {
				return nil
			}
			return parsed.Add(time.Duration(ticks) * 100).Format(time.RFC3339Nano)
		}, true); err != nil {
			return err
		}
		return c.RegisterFunc("test_dt_diff", func(left, right any) any {
			a, ok := parse(left)
			b, valid := parse(right)
			if !ok || !valid {
				return nil
			}
			return int64(a.Sub(b) / 100)
		}, true)
	}})
}

func TestTimespanSemantics(t *testing.T) {
	db, err := sql.Open("ksql_timespan_sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := ksql.New(dialect.SQLite(dialect.WithDatetimeArithmeticFunctions("test_dt_add", "test_dt_diff")), ksql.WithParameters())
	for _, tc := range []struct{ query, want, types string }{
		// Microsoft's timespan literal table, as ticks.
		{`print a=2d, b=1.5h, c=30m, d=10s, e=0.1s, f=100ms, g=10microsecond, h=1tick, i=time(15 seconds), j=time(2), k=time(0.12:34:56.7), l=-1h`,
			`[[1728000000000 54000000000 18000000000 100000000 1000000 1000000 100 1 150000000 1728000000000 452967000000 -36000000000]]`,
			`timespan timespan timespan timespan timespan timespan timespan timespan timespan timespan timespan timespan`},
		{`print a=tostring(1d), b=tostring(2h + 30m), c=tostring(-90s), d=tostring(1tick), e=tostring(time(1.02:03:04.5)), f=tostring(timespan(null))`,
			`[[1.00:00:00 02:30:00 -00:01:30 00:00:00.0000001 1.02:03:04.5000000 ]]`, `string string string string string string`},
		{`print a=1d / 1h, b=1h * 2, c=1h * 1.5, d=1h / 2, e=1d % 7h, f=2 * 1m, g=1h - 1m, h=-(1h)`,
			`[[24 72000000000 54000000000 18000000000 108000000000 1200000000 35400000000 -36000000000]]`, `real timespan timespan timespan timespan timespan timespan timespan`},
		{`print a=datetime(2020-01-01) + 1d, b=1h + datetime(2020-01-01), c=datetime(2020-01-01) - 30m, d=datetime(2020-01-02) - datetime(2020-01-01)`,
			`[[2020-01-02T00:00:00Z 2020-01-01T01:00:00Z 2019-12-31T23:30:00Z 864000000000]]`, `datetime datetime datetime timespan`},
		{`print a=datetime(2020-01-02) - datetime(2020-01-01) == 1d, b=datetime(2020-01-02 10:00) - datetime(2020-01-02) > 1h, c=1d > 1h, d=1h between (30m .. 2h)`, `[[1 1 1 1]]`, `bool bool bool bool`},
		{`datatable(n:string, t:timespan)["a", 1h, "b", 2m, "c", time(1.00:00:00)] | sort by t asc | project n, t`, `[[b 1200000000] [a 36000000000] [c 864000000000]]`, `string timespan`},
		{`datatable(t:timespan)[1h, 2h] | summarize s=sum(t), mn=min(t), mx=max(t), a=avg(t), e=sumif(t, false)`, `[[108000000000 36000000000 72000000000 54000000000 0]]`, `timespan timespan timespan timespan timespan`},
		{`datatable(Start:datetime, End:datetime)[datetime(2024-01-01 10:00:00), datetime(2024-01-01 10:05:30)] | extend Duration=End - Start | where Duration > 5m | project d=tostring(Duration)`, `[[00:05:30]]`, `string`},
		{`print t=1h | getschema | project ColumnType, DataType`, `[[timespan System.TimeSpan]]`, `string string`},
	} {
		result := compiler.Compile(tc.query)
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Fatalf("%s:\n got=%s\nwant=%s\nSQL=%s", tc.query, got, tc.want, result.SQL)
		}
		types := []string{}
		for _, column := range result.Columns {
			types = append(types, string(column.Type))
		}
		if got := strings.Join(types, " "); got != tc.types {
			t.Fatalf("%s: types=%s want=%s", tc.query, got, tc.types)
		}
	}
	for _, tc := range []struct{ query, code string }{
		{`print a=datetime(2020-01-01) + datetime(2020-01-01)`, "KQLB0412"},
		{`print a=1h + 1`, "KQLB0412"},
		{`print a=datetime(2020-01-01) * 2`, "KQLB0412"},
		{`print a=1h / datetime(2020-01-01)`, "KQLB0412"},
		{`print a=timespan(abc)`, "KQLB0411"},
		{`print a=time(1.25:00:00)`, "KQLB0411"},
	} {
		if result := compiler.Compile(tc.query); result.OK() || result.Diagnostics[0].Code != tc.code {
			t.Fatalf("%s: %#v, want %s", tc.query, result.Diagnostics, tc.code)
		}
	}
	if result := ksql.New(dialect.SQLite()).Compile(`print a=datetime(2020-01-01) + 1d`); result.OK() || result.Diagnostics[0].Code != "KQLL0412" {
		t.Fatalf("missing capability: %#v", result.Diagnostics)
	}
}
