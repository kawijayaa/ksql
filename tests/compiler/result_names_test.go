package compiler_test

import (
	"strings"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/kawijayaa/ksql/sqlast"
)

// Default column names follow Kusto.Language's ResultNameKind metadata.
func TestDefaultResultNames(t *testing.T) {
	compiler := ksql.New(dialect.SQLite(), ksql.WithTypedFunction("bin", func(args []ksql.TypedExpression) (ksql.TypedExpression, error) {
		return ksql.TypedExpression{Type: args[0].Type, SQL: &sqlast.Call{Name: "kql_bin", Args: []sqlast.Expr{args[0].SQL, args[1].SQL}}}, nil
	}))
	for _, tc := range []struct{ query, names string }{
		{`datatable(a:long, t:long)[1,2] | summarize count(), sum(a), avg(a), min(a), max(a), countif(a > 1), sumif(a, true), make_list(a) by bin(t, 1)`,
			`t,count_,sum_a,avg_a,min_a,max_a,countif_,sumif_a,list_a`},
		{`datatable(a:long)[1] | summarize sum(a * 2), count()`, `sum_,count_`},
		{`datatable(s:string)["a"] | extend tolower(s), strlen(s), tostring(s), iff(true, 1, 2), toupper(s)`, `s,Column1,strlen_s,Column2,Column3`},
		// project keeps generated ColumnN names for unnamed calls; inferring
		// them needs Kusto's numeric suffixes for colliding inferred names.
		{`datatable(s:string)["a"] | project strlen(s), bin(strlen(s), 1)`, `strlen_s,strlen_s1`},
		{`datatable(s:string)["a"] | project strlen(s), tolower(s), s`, `strlen_s,Column1,s`},
	} {
		result := compiler.Compile(tc.query)
		if !result.OK() {
			t.Fatalf("%s: %#v", tc.query, result.Diagnostics)
		}
		names := []string{}
		for _, column := range result.Columns {
			names = append(names, column.Name)
		}
		if got := strings.Join(names, ","); got != tc.names {
			t.Fatalf("%s: names=%s want=%s", tc.query, got, tc.names)
		}
	}
}
