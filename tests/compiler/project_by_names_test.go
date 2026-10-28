package compiler_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	_ "github.com/mattn/go-sqlite3"
)

// Microsoft's project-by-names examples, with constant lets and edges.
func TestProjectByNames(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters())
	people := `datatable(Name:string, Age:int, City:string, Country:string) ['Peter', 39, 'New York', 'USA']`
	lookup := `let LookupTable = ` + people + `; let LookupColumns = (Source:(Name:string), lookup_columns: dynamic) { Source | lookup LookupTable on Name | project-by-names column_names_of(Source), lookup_columns }; datatable(Name:string, Data:string) ['Peter', 'Source-data'] | invoke LookupColumns(`
	for _, tc := range []struct{ query, columns, want string }{
		{people + ` | project-by-names "Name", "City"`, `Name City`, `[[Peter New York]]`},
		{people + ` | project-by-names dynamic(["Name", "Country"])`, `Name Country`, `[[Peter USA]]`},
		{people + ` | project-by-names "C*"`, `City Country`, `[[New York USA]]`},
		{people + ` | project-by-names "Age", "Nope", "*e", "Age"`, `Age Name`, `[[39 Peter]]`},
		{`let cols = dynamic(["Country", "Name"]); ` + people + ` | project-by-names cols, "Age"`, `Country Name Age`, `[[USA Peter 39]]`},
		{lookup + `dynamic(['Country']))`, `Name Data Country`, `[[Peter Source-data USA]]`},
		{lookup + `dynamic(['Country', 'NonExistent']))`, `Name Data Country`, `[[Peter Source-data USA]]`},
		{`let T = ` + people + `; print n = column_names_of(T)`, `n`, `[[["Name","Age","City","Country"]]]`},
		{people + ` | where Age > 30 | project-by-names "City" | where City != "x"`, `City`, `[[New York]]`},
	} {
		result := compiler.Compile(tc.query)
		if len(result.Diagnostics) != 0 {
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
	for _, tc := range []struct{ query, code string }{
		{people + ` | project-by-names "Nope"`, "KQLB0343"},
		{people + ` | project-by-names strcat(Name, "x")`, "KQLB0345"},
		{`print column_names_of(1)`, "KQLB0413"},
	} {
		result := compiler.Compile(tc.query)
		if len(result.Diagnostics) == 0 || result.Diagnostics[0].Code != tc.code {
			t.Fatalf("%s: diagnostics %v want %s", tc.query, result.Diagnostics, tc.code)
		}
	}
}
