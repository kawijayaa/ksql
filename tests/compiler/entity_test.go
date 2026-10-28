package compiler_test

import (
	"fmt"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
)

// table("T") resolves exactly like T: catalog names only, never an arbitrary
// storage table such as sqlite_master.
func TestEntityFunctionsUseCatalogResolution(t *testing.T) {
	db := openInvestigationDB(t)
	compiler := ksql.New(dialect.SQLite(), ksql.WithCatalog(investigationCatalog()))
	result := compiler.Compile(`table("Events") | count`)
	if got := fmt.Sprint(queryRows(t, db, result)); got != `[[2]]` {
		t.Fatalf("rows = %s; SQL=%s", got, result.SQL)
	}
	if result.Columns[0].Name != "Count" {
		t.Fatalf("columns = %#v", result.Columns)
	}
	for _, query := range []string{`table("sqlite_master")`, `materialized_view("sqlite_master") | take 1`, `external_table("investigation_progress")`} {
		if result := compiler.Compile(query); result.OK() || result.SQL != "" || result.Diagnostics[0].Code != "KQLB0201" {
			t.Fatalf("%s: %#v %s", query, result.Diagnostics, result.SQL)
		}
	}
	if result := compiler.Compile(`table("Events") | project Host | getschema | project ColumnType`); !result.OK() {
		t.Fatalf("schema-bound entity: %#v", result.Diagnostics)
	}
}
