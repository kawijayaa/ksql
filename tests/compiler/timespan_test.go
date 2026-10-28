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

func TestTimespanExportedHelpers(t *testing.T) {
	for _, tc := range []struct {
		text      string
		ticks     int64
		formatted string
	}{
		{"1d", 864000000000, "1.00:00:00"}, {"-90s", -900000000, "-00:01:30"}, {"1tick", 1, "00:00:00.0000001"},
		{"1.02:03:04.5", 937845000000, "1.02:03:04.5000000"}, {"0", 0, "00:00:00"},
	} {
		ticks, ok := ksql.ParseTimespan(tc.text)
		if !ok || ticks != tc.ticks || ksql.FormatTimespan(ticks) != tc.formatted {
			t.Fatalf("%s: %d %v %s", tc.text, ticks, ok, ksql.FormatTimespan(ticks))
		}
	}
	if got := ksql.FormatTimespan(-9223372036854775808); got != "-10675199.02:48:05.4775808" {
		t.Fatalf("minimum ticks = %s", got)
	}
	for _, text := range []string{"", "x", "1x", "25:00", "1.2.3", "99999999999999999999d"} {
		if _, ok := ksql.ParseTimespan(text); ok {
			t.Fatalf("%q accepted", text)
		}
	}
}

func TestTypeofLiteral(t *testing.T) {
	compiler := ksql.New(dialect.SQLite(), ksql.WithTypedFunction("kind_of", func(args []ksql.TypedExpression) (ksql.TypedExpression, error) {
		return args[0], nil
	}))
	result := compiler.Compile(`print a=kind_of(typeof(real)), b=kind_of(typeof(Long)), c=kind_of(typeof(timespan)), d=kind_of(typeof(guid))`)
	if !result.OK() || result.SQL != `SELECT 'real' AS "a", 'long' AS "b", 'timespan' AS "c", 'guid' AS "d"` {
		t.Fatalf("%#v %s", result.Diagnostics, result.SQL)
	}
	for _, query := range []string{`print typeof()`, `print typeof(widget)`, `print typeof("real")`, `print typeof(real, long)`} {
		if result := compiler.Compile(query); result.OK() || result.Diagnostics[0].Code != "KQLB0413" {
			t.Fatalf("%s: %#v", query, result.Diagnostics)
		}
	}
}

func TestTemporalRange(t *testing.T) {
	db, err := sql.Open("ksql_timespan_sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		compiler := ksql.New(dialect.SQLite(dialect.WithDatetimeArithmeticFunctions("test_dt_add", "test_dt_diff")), ksql.WithParameterization(mode))
		for _, tc := range []struct{ query, want string }{
			{`range t from 1h to 3h step 1h`, `[[36000000000] [72000000000] [108000000000]]`},
			{`range t from 1h to 3h step 1h | getschema | project ColumnType`, `[[timespan]]`},
			{`range t from 3h to 1h step -1h | count`, `[[3]]`},
		} {
			result := compiler.Compile(tc.query)
			if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
				t.Fatalf("mode=%d %s: got=%s want=%s SQL=%s", mode, tc.query, got, tc.want, result.SQL)
			}
		}
	}
	compiler := ksql.New(dialect.SQLite(dialect.WithDatetimeArithmeticFunctions("test_dt_add", "test_dt_diff")), ksql.WithParameters())
	for _, tc := range []struct{ query, want string }{
		{`range d from datetime(2020-01-01) to datetime(2020-01-03) step 1d`, `[[2020-01-01T00:00:00Z] [2020-01-02T00:00:00Z] [2020-01-03T00:00:00Z]]`},
		{`range d from datetime(2020-01-01) to datetime(2020-01-01 01:00) step 25m | getschema | project ColumnType`, `[[datetime]]`},
		{`range d from datetime(2020-01-01) to datetime(2020-01-01 01:00) step 25m | count`, `[[3]]`},
	} {
		result := compiler.Compile(tc.query)
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Fatalf("%s: got=%s want=%s SQL=%s", tc.query, got, tc.want, result.SQL)
		}
	}
	for _, query := range []string{`range d from datetime(2020-01-01) to 5 step 1d`, `range d from 1h to 3 step 1h`} {
		if result := compiler.Compile(query); result.OK() || result.Diagnostics[0].Code != "KQLB0216" {
			t.Fatalf("%s: %#v", query, result.Diagnostics)
		}
	}
	if result := ksql.New(dialect.SQLite()).Compile(`range d from datetime(2020-01-01) to datetime(2020-01-02) step 1h`); result.OK() || result.Diagnostics[0].Code != "KQLL0412" {
		t.Fatalf("missing capability: %#v", result.Diagnostics)
	}
}
