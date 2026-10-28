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
