package compiler_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/kawijayaa/ksql/sqlast"
)

// Fixes found by running Microsoft's documentation examples: hex literals,
// bracket-quoted as names, column_names_of over function calls, count(x).
func TestDocumentationSyntaxFixes(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters())
	for _, tc := range []struct{ query, want string }{
		{`datatable(num:long)[0xFFFFFFFF, 0x1E1E1E1E, 0xffffffffffffffff] | project num`, `[[4294967295] [505290270] [-1]]`},
		{`print a=0x10 + 1`, `[[17]]`},
		{`print x=1 | as ['Count of events'] | project x`, `[[1]]`},
		{`let MyFunction1 = (param1:int) { print A="", B=1 }; print Columns=column_names_of(MyFunction1(1))`, `[[["A","B"]]]`},
		{`let T = datatable(A:string, B:int) []; print Columns=column_names_of(T)`, `[[["A","B"]]]`},
		{`datatable(x:long)[1, long(null), 3] | summarize count(x), count(), countif(x > 1)`, `[[3 3 1]]`},
		{`datatable(s:string)["555-1", "x"] | extend n = iff(s == "x", string(null), s) | where n !startswith "555" and n !contains "1" and n !endswith "9" | project s`, `[[x]]`},
		{`let Range10 = view () { range MyColumn from 1 to 10 step 1 }; let Range20 = view () { range MyColumn from 1 to 20 step 1 }; let NotView = () { range MyColumn from 1 to 5 step 1 }; search MyColumn == 5 | order by ['$table'] asc`, `[[Range10 5] [Range20 5]]`},
		{`range x from 1 to 3 step 1 | consume`, `[]`},
		{`union withsource=TableName (range x from 1 to 2 step 1 | as T1), (range x from 1 to 1 step 1 | as T2) | order by TableName asc, x asc`, `[[T1 1] [T1 2] [T2 1]]`},
	} {
		result := compiler.Compile(tc.query)
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Fatalf("%s:\n got=%s\nwant=%s\nSQL=%s", tc.query, got, tc.want, result.SQL)
		}
	}
}

// mv-apply subqueries that combine rows run per input row.
func TestSQLiteExecMvApplyPerRow(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters())
	for _, tc := range []struct{ query, want string }{
		{`datatable (i: long, Arr: dynamic)[0, dynamic([1, 5, 3]), 1, dynamic([10, 2])] | mv-apply v=Arr to typeof(long) on (top 1 by v) | project i, v | order by i asc`, `[[0 5] [1 10]]`},
		{`datatable (i: long, Arr: dynamic)[0, dynamic([1, 5, 3]), 1, dynamic([10, 2])] | mv-apply v=Arr to typeof(long) on (take 1 | project v) | count`, `[[2]]`},
		{`datatable (i: long, Arr: dynamic)[0, dynamic([1, 5, 3]), 1, dynamic([10, 2])] | mv-apply v=Arr to typeof(long) on (summarize s=sum(v), n=count()) | project i, s, n | order by i asc`, `[[0 9 3] [1 12 2]]`},
		{`datatable (i: long, Arr: dynamic)[0, dynamic([1, 1, 3])] | mv-apply v=Arr to typeof(long) on (distinct v | count) | project i, Count`, `[[0 2]]`},
	} {
		result := compiler.Compile(tc.query)
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Fatalf("%s:\n got=%s\nwant=%s\nSQL=%s", tc.query, got, tc.want, result.SQL)
		}
	}
	if result := compiler.Compile(`print a=dynamic([1]) | mv-apply a on (join (print a=1) on a)`); len(result.Diagnostics) == 0 || result.Diagnostics[0].Code != "KQLL0334" {
		t.Fatalf("unsupported mv-apply subquery: %v", result.Diagnostics)
	}
}

