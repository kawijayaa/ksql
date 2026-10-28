package compiler_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/mattn/go-sqlite3"
)

var parseCaptureCalls atomic.Int64

func init() {
	sql.Register("ksql_parse_sqlite3", &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		if err := c.RegisterFunc("test_regex_captures", func(value, pattern string) any {
			parseCaptureCalls.Add(1)
			expression, err := regexp.Compile(pattern)
			if err != nil {
				return nil
			}
			match := expression.FindStringSubmatchIndex(value)
			if match == nil {
				return nil
			}
			groups := make([]any, 0, len(match)/2-1)
			for i := 2; i < len(match); i += 2 {
				if match[i] < 0 {
					groups = append(groups, nil)
				} else {
					groups = append(groups, value[match[i]:match[i+1]])
				}
			}
			encoded, _ := json.Marshal(groups)
			return string(encoded)
		}, true); err != nil {
			return err
		}
		if err := c.RegisterFunc("test_tolong", func(value any) any {
			text, ok := value.(string)
			if !ok {
				return nil
			}
			number, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
			if err != nil {
				return nil
			}
			return number
		}, true); err != nil {
			return err
		}
		if err := c.RegisterFunc("test_toreal", func(value any) any {
			text, ok := value.(string)
			if !ok {
				return nil
			}
			number, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
			if err != nil {
				return nil
			}
			return number
		}, true); err != nil {
			return err
		}
		return c.RegisterFunc("test_todatetime", func(value any) any {
			text, ok := value.(string)
			if !ok {
				return nil
			}
			for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "01/02/2006 15:04:05"} {
				if parsed, err := time.ParseInLocation(layout, strings.TrimSpace(text), time.UTC); err == nil {
					return parsed.UTC().Format("2006-01-02T15:04:05Z")
				}
			}
			return nil
		}, true)
	}})
}

func parseCompiler(mode ksql.ParameterizationMode) *ksql.Compiler {
	return ksql.New(dialect.SQLite(
		dialect.WithRegexFunction("kql_regex"),
		dialect.WithRegexCapturesFunction("test_regex_captures"),
	), ksql.WithParameterization(mode),
		ksql.WithFunction("tolong", ksql.SQLFunction("test_tolong")),
		ksql.WithFunction("toint", ksql.SQLFunction("test_tolong")),
		ksql.WithFunction("toreal", ksql.SQLFunction("test_toreal")),
		ksql.WithFunction("todatetime", ksql.SQLFunction("test_todatetime")),
		ksql.WithCatalog(investigationCatalog()))
}

func openParseDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("ksql_parse_sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

const parseTraces = `let Traces = datatable(EventText: string)[
"Event: NotifySliceRelease (resourceName=PipelineScheduler, totalSlices=27, sliceNumber=23, lockTime=02/17/2016 08:40:01, releaseTime=02/17/2016 08:40:01, previousLockTime=02/17/2016 08:39:01)",
"Event: NotifySliceRelease (resourceName=PipelineScheduler, totalSlices=27, sliceNumber=15, lockTime=02/17/2016 08:40:00, releaseTime=02/17/2016 08:40:00, previousLockTime=02/17/2016 08:39:00)"];
`

func TestParseOperatorExecution(t *testing.T) {
	db := openParseDB(t)
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		compiler := parseCompiler(mode)
		for _, tc := range []struct{ query, want, types string }{
			// Microsoft's simple-mode trace example, including typed and date columns.
			{parseTraces + `Traces | parse EventText with * "resourceName=" resourceName ", totalSlices=" totalSlices: long * "sliceNumber=" sliceNumber: long * "lockTime=" lockTime ", releaseTime=" releaseTime: date "," * "previousLockTime=" previousLockTime: date ")" * | project resourceName, totalSlices, sliceNumber, lockTime, releaseTime, previousLockTime`,
				`[[PipelineScheduler 27 23 02/17/2016 08:40:01 2016-02-17T08:40:01Z 2016-02-17T08:39:01Z] [PipelineScheduler 27 15 02/17/2016 08:40:00 2016-02-17T08:40:00Z 2016-02-17T08:39:00Z]]`,
				`string long long string datetime datetime`},
			// Email alias and domain example; a trailing constant ends the match.
			{`datatable(Contacts: string)["Event: LeadContact (email=john@contosohotel.com, Website=https:contosohotel.com)","Event: LeadContact (email=abi@fourthcoffee.com, Website=https:www.fourthcoffee.com)"] | parse Contacts with * "email=" alias:string "@" domain: string ", Website=https:" WebsiteDomain: string ")" | project EmailAddress=strcat(alias, "@", domain), EmailAlias=alias, WebsiteDomain`,
				`[[john@contosohotel.com john contosohotel.com] [abi@fourthcoffee.com abi www.fourthcoffee.com]]`, `string string string`},
			// Regex mode: string columns are greedy and constants keep their own groups.
			{parseTraces + `Traces | parse kind=regex EventText with "(.*?)[a-zA-Z]*=" resourceName @", totalSlices=\s*\d+\s*.*?sliceNumber=" sliceNumber: long ".*?(previous)?lockTime=" lockTime ".*?releaseTime=" releaseTime ".*?previousLockTime=" previousLockTime: date "\\)" | project resourceName, sliceNumber, lockTime, releaseTime, previousLockTime`,
				`[[PipelineScheduler 23 02/17/2016 08:40:01,  02/17/2016 08:40:01,  2016-02-17T08:39:01Z] [PipelineScheduler 15 02/17/2016 08:40:00,  02/17/2016 08:40:00,  2016-02-17T08:39:00Z]]`, `string long string string datetime`},
			{parseTraces + `Traces | parse kind=regex EventText with * "resourceName=" resourceName ',' * | project resourceName`,
				`[[PipelineScheduler, totalSlices=27, sliceNumber=23, lockTime=02/17/2016 08:40:01, releaseTime=02/17/2016 08:40:01] [PipelineScheduler, totalSlices=27, sliceNumber=15, lockTime=02/17/2016 08:40:00, releaseTime=02/17/2016 08:40:00]]`, `string`},
			{parseTraces + `Traces | parse kind=regex flags=Ui EventText with * "RESOURCENAME=" resourceName ',' * | project resourceName`,
				`[[PipelineScheduler] [PipelineScheduler]]`, `string`},
			// The s flag lets wildcards and columns cross newlines.
			{`datatable(EventText:string)["resourceName=PipelineScheduler\ntotalSlices=27\nlockTime=2016-02-17 08:40:01)"] | parse kind=regex flags=s EventText with * "resourceName=" resourceName: string "(.*?)totalSlices=" totalSlices: long "(.*?)lockTime=" lockTime: datetime "\\)" | project-away EventText`,
				"[[PipelineScheduler\n 27 2016-02-17T08:40:01Z]]", `string long datetime`},
			{`datatable(EventText:string)["a=1\nb=2"] | parse kind=regex EventText with "a=" a:long ".b=" b:long | project a, b`, `[[<nil> <nil>]]`, `long long`},
			// Relaxed mode nulls only failed conversions; simple mode nulls every column.
			{`datatable(s:string)["n=bad, m=5, t=nonValidDateTime, u=x"] | parse kind=relaxed s with "n=" n:long ", m=" m:long ", t=" t:datetime ", u=" u | project n, m, t, u`, `[[<nil> 5 <nil> x]]`, `long long datetime string`},
			// Microsoft's relaxed example: numbers before * still match.
			{parseTraces + `Traces | parse kind=relaxed EventText with * "resourceName=" resourceName ", totalSlices=" totalSlices: long ", sliceNumber=" sliceNumber: long * "lockTime=" lockTime ", releaseTime=" releaseTime: date "," * "previousLockTime=" previousLockTime: date ")" * | project totalSlices, sliceNumber, releaseTime`,
				`[[27 23 2016-02-17T08:40:01Z] [27 15 2016-02-17T08:40:00Z]]`, `long long datetime`},
			{`datatable(s:string)["Event (totalSlices=nonValidLongValue, sliceNumber=20, releaseTime=nonValidDateTime 08:40:01, x)"] | parse kind=relaxed s with * "totalSlices=" totalSlices: long ", sliceNumber=" sliceNumber: long * "releaseTime=" releaseTime: date "," * | project totalSlices, sliceNumber, releaseTime`,
				`[[<nil> 20 <nil>]]`, `long long datetime`},
			{`datatable(s:string)["x=12abc, y=3.5z;"] | parse kind=relaxed s with "x=" x:long ", y=" y:real ";" | project x, y`, `[[<nil> <nil>]]`, `long real`},
			{`datatable(s:string)["n=7, m=5, t=nonValidDateTime, u=x"] | parse s with "n=" n:long ", m=" m:long ", t=" t:datetime ", u=" u | project n, m, t, u`, `[[<nil> <nil> <nil> <nil>]]`, `long long datetime string`},
			{`datatable(s:string)["n=bad, m=5"] | parse s with "n=" n:long ", m=" m:long | project n, m`, `[[<nil> <nil>]]`, `long long`},
			// Unmatched rows are retained with null columns; the match is anchored.
			{`datatable(s:string)["x=1","prefix x=2","x=3 suffix","other"] | parse s with "x=" x:long | project s, x`, `[[x=1 1] [prefix x=2 <nil>] [x=3 suffix <nil>] [other <nil>]]`, `string long`},
			{`datatable(s:string)["x=1 rest","x=-2"] | parse s with "x=" x:long * | project x`, `[[1] [-2]]`, `long`},
			{`datatable(s:string)["a.b|c(d)"] | parse s with a "|" b | project a, b`, `[[a.b c(d)]]`, `string string`},
			{`datatable(s:string)["v=2.5e3;"] | parse s with "v=" v:real ";" | project v`, `[[2500]]`, `real`},
			// Existing columns are replaced in place, like extend.
			{`datatable(a:string, s:string)["old","a=new"] | parse s with "a=" a | project a, s`, `[[new a=new]]`, `string string`},
			{`datatable(s:string)["a=new"] | parse s with "a=" s | project s`, `[[new]]`, `string`},
			// Non-string sources are converted with tostring.
			{`datatable(n:long)[12345] | parse n with "12" rest:long | project rest`, `[[345]]`, `long`},
			{`datatable(s:string)["k=v"] | parse s with [ 'key name' ] "=" ['value'] | project ['key name'], value`, `[[k v]]`, `string string`},
			// parse-where drops unmatched rows and, in strict modes, failed conversions.
			{`datatable(s:string)["x=1","x=bad","other","x="] | parse-where s with "x=" x | project x`, `[[1] [bad] []]`, `string`},
			{`datatable(s:string)["x=1, y=2026-01-02 03:04:05","x=1, y=never","other"] | parse-where s with "x=" x:long ", y=" y:datetime | project x, y`, `[[1 2026-01-02T03:04:05Z]]`, `long datetime`},
			{`datatable(s:string)["ID=ab","id=cd"] | parse-where kind=regex flags=i s with "id=" v | project v`, `[[ab] [cd]]`, `string`},
			// SQL-looking text is data, never query text.
			{`datatable(s:string)["x='); DROP TABLE t; --"] | parse s with "x=" x | project x`, `[['); DROP TABLE t; --]]`, `string`},
			{`datatable(s:string)["a\"b"] | parse s with "a" @'"' rest | project rest`, `[[b]]`, `string`},
		} {
			result := compiler.Compile(tc.query)
			if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
				t.Fatalf("mode=%d %s:\n got=%q\nwant=%q\nSQL=%s", mode, tc.query, got, tc.want, result.SQL)
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

const parseWhereTraces = `let Traces = datatable(EventText: string)[
"Event: NotifySliceRelease (resourceName=PipelineScheduler, totalSlices=27, sliceNumber=invalid_number, lockTime=02/17/2016 08:40:01, releaseTime=02/17/2016 08:40:01, previousLockTime=02/17/2016 08:39:01)",
"Event: NotifySliceRelease (resourceName=PipelineScheduler, totalSlices=27, sliceNumber=15, lockTime=02/17/2016 08:40:00, releaseTime=invalid_datetime, previousLockTime=02/17/2016 08:39:00)",
"Event: NotifySliceRelease (resourceName=PipelineScheduler, totalSlices=27, sliceNumber=20, lockTime=02/17/2016 08:40:01, releaseTime=02/17/2016 08:40:01, previousLockTime=02/17/2016 08:39:01)",
"Event: NotifySliceRelease (resourceName=PipelineScheduler, totalSlices=invalid_number, sliceNumber=16, lockTime=02/17/2016 08:41:00, releaseTime=02/17/2016 08:41:00, previousLockTime=02/17/2016 08:40:00)"];
`

// Microsoft's parse-where page contrasts parse nulls with parse-where filtering.
func TestParseWhereDocumentedExamples(t *testing.T) {
	db := openParseDB(t)
	compiler := parseCompiler(ksql.BoundParameters)
	pattern := ` EventText with * "resourceName=" resourceName ", totalSlices=" totalSlices: long * "sliceNumber=" sliceNumber: long * "lockTime=" lockTime ", releaseTime=" releaseTime: date "," * "previousLockTime=" previousLockTime: date ")" * | project resourceName, totalSlices, sliceNumber, releaseTime`
	for _, tc := range []struct{ query, want string }{
		{parseWhereTraces + `Traces | parse` + pattern, `[[<nil> <nil> <nil> <nil>] [<nil> <nil> <nil> <nil>] [PipelineScheduler 27 20 2016-02-17T08:40:01Z] [<nil> <nil> <nil> <nil>]]`},
		{parseWhereTraces + `Traces | parse-where` + pattern, `[[PipelineScheduler 27 20 2016-02-17T08:40:01Z]]`},
		{parseWhereTraces + `Traces | parse-where kind = regex EventText with * "RESOURCENAME=" resourceName "," * "totalSlices=" totalSlices: long "," * | project resourceName, totalSlices`, `[]`},
		{parseWhereTraces + `Traces | parse-where kind = regex flags=i EventText with * "RESOURCENAME=" resourceName "," * "totalSlices=" totalSlices: long "," * | project resourceName, totalSlices`, `[[PipelineScheduler 27] [PipelineScheduler 27] [PipelineScheduler 27]]`},
	} {
		result := compiler.Compile(tc.query)
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Fatalf("%s:\n got=%s\nwant=%s", tc.query, got, tc.want)
		}
	}
}

func TestParseOperatorSchemaAndPlaceholders(t *testing.T) {
	result := parseCompiler(ksql.BoundParameters).Compile(`datatable(s:string, n:long)["a=1", 2] | parse s with "a=" a:long | getschema | project ColumnName`)
	db := openParseDB(t)
	if got := fmt.Sprint(queryRows(t, db, result)); got != `[[s] [n] [a]]` {
		t.Fatalf("schema = %s", got)
	}
	bound := parseCompiler(ksql.BoundParameters).Compile(`datatable(s:string)["x"] | parse s with "');--" x`)
	if !bound.OK() || strings.Contains(bound.SQL, "');--") {
		t.Fatalf("pattern must be a placeholder: %#v", bound)
	}
}

func TestParseOperatorEvaluatesCapturesPerRow(t *testing.T) {
	db := openParseDB(t)
	parseCaptureCalls.Store(0)
	result := parseCompiler(ksql.BoundParameters).Compile(`datatable(s:string)["a=1 b=2 c=3","a=4 b=5 c=6"] | parse s with "a=" a:long " b=" b:long " c=" c:long | project a, b, c`)
	if got := fmt.Sprint(queryRows(t, db, result)); got != `[[1 2 3] [4 5 6]]` {
		t.Fatalf("rows = %s", got)
	}
	if calls := parseCaptureCalls.Load(); calls != 2 {
		t.Fatalf("capture evaluations = %d, want one per row; SQL=%s", calls, result.SQL)
	}
	// The fence keeps rows streaming: take stops before later rows are parsed.
	parseCaptureCalls.Store(0)
	result = parseCompiler(ksql.BoundParameters).Compile(`datatable(s:string)["a=1","a=2","a=3"] | parse s with "a=" a:long | take 1 | project a`)
	if got := fmt.Sprint(queryRows(t, db, result)); got != `[[1]]` {
		t.Fatalf("rows = %s", got)
	}
	if calls := parseCaptureCalls.Load(); calls != 1 {
		t.Fatalf("capture evaluations after take 1 = %d, want 1", calls)
	}
}

func TestParseOperatorDiagnostics(t *testing.T) {
	compiler := parseCompiler(ksql.BoundParameters)
	for _, tc := range []struct{ query, code string }{
		{`datatable(s:string)[""] | parse kind=fuzzy s with "a" a`, "KQLP0350"},
		{`datatable(s:string)[""] | parse s`, "KQLP0350"},
		{`datatable(s:string)[""] | parse s with`, "KQLP0350"},
		{`datatable(s:string)[""] | parse s with "a" a +`, "KQLP0350"},
		{`datatable(s:string)[""] | parse s with "a" a:`, "KQLP0350"},
		{`datatable(s:string)[""] | parse flags=i s with "a" a`, "KQLP0350"},
		{`datatable(s:string)[""] | parse kind=simple flags=i s with "a" a`, "KQLB0370"},
		{`datatable(s:string)[""] | parse kind=regex flags=x s with "a" a`, "KQLB0370"},
		{`datatable(s:string)[""] | parse-where kind=relaxed s with "a" a`, "KQLB0370"},
		{`datatable(s:string)[""] | parse s with "a" a * "b" b:long`, "KQLB0371"},
		{`datatable(s:string)[""] | parse kind=regex s with "(" a`, "KQLB0372"},
		{`datatable(s:string)[""] | parse s with "a" a:widget`, "KQLB0373"},
		{`datatable(s:string)[""] | parse s with "a" a "b" A`, "KQLB0374"},
		{`datatable(s:string)[""] | parse s with * "a" *`, "KQLB0375"},
		{`datatable(s:string)[""] | parse missing with "a" a`, "KQLB0401"},
	} {
		result := compiler.Compile(tc.query)
		if result.OK() || result.SQL != "" || len(result.Diagnostics) == 0 || result.Diagnostics[0].Code != tc.code {
			t.Fatalf("%s: diagnostics = %#v, want %s", tc.query, result.Diagnostics, tc.code)
		}
	}
	plain := ksql.New(dialect.SQLite()).Compile(`datatable(s:string)["a"] | parse s with "a" a`)
	if plain.OK() || plain.Diagnostics[0].Code != "KQLL0370" {
		t.Fatalf("dialect without captures: %#v", plain.Diagnostics)
	}
	limited := ksql.New(dialect.SQLite(dialect.WithRegexCapturesFunction("test_regex_captures")), ksql.WithLimits(ksql.Limits{MaxRegexBytes: 16})).Compile(`datatable(s:string)["a"] | parse s with "abcdefghijklmnop" a`)
	if limited.OK() || limited.Diagnostics[0].Code != "KQLB0376" {
		t.Fatalf("regex limit: %#v", limited.Diagnostics)
	}
	// User groups in regex constants count toward the capture limit.
	captures := ksql.New(dialect.SQLite(dialect.WithRegexCapturesFunction("test_regex_captures")), ksql.WithLimits(ksql.Limits{MaxParseCaptures: 2}))
	if r := captures.Compile(`datatable(s:string)["a"] | parse kind=regex s with "(a)" a "b" b`); r.OK() || r.Diagnostics[0].Code != "KQLB0375" {
		t.Fatalf("capture limit: %#v", r.Diagnostics)
	}
	if r := captures.Compile(`datatable(s:string)["a"] | parse kind=regex s with "(?:a)" a "b" b`); !r.OK() {
		t.Fatalf("non-capturing groups: %#v", r.Diagnostics)
	}
}

func FuzzParseOperator(f *testing.F) {
	for _, seed := range [][2]string{{`* "a=" a:long *`, "x a=1 y"}, {`"(x)" a "b" b:real`, "xqb2.5"}, {`a "|" b:datetime`, "v|2026-01-01 00:00:00"}, {`"[" a`, "["}} {
		f.Add(seed[0], seed[1])
	}
	db, err := sql.Open("ksql_parse_sqlite3", ":memory:")
	if err != nil {
		f.Fatal(err)
	}
	defer db.Close()
	f.Fuzz(func(t *testing.T, pattern, value string) {
		if len(pattern) > 256 || len(value) > 256 {
			return
		}
		for _, kind := range []string{"simple", "regex", "relaxed"} {
			query := `datatable(s:string)[` + strconv.Quote(value) + `] | parse kind=` + kind + ` s with ` + pattern
			result := parseCompiler(ksql.BoundParameters).Compile(query)
			if !result.OK() {
				if result.SQL != "" {
					t.Fatalf("diagnostics with SQL for %q", query)
				}
				continue
			}
			rows, err := db.Query(result.SQL, result.Args...)
			if err != nil {
				t.Fatalf("Query(%q): %v\nSQL=%s", query, err, result.SQL)
			}
			for rows.Next() {
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows(%q): %v", query, err)
			}
			rows.Close()
		}
	})
}
