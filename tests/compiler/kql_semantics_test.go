package compiler_test

import (
	"fmt"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
)

// Documented conversions and membership semantics on SQLite.
func TestDocumentedConversionAndMembership(t *testing.T) {
	db := openInvestigationDB(t)
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		compiler := ksql.New(dialect.SQLite(), ksql.WithParameterization(mode))
		for _, tc := range []struct{ query, want string }{
			{`print a=tostring(true), b=tostring(false), c=tostring(bool(null)), d=tostring(1.5), e=tostring(10), f=tostring(int(null))`, `[[true false  1.5 10 ]]`},
			{`print a=strcat("a", int(null), "b"), b=strcat("x", true, 1), c=strcat(tostring(dynamic(null)))`, `[[ab xtrue1 ]]`},
			{`print a=1 in (dynamic([1,2])), b="a" in (dynamic(["a"])), c=3 !in (1, 2), d="A" in~ (dynamic(["a"])), e="b" in ("a", dynamic(["b", "c"])), f=5 !in (dynamic([1, 2]), 3)`, `[[1 1 1 1 1 1]]`},
			{`print a=1 in (dynamic([])), b="x" in (dynamic(null)), c="a" !in~ (dynamic(["A"]))`, `[[0 0 0]]`},
			{`datatable(value:string, key:string)["b","k"] | where value in (dynamic(["a","b"])) and key in (dynamic(["k"])) | count`, `[[1]]`},
		} {
			result := compiler.Compile(tc.query)
			if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
				t.Fatalf("mode=%d %s: got=%s want=%s\nSQL=%s", mode, tc.query, got, tc.want, result.SQL)
			}
		}
	}
	if result := ksql.New(dialect.SQLite()).Compile(`print a=log(10)`); result.SQL != `SELECT LN(10) AS "a"` {
		t.Fatalf("log must be natural: %s", result.SQL)
	}
}

func TestMakeListEncodesElementsAsTyped(t *testing.T) {
	db := openInvestigationDB(t)
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters())
	result := compiler.Compile(`datatable(d:dynamic, b:bool, t:long)[dynamic({"a":1}), true, 1, dynamic([2]), false, 2, dynamic(null), bool(null), 3] | summarize ds=make_list(d), bs=make_list(b), ts=make_list(t * 1s)`)
	if got := fmt.Sprint(queryRows(t, db, result)); got != `[[[{"a":1},[2]] [true,false] ["00:00:01","00:00:02","00:00:03"]]]` {
		t.Fatalf("rows = %s; SQL=%s", got, result.SQL)
	}
}

// Microsoft's let-statement example: names are case-sensitive, so the scalar
// let region is not shadowed by the column Region.
func TestLetNamesAreCaseSensitive(t *testing.T) {
	db := openInvestigationDB(t)
	compiler := ksql.New(dialect.SQLite(), ksql.WithParameters())
	result := compiler.Compile(`let threshold = 50; let region = "West"; datatable(Name:string, Score:int, Region:string) ["Alice", 45, "West", "Bob", 60, "East", "Charlie", 55, "West", "Dana", 70, "North"] | where Score > threshold and Region == region | project Name`)
	if got := fmt.Sprint(queryRows(t, db, result)); got != `[[Charlie]]` {
		t.Fatalf("rows = %s; SQL=%s", got, result.SQL)
	}
	exact := compiler.Compile(`let Region = "East"; datatable(Region:string)["West","East"] | where Region == "West" | count`)
	if got := fmt.Sprint(queryRows(t, db, exact)); got != `[[1]]` {
		t.Fatalf("an exact-case column must win over a let: %s", got)
	}
}

func TestTabularMembershipAndHints(t *testing.T) {
	db := openInvestigationDB(t)
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		compiler := ksql.New(dialect.SQLite(), ksql.WithCatalog(investigationCatalog()), ksql.WithParameterization(mode))
		for _, tc := range []struct{ query, want string }{
			{`datatable(h:string)["alpha","gamma"] | where h in (Events | project Host) | project h`, `[[alpha]]`},
			{`datatable(h:string)["alpha","gamma"] | where h !in (Events | project Host) | project h`, `[[gamma]]`},
			{`datatable(Host:string)["ALPHA","gamma"] | where Host in~ (Events | where Source != "x" | project Host) | project Host`, `[[ALPHA]]`},
			{`let hosts = Events | where EventType == "process" | project Host; datatable(h:string)["alpha","beta"] | where h in (hosts) | count`, `[[1]]`},
			{`datatable(h:string)["alpha"] | where h !in (datatable(x:string)["b", ""] | where x != "" | union (datatable(x:string)["c"])) | count`, `[[1]]`},
			{`Events | summarize hint.strategy = shuffle n=count() by Host | join kind=inner hint.strategy=broadcast (Events | project Host, Source) on Host | project Host, n | order by Host asc`, `[[alpha 1] [beta 1]]`},
		} {
			result := compiler.Compile(tc.query)
			if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
				t.Fatalf("mode=%d %s: got=%s want=%s\nSQL=%s", mode, tc.query, got, tc.want, result.SQL)
			}
		}
	}
}

func TestFuzzyUnionAndViews(t *testing.T) {
	db := openInvestigationDB(t)
	compiler := ksql.New(dialect.SQLite(), ksql.WithCatalog(investigationCatalog()))
	for _, tc := range []struct{ query, want string }{
		{`union isfuzzy=true Events, Missing | count`, `[[2]]`},
		{`union isfuzzy=true (Missing | where x > 0), (Events | project Host) | count`, `[[2]]`},
		{`let View_1 = view () { print x=1 }; let View_2 = view () { print x=1 }; union isfuzzy=true (View_1 | where x > 0), (View_2 | where x > 0), (Absent | where x > 0) | count`, `[[2]]`},
	} {
		result := compiler.Compile(tc.query)
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Fatalf("%s: got=%s want=%s %#v", tc.query, got, tc.want, result.Diagnostics)
		}
	}
	for _, query := range []string{`union Events, Missing`, `union isfuzzy=true Missing, Absent`, `union isfuzzy=true Events, (Events | where nope > 1)`} {
		if result := compiler.Compile(query); result.OK() {
			t.Fatalf("%s accepted", query)
		}
	}
}