// set statements: query_take_max_records limits results, query_now fixes
// now(), the bin_auto options define bin_auto, and others only warn.
func TestSetStatements(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters())
	for _, tc := range []struct{ query, want string }{
		{`set query_take_max_records=2; range x from 1 to 5 step 1 | order by x asc`, `[[1] [2]]`},
		{`set truncationmaxrecords = 1; range x from 1 to 5 step 1`, `[[1]]`},
		{`set notruncation; print x=1`, `[[1]]`},
	} {
		result := compiler.Compile(tc.query)
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Fatalf("%s:\n got=%s\nwant=%s\nSQL=%s", tc.query, got, tc.want, result.SQL)
		}
	}
	warned := compiler.Compile(`set notruncation; print x=1`)
	if len(warned.Diagnostics) != 1 || warned.Diagnostics[0].Code != "KQLL0104" || warned.Diagnostics[0].Severity != ksql.SeverityWarning {
		t.Fatalf("set notruncation diagnostics: %v", warned.Diagnostics)
	}
	for _, tc := range []struct{ query, code string }{
		{`print bin_auto(1)`, "KQLB0105"},
		{`set query_take_max_records="a"; print 1`, "KQLB0105"},
		{`set = 1; print 1`, "KQLP0103"},
	} {
		result := compiler.Compile(tc.query)
		if len(result.Diagnostics) == 0 || result.Diagnostics[0].Code != tc.code {
			t.Fatalf("%s: diagnostics %v want %s", tc.query, result.Diagnostics, tc.code)
		}
	}
}

// Microsoft's pattern statement examples, normalization and missing
// references.
func TestPatternStatements(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters())
	country := `declare pattern country = (name:string)[state:string]
{
  ("USA").["New York"] = { print Capital = "Albany" };
  ("USA").["Washington"] = { print Capital = "Olympia" };
  ("Canada").["Alberta"] = { print Capital = "Edmonton" };
};
`
	app := `declare pattern App = (applicationId:string)[scope:string]
{
    ('a1').['Data']    = { range x from 1 to 2 step 1 | project App = "App #1", Data = x };
    ('a2').['Data']    = { range x from 1 to 2 step 1 | project App = "App #2", Data = 10 - x };
};
`
	for _, tc := range []struct{ query, want string }{
		{country + `country("Canada").Alberta`, `[[Edmonton]]`},
		{country + `country('USA').["New York"]`, `[[Albany]]`},
		{app + `union App('a2').Data, App("a1").['Data'] | order by App asc, Data asc`, `[[App #1 1] [App #1 2] [App #2 8] [App #2 9]]`},
		{`declare pattern ip = (address:string) { ("10.10.10.10") = { print Lat=37.405992, Long=-122.078515 }; }; ip("10.10.10.10")`, `[[37.405992 -122.078515]]`},
	} {
		result := compiler.Compile(tc.query)
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Fatalf("%s:\n got=%s\nwant=%s\nSQL=%s", tc.query, got, tc.want, result.SQL)
		}
	}
	for _, tc := range []struct{ query, code, message string }{
		{`declare pattern map_ip_to_longlat; map_ip_to_longlat("10.10.10.10")`, "KQLB0109", `One or more pattern references were not declared. Detected pattern references: ["map_ip_to_longlat('10.10.10.10')"]`},
		{app + `union App("a1").["*"] | count`, "KQLB0109", `One or more pattern references were not declared. Detected pattern references: ["App('a1').['*']"]`},
		{`declare pattern p; declare pattern p; print 1`, "KQLB0106", ""},
		{`declare pattern p = (a:long) { ("1") = { print 1 }; }; print 1`, "KQLP0104", ""},
	} {
		result := compiler.Compile(tc.query)
		if len(result.Diagnostics) == 0 || result.Diagnostics[0].Code != tc.code || (tc.message != "" && result.Diagnostics[0].Message != tc.message) {
			t.Fatalf("%s: diagnostics %v want %s %s", tc.query, result.Diagnostics, tc.code, tc.message)
		}
	}
}

