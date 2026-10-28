package compiler_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
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
