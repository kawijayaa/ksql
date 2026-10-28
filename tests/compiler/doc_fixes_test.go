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
		{`datatable(x:long)[1, long(null), 3] | summarize count(x), count(), countif(x > 1)`, `[[2 3 1]]`},
	} {
		result := compiler.Compile(tc.query)
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Fatalf("%s:\n got=%s\nwant=%s\nSQL=%s", tc.query, got, tc.want, result.SQL)
		}
	}
}