// scan over the serialized input: cumulative state, sequences with match
// ids, and step validation.
func TestSQLiteExecScan(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters())
	for _, tc := range []struct{ query, want string }{
		{`range x from 1 to 4 step 1 | scan declare (c:long=10) with (step s1: true => c = x + s1.c;)`, `[[1 11] [2 13] [3 16] [4 20]]`},
		{`datatable(e:string)["a", "start", "b", "stop", "x", "start", "stop"] | scan with_match_id=id with (step s1: e == "start"; step s2: e == "stop";)`, `[[start 0] [stop 0] [start 1] [stop 1]]`},
		{`range x from 1 to 3 step 1 | scan with (step s1 output=none: true;)`, `[]`},
	} {
		result := compiler.Compile(tc.query)
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Fatalf("%s:\n got=%s\nwant=%s\nSQL=%s", tc.query, got, tc.want, result.SQL)
		}
	}
	for _, tc := range []struct{ query, code string }{
		{`range x from 1 to 3 step 1 | scan with (step s1: true; step s1: true;)`, "KQLB0360"},
		{`range x from 1 to 3 step 1 | scan declare (x:long) with (step s1: true;)`, "KQLB0360"},
		{`range x from 1 to 3 step 1 | scan with (step s1: true => x = 1;)`, "KQLB0360"},
		{`range x from 1 to 3 step 1 | scan with (step s1: s1.nope > 1;)`, "KQLB0360"},
		{`range x from 1 to 3 step 1 | scan with (step s1 output=some: true;)`, "KQLP0359"},
		{`range x from 1 to 3 step 1 | scan declare (c:long) with (step s1: true => c = nope; step s2: true => c = nope;)`, "KQLB0401"},
	} {
		result := compiler.Compile(tc.query)
		if len(result.Diagnostics) == 0 || result.Diagnostics[0].Code != tc.code {
			t.Fatalf("%s: diagnostics %v want %s", tc.query, result.Diagnostics, tc.code)
		}
	}
}

// top-hitters and top-nested over small tables.
func TestSQLiteExecTopHittersAndNested(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters())
	data := `datatable(host:string, user:string, bytes:long)["a", "u1", 10, "a", "u2", 5, "a", "u1", 1, "b", "u3", 100, "b", "u4", 1, "c", "u5", 2]`
	for _, tc := range []struct{ query, columns, want string }{
		{data + ` | top-hitters 2 of host`, `host approximate_count_host`, `[[a 3] [b 2]]`},
		{data + ` | top-hitters 1 of host by bytes`, `host approximate_sum_bytes`, `[[b 101]]`},
		{data + ` | top-nested 2 of host by sum(bytes), top-nested 1 of user by sum(bytes) | order by host asc`, `host aggregated_host user aggregated_user`, `[[a 16 u1 11] [b 101 u3 100]]`},
		{data + ` | top-nested of host by n=count(), top-nested 1 of u=user by s=max(bytes) asc | order by host asc`, `host n u s`, `[[a 3 u2 5] [b 2 u4 1] [c 1 u5 2]]`},
	} {
		result := compiler.Compile(tc.query)
		if !result.OK() {
			t.Fatalf("%s: %v", tc.query, result.Diagnostics)
		}
		names := []string{}
		for _, column := range result.Columns {
			names = append(names, column.Name)
		}
		if got := strings.Join(names, " "); got != tc.columns {
			t.Fatalf("%s: columns %s want %s", tc.query, got, tc.columns)
		}
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Fatalf("%s:\n got=%s\nwant=%s\nSQL=%s", tc.query, got, tc.want, result.SQL)
		}
	}
	if result := compiler.Compile(data + ` | top-nested 1 of host with others = "x" by count()`); len(result.Diagnostics) == 0 || result.Diagnostics[0].Code != "KQLB0362" {
		t.Fatalf("with others: %v", result.Diagnostics)
	}
}

