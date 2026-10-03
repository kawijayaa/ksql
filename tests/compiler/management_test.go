package compiler_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

func managementFixture() ksql.ManagementCatalog {
	return ksql.ManagementCatalog{DatabaseName: "Local", Tables: []ksql.ManagementTable{
		{Name: "Events", Folder: "Logs", DocString: "Investigation", Schema: ksql.Schema{Columns: []ksql.Column{{Name: "n", Type: ksql.TypeLong}, {Name: "ok", Type: ksql.TypeBool}, {Name: "['quoted']", Type: ksql.TypeString}}}},
		{Name: "O'Reilly", Schema: ksql.Schema{Columns: []ksql.Column{{Name: "schema", Type: ksql.TypeInt}}}},
	}}
}

func TestManagementMetadataAndResultPipelines(t *testing.T) {
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		catalog := managementFixture()
		c := ksql.New(dialect.SQLite(), ksql.WithManagementCatalog(catalog), ksql.WithParameterization(mode))
		catalog.Tables[0].Folder = "mutated"
		catalog.Tables[0].Schema.Columns[0].Type = ksql.TypeReal
		for _, tc := range []struct{ q, want string }{
			{`.show tables`, `[[Events Local Logs Investigation] [O'Reilly Local  ]]`},
			{`.show tables (Events,Events)|project TableName,Folder`, `[[Events Logs]]`},
			{`.show tables (["O'Reilly"])|project TableName`, `[[O'Reilly]]`},
			{`// comment
.show tables | where Folder == "Logs" | project TableName`, `[[Events]]`},
			{`.show tables | summarize n=count()`, `[[2]]`},
			{`.show table Events cslschema|project Schema`, `[[n:long,ok:bool,["['quoted']"]:string]]`},
			{`.show table Events kqlschema|project TableName`, `[[Events]]`},
			{`.show table ["O'Reilly"] cslschema|project Schema`, `[[schema:int]]`},
		} {
			r := c.Compile(tc.q)
			if got := fmt.Sprint(queryRows(t, openInvestigationDB(t), r)); got != tc.want {
				t.Fatalf("mode=%d %s: got=%s want=%s", mode, tc.q, got, tc.want)
			}
		}
		r := c.Compile(`.show table Events schema as json`)
		rows := queryRows(t, openInvestigationDB(t), r)
		if len(r.Columns) != 5 || r.Columns[1].Type != ksql.TypeString || len(rows) != 1 {
			t.Fatal(r, rows)
		}
		var schema struct {
			Name           string
			OrderedColumns []struct{ Name, Type, CslType string }
		}
		if err := json.Unmarshal([]byte(rows[0][1].(string)), &schema); err != nil {
			t.Fatal(err)
		}
		if schema.Name != "Events" || len(schema.OrderedColumns) != 3 || schema.OrderedColumns[0].Type != "System.Int64" || schema.OrderedColumns[1].CslType != "bool" {
			t.Fatal(schema)
		}
	}
	for _, target := range []dialect.Dialect{dialect.SQLite(), dialect.PostgreSQL()} {
		r := ksql.New(target, ksql.WithParameters(), ksql.WithManagementCatalog(managementFixture())).Compile(`.show tables|where TableName=="Events"`)
		if !r.OK() || len(r.Columns) != 4 || len(r.Args) != 9 {
			t.Fatalf("%s: %#v", target.Name(), r)
		}
	}
	c := ksql.New(dialect.SQLite(), ksql.WithManagementCatalog(ksql.ManagementCatalog{DatabaseName: "Empty"}))
	if rows := queryRows(t, openInvestigationDB(t), c.Compile(`.show tables`)); len(rows) != 0 {
		t.Fatal(rows)
	}
}

