package compiler_test

import (
	"strings"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/kawijayaa/ksql/sqlast"
)

func percentileCompiler() *ksql.Compiler {
	rule := func(name string) ksql.TypedFunctionRule {
		return func(args []ksql.TypedExpression) (ksql.TypedExpression, error) {
			values := make([]sqlast.Expr, len(args))
			for i, arg := range args {
				values[i] = arg.SQL
			}
			return ksql.TypedExpression{Type: ksql.TypeReal, SQL: &sqlast.Call{Name: name, Args: values}}, nil
		}
	}
	return ksql.New(dialect.SQLite(), ksql.WithCatalog(investigationCatalog()),
		ksql.WithTypedAggregate("percentile", rule("pct")), ksql.WithTypedAggregate("percentilew", rule("pctw")),
		ksql.WithTypedAggregate("percentiles_array", rule("pcts")), ksql.WithTypedAggregate("hll_merge", rule("merge")),
		ksql.WithScalarOverload("hll_merge", 2), ksql.WithTypedAggregate("covariance", rule("cov")), ksql.WithTypedAggregate("covariancep", rule("covp")))
}

// percentiles expands to one named column per percentile; aggregates with a
// scalar overload are scalar when called with enough arguments.
func TestPercentileExpansionAndScalarOverloads(t *testing.T) {
	compiler := percentileCompiler()
	for _, tc := range []struct{ query, columns, sql string }{
		{`UAL | summarize percentiles(Host, 50, 99.9), percentile(User, 5)`, `percentile_Host_50 percentile_Host_99_9 percentile_User_5`, `pct("Host", 50)`},
		{`UAL | summarize (a, b) = percentiles(Host, 25, 75) by User`, `User a b`, `pct("Host", 75)`},
		{`UAL | summarize percentilesw(Host, 2, 10, 20)`, `percentile_Host_10 percentile_Host_20`, `pctw("Host", 2, 20)`},
		{`UAL | summarize percentiles_array(Host, 1, 2)`, `percentiles_Host`, `pcts("Host", 1, 2)`},
		{`UAL | summarize m = hll_merge(Host)`, `m`, `merge("Host")`},
		{`UAL | summarize covariance(Host, User), covariancep(Host, User)`, `covariance_Host_User covariancep_Host_User`, `cov("Host", "User")`},
		{`UAL | extend m = hll_merge(Host, User)`, `User Host m`, `merge("Host", "User")`},
	} {
		result := compiler.Compile(tc.query)
		if !result.OK() {
			t.Fatalf("%s: %v", tc.query, result.Diagnostics)
		}
		names := []string{}
		for _, column := range result.Columns {
			names = append(names, column.Name)
		}
		if got := strings.Join(names, " "); got != tc.columns || !strings.Contains(strings.ToLower(result.SQL), strings.ToLower(tc.sql)) {
			t.Fatalf("%s: columns %s want %s; SQL %s", tc.query, got, tc.columns, result.SQL)
		}
	}
	for _, tc := range []struct{ query, code string }{
		{`UAL | summarize (a) = percentiles(Host, 25, 75)`, "KQLB0514"},
		{`UAL | extend m = hll_merge(Host)`, "KQLB0511"},
	} {
		result := compiler.Compile(tc.query)
		if len(result.Diagnostics) == 0 || result.Diagnostics[0].Code != tc.code {
			t.Fatalf("%s: diagnostics %v want %s", tc.query, result.Diagnostics, tc.code)
		}
	}
}