// reduce lowers through the application's pattern aggregates.
func TestReduceLowering(t *testing.T) {
	rule := func(name string) ksql.Option {
		return ksql.WithTypedAggregate(name, func(args []ksql.TypedExpression) (ksql.TypedExpression, error) {
			return ksql.TypedExpression{Type: ksql.TypeDynamic, SQL: &sqlast.Call{Name: name, Args: []sqlast.Expr{args[0].SQL, args[1].SQL, args[2].SQL}}}, nil
		})
	}
	cast := func(name string, result ksql.ScalarType) ksql.Option {
		return ksql.WithTypedFunction(name, func(args []ksql.TypedExpression) (ksql.TypedExpression, error) {
			return ksql.TypedExpression{Type: result, SQL: &sqlast.Call{Name: name, Args: []sqlast.Expr{args[0].SQL}}}, nil
		})
	}
	compiler := ksql.New(dialect.SQLite(), rule("reduce_patterns"), rule("reduce_source"), cast("tostring", ksql.TypeString), cast("tolong", ksql.TypeLong))
	data := `datatable(s:string, n:long)["a b", 1]`
	for _, tc := range []struct{ query, columns string }{
		{data + ` | reduce by s`, `Pattern Count Representative`},
		{data + ` | reduce by s with threshold=0.5, characters="-"`, `Pattern Count Representative`},
		{data + ` | reduce kind=source by strcat(s, n) with threshold=0.2`, `s n Pattern`},
	} {
		result := compiler.Compile(tc.query)
		if !result.OK() {
			t.Fatalf("%s: %v", tc.query, result.Diagnostics)
		}
		names := []string{}
		for _, column := range result.Columns {
			names = append(names, column.Name)
		}
		if got := strings.Join(names, " "); got != tc.columns {
			t.Errorf("%s: columns %s, want %s", tc.query, got, tc.columns)
		}
	}
	for _, tc := range []struct{ query, code string }{
		{data + ` | reduce kind=other by s`, "KQLP0364"},
		{data + ` | reduce s`, "KQLP0364"},
		{data + ` | reduce by s with limit=1`, "KQLP0364"},
		{`T | reduce kind=source by s`, "KQLB0364"},
	} {
		result := compiler.Compile(tc.query)
		if result.OK() || result.Diagnostics[0].Code != tc.code {
			t.Errorf("%s: diagnostics %v, want %s", tc.query, result.Diagnostics, tc.code)
		}
	}
}

// sequence_detect's output columns and argument validation.
func TestSequenceDetectLowering(t *testing.T) {
	compiler := ksql.New(dialect.SQLite(dialect.WithDatetimeArithmeticFunctions("test_dt_add", "test_dt_diff")))
	data := `datatable(T:datetime, E:string, U:string)[datetime(2026-01-01), "a", "u"]`
	result := compiler.Compile(data + ` | evaluate sequence_detect(T, 1h, 2h, a=(E == "a"), E == "b", U, H=strcat(U, "x"))`)
	if !result.OK() {
		t.Fatal(result.Diagnostics)
	}
	names := []string{}
	for _, column := range result.Columns {
		names = append(names, column.Name)
	}
	if got := strings.Join(names, " "); got != "U H a_T Expr2_T Duration" {
		t.Errorf("columns %s", got)
	}
	for _, query := range []string{
		data + ` | evaluate sequence_detect(T, 1h, 2h)`,
		data + ` | evaluate sequence_detect(E, 1h, 2h, E == "a")`,
		data + ` | evaluate sequence_detect(T, 1h, 2h, U)`,
		data + ` | evaluate sequence_detect(T, 1h, 2h, E == "a", U, E == "b")`,
		`T | evaluate sequence_detect(T, 1h, 2h, E == "a")`,
	} {
		if result := compiler.Compile(query); result.OK() {
			t.Errorf("%s: want an error", query)
		}
	}
}

