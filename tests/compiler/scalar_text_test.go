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
	sql.Register("ksql_scalar_text_sqlite3", &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		return c.RegisterFunc("test_scalar_text", runtime.ScalarText, true)
	}})
}

// SQLite casts reals with 17 significant digits; with a scalar-text UDF,
// tostring and strcat use Kusto's shortest round-trip form.
func TestScalarTextFunction(t *testing.T) {
	db, err := sql.Open("ksql_scalar_text_sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	compiler := ksql.New(dialect.SQLite(dialect.WithScalarTextFunction("test_scalar_text")), ksql.WithParameters())
	query := `print a=tostring(32.15620994567871), b=strcat(1e15, "|", 0.00001, "|", 100.0, "|", 7, "|", dynamic([2.5])[0]), c=tostring(real(null))`
	result := compiler.Compile(query)
	if !strings.Contains(result.SQL, "test_scalar_text(") {
		t.Fatalf("SQL does not use the scalar text function: %s", result.SQL)
	}
	if got := fmt.Sprint(queryRows(t, db, result)); got != "[[32.15620994567871 1E+15|1E-05|100|7|2.5 ]]" {
		t.Fatalf("got %s\nSQL=%s", got, result.SQL)
	}
	plain := ksql.New(dialect.SQLite(), ksql.WithParameters()).Compile(query)
	if !strings.Contains(plain.SQL, "CAST(") || strings.Contains(plain.SQL, "test_scalar_text") {
		t.Fatalf("default SQLite rendering changed: %s", plain.SQL)
	}
}
