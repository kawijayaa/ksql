package dialect

import (
	"testing"

	"github.com/oka/ksql/sqlast"
)

func TestRenderNestedSelect(t *testing.T) {
	query := &sqlast.Select{
		Projections: []sqlast.SelectItem{{Expr: &sqlast.Identifier{Parts: []string{"x"}}}},
		From: &sqlast.Subquery{
			Query: &sqlast.Select{From: &sqlast.Table{Parts: []string{"Events"}}},
			Alias: "_k0",
		},
		Where: &sqlast.Binary{
			Left: &sqlast.Identifier{Parts: []string{"x"}}, Operator: ">",
			Right: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "1"},
		},
		Limit: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "5"},
	}
	got, err := Render(PostgreSQL(), query)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT "x" FROM (SELECT * FROM "Events") AS "_k0" WHERE ("x" > 1) LIMIT 5`
	if got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}

func TestDialectSyntax(t *testing.T) {
	if got := SQLServer().QuoteIdentifier("a]b"); got != "[a]]b]" {
		t.Errorf("SQL Server quote = %q", got)
	}
	if got := MySQL().QuoteIdentifier("a`b"); got != "`a``b`" {
		t.Errorf("MySQL quote = %q", got)
	}
	if got := PostgreSQL().Placeholder(2); got != "$2" {
		t.Errorf("PostgreSQL placeholder = %q", got)
	}
}