// sliding_window_counts columns and validation; typed datetime expansions
// convert rather than cast; datetime literals accept spaced dashes.
func TestSlidingWindowCountsAndTypedExpansion(t *testing.T) {
	stub := func(name, sqlName string, result ksql.ScalarType) ksql.Option {
		return ksql.WithTypedFunction(name, func(args []ksql.TypedExpression) (ksql.TypedExpression, error) {
			call := &sqlast.Call{Name: sqlName}
			for _, arg := range args {
				call.Args = append(call.Args, arg.SQL)
			}
			return ksql.TypedExpression{Type: result, SQL: call}, nil
		})
	}
	compiler := ksql.New(dialect.SQLite(dialect.WithDatetimeArithmeticFunctions("test_dt_add", "test_dt_diff")),
		stub("todatetime", "host_todatetime", ksql.TypeDateTime), stub("bin_at", "host_bin_at", ksql.TypeDateTime), stub("range", "host_range", ksql.TypeDynamic),
		ksql.WithTypedAggregate("dcount", func(args []ksql.TypedExpression) (ksql.TypedExpression, error) {
			return ksql.TypedExpression{Type: ksql.TypeLong, SQL: &sqlast.Call{Name: "host_dcount", Args: []sqlast.Expr{args[0].SQL}}}, nil
		}))
	data := `datatable(U:string, T:datetime, D:string)["a", datetime(2026-01-01), "x"]`
	result := compiler.Compile(data + ` | evaluate sliding_window_counts(U, T, datetime(2026-01-01), datetime(2026-01-07), 3d, 1d, D)`)
	if !result.OK() {
		t.Fatal(result.Diagnostics)
	}
	names := []string{}
	for _, column := range result.Columns {
		names = append(names, column.Name+":"+string(column.Type))
	}
	if got := strings.Join(names, " "); got != "T:datetime D:string Count:long dcount:long" {
		t.Errorf("columns %s", got)
	}
	for _, query := range []string{
		data + ` | evaluate sliding_window_counts(U, D, 1, 2, 1, 1)`,
		data + ` | evaluate sliding_window_counts(U, T, now(), now(), 7d, "week")`,
		data + ` | evaluate sliding_window_counts(U, T, now(), now(), 7d)`,
		`T | evaluate sliding_window_counts(U, T, now(), now(), 7d, 1d)`,
	} {
		if result := compiler.Compile(query); result.OK() {
			t.Errorf("%s: want an error", query)
		}
	}
	expanded := compiler.Compile(`print x = dynamic(["2026-01-01"]) | mv-expand x to typeof(datetime)`)
	if !expanded.OK() || !strings.Contains(strings.ToLower(expanded.SQL), "host_todatetime") || strings.Contains(expanded.SQL, "TIMESTAMP") {
		t.Errorf("typed datetime expansion: %v\n%s", expanded.Diagnostics, expanded.SQL)
	}
	spaced := compiler.Compile(`print datetime(2017 - 08 - 01 10:00)`)
	if !spaced.OK() || !strings.Contains(spaced.SQL, "2017-08-01 10:00") {
		t.Errorf("spaced datetime literal: %v\n%s", spaced.Diagnostics, spaced.SQL)
	}
}

// union table patterns over views, positional arguments after named ones,
// and guid literals.
func TestUnionPatternsArgumentsAndGuids(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters())
	views := "let View_1 = view () { print x=1 }; let View_2 = view () { print x=2 }; let OtherView_1 = view () { print x=3 };\n"
	for _, tc := range []struct{ query, want string }{
		{views + `union isfuzzy=true View*, SomeView*, OtherView* | count`, `[[3]]`},
		{views + `union withsource=T View* | order by T asc`, `[[View_1 1] [View_2 2]]`},
		{views + `union * | summarize sum(x)`, `[[6]]`},
		{`let f = (a:long, b:long, c:long = 100) { a - b + c }; print f(b=1, 5), f(c=0, 9, 3)`, `[[104 6]]`},
		{`print g = guid({12E8B78D-55B4-46AE-B068-26D7A0080254})`, `[[12e8b78d-55b4-46ae-b068-26d7a0080254]]`},
	} {
		result := compiler.Compile(tc.query)
		if !result.OK() {
			t.Fatalf("%s: %v", tc.query, result.Diagnostics)
		}
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.query, got, tc.want)
		}
	}
	for _, query := range []string{
		views + `union SomeView*`,
		`print guid(12e8b78d)`,
		`let f = (a:long) { a }; print f(a=1, 2)`,
	} {
		if result := compiler.Compile(query); result.OK() {
			t.Errorf("%s: want an error", query)
		}
	}
}

// column_ifexists, bag_pack_columns, table() with parameters and scopes,
// and documented aliases.
func TestColumnFunctionsAndTableReferences(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters())
	for _, tc := range []struct{ query, want string }{
		{`datatable(a:long, b:string)[1, "x"] | project column_ifexists("a", 0), column_ifexists("z", b), columnifexists("b", "")`, `[[1 x x]]`},
		{`let T = datatable(x:long)[1, 2, 3]; let f = (name:string) { table(name) | count }; f("T")`, `[[3]]`},
		{`let T = datatable(x:long)[1]; table("T", "all") | count`, `[[1]]`},
	} {
		result := compiler.Compile(tc.query)
		if !result.OK() {
			t.Fatalf("%s: %v", tc.query, result.Diagnostics)
		}
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.query, got, tc.want)
		}
	}
	for _, query := range []string{
		`datatable(a:long)[1] | project column_ifexists(tostring(a), 0)`,
		`datatable(a:long)[1] | project bag_pack_columns(a + 1)`,
		`let T = datatable(x:long)[1]; table("T", "warm")`,
	} {
		if result := compiler.Compile(query); result.OK() {
			t.Errorf("%s: want an error", query)
		}
	}
}

