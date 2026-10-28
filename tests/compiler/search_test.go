package compiler_test

import (
	"fmt"
	"sort"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
)

type listedCatalog struct{ testCatalog }

func (c listedCatalog) TableNames() []string {
	names := make([]string, 0, len(c.testCatalog))
	for name := range c.testCatalog {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Kusto's search predicate forms, standalone search over listed tables and
// table patterns, and case sensitivity.
func TestSQLiteExecSearchForms(t *testing.T) {
	db := openInvestigationDB(t)
	listing := ksql.New(dialect.SQLite(dialect.WithRegexFunction("kql_regex"), dialect.WithRegexCaseInsensitiveFlag("(?i)")),
		ksql.WithCatalog(listedCatalog{investigationCatalog()}))
	for _, tc := range []struct{ query, want string }{
		{`UAL | search "alice" | project User`, `[[alice]]`},
		{`UAL | search "ali*" | project User`, `[[alice]]`},
		{`UAL | search "*ice" | project User`, `[[alice]]`},
		{`UAL | search "*lic*" | project User`, `[[alice]]`},
		{`UAL | search "cl*b" | project User`, `[[bob]]`},
		{`UAL | search User:"bob" | project User`, `[[bob]]`},
		{`UAL | search User=="alice" or Host:"client-b" | project User`, `[[alice] [bob]]`},
		{`UAL | search "client" and not("alice") | project User`, `[[bob]]`},
		{`UAL | search * | count`, `[[2]]`},
		{`UAL | search kind=case_sensitive "ALICE" | count`, `[[0]]`},
		{`UAL | search "ALICE" | count`, `[[1]]`},
		{`UAL | search * has "client" | count`, `[[2]]`},
		{`search "alice" | project ['$table'], User | order by ['$table'] asc`, `[[Sysmon alice] [UAL alice]]`},
		{`search in (Sys*) "matched" | project ['$table'], Message`, `[[Sysmon matched]]`},
		{`search in (UAL, Sysmon) User == "bob" | project ['$table'], Host`, `[[UAL client-b]]`},
		{`search "10.10.1.9" | project ['$table'], Host`, `[[Events alpha]]`},
	} {
		result := listing.Compile(tc.query)
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Fatalf("%s:\n got=%s\nwant=%s\nSQL=%s", tc.query, got, tc.want, result.SQL)
		}
	}
	for _, tc := range []struct {
		compiler *ksql.Compiler
		query    string
		code     string
	}{
		{sqliteCompiler(), `search "alice"`, "KQLB0356"},
		{listing, `search in (Nope*) "x"`, "KQLB0356"},
		{listing, `UAL | search User:Host`, "KQLB0351"},
		{listing, `UAL | search in (UAL) "x"`, "KQLP0356"},
		{listing, `search kind=loud "x"`, "KQLP0356"},
	} {
		result := tc.compiler.Compile(tc.query)
		if len(result.Diagnostics) == 0 || result.Diagnostics[0].Code != tc.code {
			t.Fatalf("%s: diagnostics %v want %s", tc.query, result.Diagnostics, tc.code)
		}
	}
}