func TestManagementDiagnosticsAndIsolation(t *testing.T) {
	c := ksql.New(dialect.SQLite(), ksql.WithManagementCatalog(managementFixture()))
	for _, q := range []string{
		`.`, `.show`, `.show tables ()`, `.show tables (Events,)`, `.show tables (Events`,
		`.show tables junk`, `.show table`, `.show table Events schema`, `.show table Events schema as csl`,
		`.show table Events schema as json trailing`, `.show table ['bad' cslschema`,
		`.show table events cslschema`, `.show table Missing cslschema`, `.show tables (Missing)`,
		`.show tables details`, `.drop table Events`, `.create table T(n:long)`, `.show external tables`,
		`(.show tables)`, `let x=(.show tables);x`, `print toscalar(.show tables)`, `print 1|join (.show tables) on TableName`,
		`.show tables|join (.show tables) on TableName`, `.show tables;print 1`, `let x=1;.show tables`,
	} {
		r := c.Compile(q)
		if r.OK() || r.SQL != "" || len(r.Diagnostics) == 0 || r.Diagnostics[0].Span.Line < 1 {
			t.Fatalf("accepted %s: %#v", q, r)
		}
	}
	r := ksql.New(dialect.SQLite()).Compile(`.show tables`)
	if r.OK() || !strings.Contains(fmt.Sprint(r.Diagnostics), "management catalog") {
		t.Fatal(r)
	}
	for _, catalog := range []ksql.ManagementCatalog{
		{}, {DatabaseName: "Local", Tables: []ksql.ManagementTable{{Name: "T"}, {Name: "T"}}},
		{DatabaseName: "Local", Tables: make([]ksql.ManagementTable, 4097)},
		{DatabaseName: "Local", Tables: []ksql.ManagementTable{{Name: "T", DocString: strings.Repeat("x", 4<<20)}}},
	} {
		r := ksql.New(dialect.SQLite(), ksql.WithManagementCatalog(catalog)).Compile(`.show tables`)
		if r.OK() || r.SQL != "" {
			t.Fatalf("accepted invalid catalog: %#v", r)
		}
	}
	c = ksql.New(dialect.SQLite(), ksql.WithManagementCatalog(ksql.ManagementCatalog{DatabaseName: "Local", Tables: []ksql.ManagementTable{{Name: "T", Schema: ksql.Schema{Unknown: true}}}}))
	if !c.Compile(`.show tables`).OK() || c.Compile(`.show table T schema as json`).OK() {
		t.Fatal("unknown schema was invented")
	}
}

func TestManagementExtensionRequiresStandaloneCommand(t *testing.T) {
	var calls atomic.Int64
	c := ksql.New(dialect.SQLite(), ksql.WithSource("management", func(ctx ksql.LoweringContext, source kql.Source) (ksql.Relation, error) {
		calls.Add(1)
		if source.Command == nil || source.Command.Kind != "show" {
			return ksql.Relation{}, fmt.Errorf("expected a show command AST")
		}
		return ksql.Relation{Query: &sqlast.Select{Projections: []sqlast.SelectItem{{Expr: ctx.Bind("external-result"), Alias: "Value"}}}, Schema: ksql.Schema{Columns: []ksql.Column{{Name: "Value", Type: ksql.TypeString}}}}, nil
	}))
	if got := fmt.Sprint(queryRows(t, openInvestigationDB(t), c.Compile(`.show operations|project Value`))); got != "[[external-result]]" {
		t.Fatal(got)
	}
	for _, q := range []string{`let x=(.show operations);x`, `print toscalar(.show operations)`, `.show operations; print 1`} {
		before := calls.Load()
		r := c.Compile(q)
		// Multiple result statements may bind the first source but never emit SQL.
		if r.OK() || r.SQL != "" || q != `.show operations; print 1` && calls.Load() != before {
			t.Fatal(q, r, calls.Load())
		}
	}
}

func TestManagementResultBudgetIncludesRepeatedDatabaseNames(t *testing.T) {
	catalog := ksql.ManagementCatalog{DatabaseName: strings.Repeat("x", 1<<20), Tables: []ksql.ManagementTable{{Name: "A"}, {Name: "B"}, {Name: "C"}, {Name: "D"}}}
	c := ksql.New(dialect.SQLite(), ksql.WithParameters(), ksql.WithManagementCatalog(catalog))
	r := c.Compile(`.show tables`)
	if r.OK() || r.SQL != "" || !strings.Contains(fmt.Sprint(r.Diagnostics), "4 MiB") {
		t.Fatal("repeated metadata escaped the result budget")
	}
	if !c.Compile(`.show tables (A)`).OK() {
		t.Fatal("bounded subset rejected")
	}
}
