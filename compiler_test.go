package ksql_test

import (
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/kawijayaa/ksql/features"
)

func TestCompilePipeline(t *testing.T) {
	compiler := ksql.New(dialect.PostgreSQL())
	result := compiler.Compile(`let cutoff = 10;
Events
| where severity >= cutoff and message contains "error"
| extend day = floor(duration)
| summarize total = count(), average = avg(duration) by service
| sort by total desc
| take 5`)
	if !result.OK() {
		t.Fatalf("Compile() diagnostics = %#v", result.Diagnostics)
	}
	want := `SELECT * FROM (SELECT * FROM (SELECT "service" AS "service", COUNT(*) AS "total", AVG("duration") AS "average" FROM (SELECT *, FLOOR("duration") AS "day" FROM (SELECT * FROM (SELECT * FROM "Events") AS "_k0" WHERE (("severity" >= 10) AND (LOWER("message") LIKE LOWER(CONCAT('%', 'error', '%'))))) AS "_k1") AS "_k2" GROUP BY "service") AS "_k3" ORDER BY "total" DESC NULLS LAST) AS "_k4" LIMIT 5`
	if result.SQL != want {
		t.Fatalf("Compile() SQL =\n%s\nwant:\n%s", result.SQL, want)
	}
	for _, used := range result.Features {
		if _, ok := features.Lookup(used.ID); !ok {
			t.Errorf("runtime feature %q is absent from ledger", used.ID)
		}
	}
}

func TestCompilePrint(t *testing.T) {
	result := ksql.New(dialect.SQLite()).Compile(`print answer=40+2, label="ok"`)
	if !result.OK() {
		t.Fatalf("Compile() diagnostics = %#v", result.Diagnostics)
	}
	if result.SQL != `SELECT (40 + 2) AS "answer", 'ok' AS "label"` {
		t.Fatalf("Compile() SQL = %q", result.SQL)
	}
}

func TestUnsupportedFeatureIsDiagnostic(t *testing.T) {
	result := ksql.New(dialect.PostgreSQL()).Compile(`T | evaluate python(typeof(*), code)`)
	if result.OK() || result.SQL != "" {
		t.Fatalf("Compile() = %#v", result)
	}
	if got := result.Diagnostics[0].Feature; got != "operator.evaluate" {
		t.Fatalf("feature = %q", got)
	}
}

func TestUnknownFunctionIsNotPassedThrough(t *testing.T) {
	result := ksql.New(dialect.PostgreSQL()).Compile(`T | project mystery(x)`)
	if result.OK() || result.SQL != "" {
		t.Fatalf("Compile() = %#v", result)
	}
	if got := result.Diagnostics[0].Code; got != "KQLL0503" {
		t.Fatalf("code = %q", got)
	}
}

func TestCompileJoin(t *testing.T) {
	result := ksql.New(dialect.PostgreSQL()).Compile(`T | join kind=inner (U | where active == true) on key`)
	if !result.OK() {
		t.Fatalf("Compile() diagnostics = %#v", result.Diagnostics)
	}
	want := `SELECT * FROM (SELECT * FROM "T") AS "_k0" INNER JOIN (SELECT * FROM (SELECT * FROM "U") AS "_k1" WHERE ("active" = TRUE)) AS "_k2" USING ("key")`
	if result.SQL != want {
		t.Fatalf("Compile() SQL = %q, want %q", result.SQL, want)
	}
}

func TestCompileQualifiedJoin(t *testing.T) {
	result := ksql.New(dialect.PostgreSQL()).Compile(`T | join kind=leftouter (U) on $left.key == $right.other_key`)
	if !result.OK() {
		t.Fatalf("Compile() diagnostics = %#v", result.Diagnostics)
	}
	want := `SELECT * FROM (SELECT * FROM "T") AS "_k0" LEFT JOIN (SELECT * FROM "U") AS "_k1" ON ("_k0"."key" = "_k1"."other_key")`
	if result.SQL != want {
		t.Fatalf("Compile() SQL = %q, want %q", result.SQL, want)
	}
}

func TestCompileDatatable(t *testing.T) {
	result := ksql.New(dialect.PostgreSQL()).Compile(`datatable(id:long, name:string)[1, "one", 2, "two"] | where id > 1`)
	if !result.OK() {
		t.Fatalf("Compile() diagnostics = %#v", result.Diagnostics)
	}
	want := `SELECT * FROM (SELECT * FROM (VALUES (1, 'one'), (2, 'two')) AS "_k0"("id", "name")) AS "_k1" WHERE ("id" > 1)`
	if result.SQL != want {
		t.Fatalf("Compile() SQL = %q, want %q", result.SQL, want)
	}
}

func TestCompileRange(t *testing.T) {
	result := ksql.New(dialect.PostgreSQL()).Compile(`range id from 1 to 5 step 2`)
	if !result.OK() {
		t.Fatalf("Compile() diagnostics = %#v", result.Diagnostics)
	}
	want := `SELECT * FROM generate_series(1, 5, 2) AS "_k0"("id")`
	if result.SQL != want {
		t.Fatalf("Compile() SQL = %q, want %q", result.SQL, want)
	}
}

func TestCompileTabularLet(t *testing.T) {
	result := ksql.New(dialect.PostgreSQL()).Compile(`let Active = Events | where active == true; Active | project id | sort by id`)
	if !result.OK() {
		t.Fatalf("Compile() diagnostics = %#v", result.Diagnostics)
	}
	want := `SELECT * FROM (SELECT "id" FROM (SELECT * FROM (SELECT * FROM "Events") AS "_k0" WHERE ("active" = TRUE)) AS "_k1") AS "_k2" ORDER BY "id" DESC NULLS LAST`
	if result.SQL != want {
		t.Fatalf("Compile() SQL = %q, want %q", result.SQL, want)
	}
}
