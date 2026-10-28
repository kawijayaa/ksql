package compiler_test

import (
	"fmt"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
)

// contains/startswith/endswith match literally: LIKE wildcards in the search
// text are ordinary characters, and _cs operators are case-sensitive.
func TestStringMatchOperatorsAreLiteral(t *testing.T) {
	db := openInvestigationDB(t)
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		compiler := ksql.New(dialect.SQLite(), ksql.WithParameterization(mode))
		for _, tc := range []struct{ query, want string }{
			{`print a="abc" contains "B", b="abc" contains_cs "B", c="abc" contains_cs "b", d="abc" !contains "z", e="abc" !contains_cs "B"`, `[[1 0 1 1 1]]`},
			{`print a="abc" startswith "AB", b="abc" startswith_cs "AB", c="abc" startswith_cs "ab", d="abc" endswith "BC", e="abc" endswith_cs "BC", f="abc" endswith_cs "bc"`, `[[1 0 1 1 0 1]]`},
			{`print a="abc" contains "_", b="abc" contains "%", c="a_c" contains "_", d="100%" endswith "0%", e="abc" startswith "%", f="a%c" startswith "a%"`, `[[0 0 1 1 0 1]]`},
			{`print a="abc" contains "", b="abc" startswith "", c="abc" endswith "", d="" endswith "a", e="ab" endswith "abc", f="abc" startswith "abcd"`, `[[1 1 1 0 0 0]]`},
			{`print a="a\\b" contains "\\", b="aXbXc" endswith "Xc", c="xyxy" endswith "xy", d="ab" !startswith "a", e="ab" !endswith_cs "B"`, `[[1 1 1 0 1]]`},
			{`datatable(s:string, p:string)["Error: disk", "error", "warn", "ERR"] | where s startswith p | project s`, `[[Error: disk]]`},
		} {
			result := compiler.Compile(tc.query)
			if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
				t.Fatalf("mode=%d %s: got=%s want=%s\nSQL=%s args=%v", mode, tc.query, got, tc.want, result.SQL, result.Args)
			}
		}
	}
	for _, target := range []dialect.Dialect{dialect.PostgreSQL(), dialect.MySQL(), dialect.SQLServer()} {
		if result := ksql.New(target).Compile(`print a="abc" contains "_"`); !result.OK() {
			t.Fatalf("%s: %#v", target.Name(), result.Diagnostics)
		}
	}
}
