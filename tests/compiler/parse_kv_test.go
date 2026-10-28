package compiler_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/kawijayaa/ksql/runtime"
	"github.com/mattn/go-sqlite3"
)

func init() {
	sql.Register("ksql_parse_kv_sqlite3", &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		if err := c.RegisterFunc("test_key_values", runtime.KeyValuesJSON, true); err != nil {
			return err
		}
		return c.RegisterFunc("test_tolong", func(value any) any {
			text, ok := value.(string)
			if !ok {
				return nil
			}
			var number int64
			if _, err := fmt.Sscan(text, &number); err != nil || fmt.Sprint(number) != text {
				return nil
			}
			return number
		}, true)
	}})
}

func parseKVCompiler() *ksql.Compiler {
	return ksql.New(dialect.SQLite(dialect.WithKeyValuesFunction("test_key_values")), ksql.WithParameters(),
		ksql.WithFunction("tolong", ksql.SQLFunction("test_tolong")))
}

func TestParseKVExecution(t *testing.T) {
	db, err := sql.Open("ksql_parse_kv_sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := parseKVCompiler()
	for _, tc := range []struct{ query, want, types string }{
		// Microsoft's documented examples (datetime columns are read as strings here).
		{`print str="ThreadId:458745723, Machine:Node001, Text: The service is up, Level: Info" | parse-kv str as (Text: string, ThreadId:long, Machine: string) with (pair_delimiter=',', kv_delimiter=':') | project-away str`,
			`[[The service is up 458745723 Node001]]`, `string long string`},
		{`print str='src=10.1.1.123 dst=10.1.1.124 bytes=125 failure="connection aborted" "event time"=2021-01-01T10:00:54' | parse-kv str as (['event time']:string, src:string, dst:string, bytes:long, failure:string) with (pair_delimiter=' ', kv_delimiter='=', quote='"') | project-away str`,
			`[[2021-01-01T10:00:54 10.1.1.123 10.1.1.124 125 connection aborted]]`, `string string string long string`},
		{`print str='src=10.1.1.123 dst=10.1.1.124 bytes=125 failure=(connection aborted) (event time)=(2021-01-01 10:00:54)' | parse-kv str as (['event time']:string, failure:string) with (pair_delimiter=' ', kv_delimiter='=', quote='()') | project-away str`,
			`[[2021-01-01 10:00:54 connection aborted]]`, `string string`},
		{`print str='src=10.1.1.123 dst=10.1.1.124 bytes=125 failure="the remote host sent \\"bye!\\"" time=2021-01-01T10:00:54' | parse-kv str as (['time']:string, failure:string) with (pair_delimiter=' ', kv_delimiter='=', quote='"', escape='\\') | project-away str`,
			`[[2021-01-01T10:00:54 the remote host sent "bye!"]]`, `string string`},
		{`print str='name=John Doe phone=555 5555 city=New York' | parse-kv str as (name:string, phone:string, city:string) with (pair_delimiter=' ', kv_delimiter='=') | project-away str`,
			`[[John 555 New]]`, `string string string`},
		{`print str='name=John Doe phone=555 5555 city=New York' | parse-kv str as (name:string, phone:string, city:string) with (pair_delimiter=' ', kv_delimiter='=', greedy=true) | project-away str`,
			`[[John Doe 555 5555 New York]]`, `string string string`},
		{`print str="2021-01-01T10:00:34 [INFO] ThreadId:458745723, Machine:Node001, Text: Started" | parse-kv str as (Text: string, ThreadId:long, Machine: string) | project-away str`,
			`[[Started 458745723 Node001]]`, `string long string`},
		{`print str="2021-01-01T10:00:34 [INFO] ThreadId:458745723, Machine:Node001, Text: 'The service \\' is up'" | parse-kv str as (Text: string, ThreadId:long, Machine: string) with (quote="'", escape='\\') | project-away str`,
			`[[The service ' is up 458745723 Node001]]`, `string long string`},
		{`print str=@'["referer url: https://hostname.com/redirect?dest=/?h=1234", "request url: https://hostname.com/?h=1234", "advertiser id: 24fefbca-cf27-4d62-a623-249c2ad30c73"]' | parse-kv str as (['referer url']:string, ['request url']:string, ['advertiser id']: string) with (regex=@'"([\w ]+)\s*:\s*([^"]*)"') | project-away str`,
			`[[https://hostname.com/redirect?dest=/?h=1234 https://hostname.com/?h=1234 24fefbca-cf27-4d62-a623-249c2ad30c73]]`, `string string string`},
		// Missing keys and failed conversions are null; existing columns are replaced in place.
		{`datatable(a:string, s:string)["old","a=1 b=x"] | parse-kv s as (a:long, b:long, c) with (pair_delimiter=' ', kv_delimiter='=') | project a, b, c, s`, `[[1 <nil> <nil> a=1 b=x]]`, `long long string string`},
		{`datatable(n:long)[5] | parse-kv n as (x) | project n, x`, `[[5 <nil>]]`, `long string`},
		{`datatable(s:string)['k"q=v'] | parse-kv s as (['k"q']) with (pair_delimiter=' ', kv_delimiter='=') | project ['k"q']`, `[[v]]`, `string`},
		{`datatable(s:string)["user='); DROP TABLE x; --"] | parse-kv s as (user) with (pair_delimiter=' ', kv_delimiter='=') | project user`, `[[');]]`, `string`},
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
}

func TestParseKVDiagnostics(t *testing.T) {
	compiler := parseKVCompiler()
	for _, tc := range []struct{ query, code string }{
		{`print s="" | parse-kv s`, "KQLP0351"},
		{`print s="" | parse-kv s as ()`, "KQLP0351"},
		{`print s="" | parse-kv s as (a:)`, "KQLP0351"},
		{`print s="" | parse-kv s as (a) with (pair_delimiter)`, "KQLP0351"},
		{`print s="" | parse-kv s as (a) using (x='y')`, "KQLP0351"},
		{`print s="" | parse-kv s as (a) with (pair_delimiter=',')`, "KQLB0380"},
		{`print s="" | parse-kv s as (a) with (pair_delimiter='', kv_delimiter='=')`, "KQLB0380"},
		{`print s="" | parse-kv s as (a) with (greedy=true)`, "KQLB0380"},
		{`print s="" | parse-kv s as (a) with (greedy=maybe, pair_delimiter=',', kv_delimiter=':')`, "KQLB0380"},
		{`print s="" | parse-kv s as (a) with (regex='(a)')`, "KQLB0380"},
		{`print s="" | parse-kv s as (a) with (regex='(a)(b)', quote='"')`, "KQLB0380"},
		{`print s="" | parse-kv s as (a) with (quote='abc')`, "KQLB0380"},
		{`print s="" | parse-kv s as (a) with (escape='\\')`, "KQLB0380"},
		{`print s="" | parse-kv s as (a) with (pair_delimiter=',', pair_delimiter=';', kv_delimiter=':')`, "KQLB0380"},
		{`print s="" | parse-kv s as (a) with (separator=',')`, "KQLB0380"},
		{`print s="" | parse-kv s as (a:widget)`, "KQLB0373"},
		{`print s="" | parse-kv s as (a, A)`, "KQLB0374"},
	} {
		result := compiler.Compile(tc.query)
		if result.OK() || result.SQL != "" || result.Diagnostics[0].Code != tc.code {
			t.Fatalf("%s: diagnostics = %#v, want %s", tc.query, result.Diagnostics, tc.code)
		}
	}
	if result := ksql.New(dialect.SQLite()).Compile(`print s="a=1" | parse-kv s as (a)`); result.OK() || result.Diagnostics[0].Code != "KQLL0371" {
		t.Fatalf("dialect without key values: %#v", result.Diagnostics)
	}
}