// Kusto strings are never null, and between (datetime .. timespan) spans
// Start to Start + Span.
func TestNullStringsAndDatetimeBetween(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters())
	data := `datatable(s:string)["a", "", "b"] | extend n = iff(s == "a", string(null), s)`
	for _, tc := range []struct{ query, want string }{
		{data + ` | where n != "b" | count`, `[[2]]`},
		{data + ` | where n == "" | count`, `[[2]]`},
		{data + ` | where n !in ("b") | count`, `[[2]]`},
		{data + ` | where n in ("") | count`, `[[2]]`},
		{data + ` | where n =~ "" | count`, `[[2]]`},
		{data + ` | where n == "b" | count`, `[[1]]`},
		{data + ` | where n == s | count`, `[[2]]`},
	} {
		result := compiler.Compile(tc.query)
		if !result.OK() {
			t.Fatalf("%s: %v", tc.query, result.Diagnostics)
		}
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.query, got, tc.want)
		}
	}
	timed := ksql.New(dialect.SQLite(dialect.WithDatetimeArithmeticFunctions("test_dt_add", "test_dt_diff")))
	result := timed.Compile(`print datetime(2020-01-02) between (datetime(2020-01-01) .. 1d)`)
	if !result.OK() || !strings.Contains(strings.ToLower(result.SQL), "test_dt_add") {
		t.Errorf("datetime between timespan: %v\n%s", result.Diagnostics, result.SQL)
	}
}

// toscalar is computed once per query, even through a function called twice.
func TestToScalarComputedOnce(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters(), ksql.WithTypedFunction("rand", func(args []ksql.TypedExpression) (ksql.TypedExpression, error) {
		return ksql.TypedExpression{Type: ksql.TypeLong, SQL: &sqlast.Call{Name: "random"}}, nil
	}))
	result := compiler.Compile(`let y = () { toscalar(rand()) }; print a = y, b = y, c = toscalar(rand()) | extend same = a == b and b == c`)
	if !result.OK() {
		t.Fatal(result.Diagnostics)
	}
	rows := queryRows(t, db, result)
	if fmt.Sprint(rows[0][3]) != "1" {
		t.Errorf("toscalar values differ: %v\n%s", rows, result.SQL)
	}
	distinct := compiler.Compile(`let f = (n:long) { toscalar(n * 2) }; print a = f(1), b = f(2)`)
	if got := fmt.Sprint(queryRows(t, db, distinct)); got != `[[2 4]]` {
		t.Errorf("toscalar with different arguments: %s", got)
	}
}

// Row limits may be let names or function parameters bound to integers.
func TestLetBoundRowLimits(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, compiler := range []*ksql.Compiler{ksql.New(dialect.SQLite(), ksql.WithParameters()), ksql.New(dialect.SQLite())} {
		for _, tc := range []struct{ query, want string }{
			{`let nn = 2; range x from 1 to 5 step 1 | take nn`, `[[1] [2]]`},
			{`let n = 1; range x from 1 to 5 step 1 | top n by x desc`, `[[5]]`},
			{`let f = (k:long) { range x from 1 to 5 step 1 | order by x asc | take k }; f(3) | count`, `[[3]]`},
		} {
			result := compiler.Compile(tc.query)
			if !result.OK() {
				t.Fatalf("%s: %v", tc.query, result.Diagnostics)
			}
			if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
				t.Errorf("%s: got %s, want %s", tc.query, got, tc.want)
			}
		}
		for _, query := range []string{`let n = 1.5; print 1 | take n`, `let n = -1; print 1 | take n`, `let s = "2"; print 1 | take s`, `print 1 | take rand()`} {
			if result := compiler.Compile(query); result.OK() {
				t.Errorf("%s: want an error", query)
			}
		}
	}
}
