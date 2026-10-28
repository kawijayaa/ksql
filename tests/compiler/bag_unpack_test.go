package compiler_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/kawijayaa/ksql/sqlast"
)

func bagUnpackCompiler(t *testing.T, db *sql.DB, calls *int) *ksql.Compiler {
	target := dialect.SQLite()
	cast := func(kind string) ksql.FunctionRule {
		return func(args []sqlast.Expr) (sqlast.Expr, error) { return &sqlast.Cast{Expr: args[0], Type: kind}, nil }
	}
	return ksql.New(target, ksql.WithParameters(), ksql.WithFunction("tolong", cast("INTEGER")), ksql.WithFunction("tobool", cast("INTEGER")), ksql.WithFunction("toreal", cast("REAL")), ksql.WithBagKeysResolver(func(query sqlast.Query, args []any) ([]ksql.BagKey, error) {
		*calls++
		text, err := dialect.Render(target, query)
		if err != nil {
			return nil, err
		}
		rows, err := db.Query(text, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var keys []ksql.BagKey
		for rows.Next() {
			var key ksql.BagKey
			if err := rows.Scan(&key.Name, &key.JSONType); err != nil {
				return nil, err
			}
			keys = append(keys, key)
		}
		return keys, rows.Err()
	}))
}

// Microsoft's bag_unpack examples.
func TestBagUnpack(t *testing.T) {
	db := openInvestigationDB(t)
	calls := 0
	compiler := bagUnpackCompiler(t, db, &calls)
	people := `datatable(d:dynamic)[dynamic({"Name": "John", "Age":20}), dynamic({"Name": "Dave", "Age":40}), dynamic({"Name": "Jasmine", "Age":30})]`
	for _, tc := range []struct{ query, names, types, want string }{
		{people + ` | evaluate bag_unpack(d)`, `Age,Name`, `long string`, `[[20 John] [40 Dave] [30 Jasmine]]`},
		{people + ` | evaluate bag_unpack(d, 'Property_')`, `Property_Age,Property_Name`, `long string`, `[[20 John] [40 Dave] [30 Jasmine]]`},
		{`datatable(Name:string, d:dynamic)['James', dynamic({"Name": "John", "Age":20}), 'David', dynamic({ "Age":40}), 'Emily', dynamic({"Name": "Jasmine", "Age":30})] | evaluate bag_unpack(d, columnsConflict='replace_source')`,
			`Name,Age`, `string long`, `[[John 20] [ 40] [Jasmine 30]]`},
		{`datatable(Name:string, d:dynamic)['James', dynamic({"Name": "John", "Age":20}), 'David', dynamic({"Name": "Dave", "Age":40}), 'Emily', dynamic({"Name": "Jasmine", "Age":30})] | evaluate bag_unpack(d, columnsConflict='keep_source')`,
			`Name,Age`, `string long`, `[[James 20] [David 40] [Emily 30]]`},
		{`datatable(d:dynamic)[dynamic({"Name": "John", "Age":20, "Address": "Address-1" }), dynamic({"Name": "Dave", "Age":40, "Address": "Address-2"})] | evaluate bag_unpack(d, ignoredProperties=dynamic(['Address', 'Age']))`,
			`Name`, `string`, `[[John] [Dave]]`},
		{`datatable(d:dynamic)[dynamic({"Name": "John", "Age":20}), dynamic({ "Name": "Dave", "Height": 170, "Age":40}), dynamic({"Name": "Jasmine", "Age":30})] | evaluate bag_unpack(d)`,
			`Age,Height,Name`, `long long string`, `[[20 <nil> John] [40 170 Dave] [30 <nil> Jasmine]]`},
		{`datatable(d:dynamic, Description: string)[dynamic({"Name": "John", "Age":20, "height":180}), "Student", dynamic({"Name": "Dave", "Age":40, "height":160}), "Teacher"] | evaluate bag_unpack(d) : (*, Name:string, Age:long)`,
			`Description,Name,Age`, `string string long`, `[[Student John 20] [Teacher Dave 40]]`},
		// Mixed slot types become dynamic; non-object values and nulls contribute nothing.
		{`datatable(d:dynamic)[dynamic({"v": 1, "b": true}), dynamic({"v": "x", "o": {"k": 1}}), dynamic([1,2]), dynamic(null), dynamic("s")] | evaluate bag_unpack(d)`,
			`b,o,v`, `bool dynamic dynamic`, `[[1 <nil> 1] [<nil> {"k":1} x] [<nil> <nil> <nil>] [<nil> <nil> <nil>] [<nil> <nil> <nil>]]`},
		{`datatable(d:dynamic)[dynamic({"Name": "John"})] | evaluate bag_unpack(d) : (Name:string)`, `Name`, `string`, `[[John]]`},
	} {
		result := compiler.Compile(tc.query)
		if !result.OK() {
			t.Fatalf("%s: %#v", tc.query, result.Diagnostics)
		}
		names, types := []string{}, []string{}
		for _, column := range result.Columns {
			names, types = append(names, column.Name), append(types, string(column.Type))
		}
		if got := strings.Join(names, ","); got != tc.names {
			t.Fatalf("%s: names %s want %s", tc.query, got, tc.names)
		}
		if got := strings.Join(types, " "); got != tc.types {
			t.Fatalf("%s: types %s want %s", tc.query, got, tc.types)
		}
		if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
			t.Fatalf("%s:\n got %s\nwant %s\nSQL=%s", tc.query, got, tc.want, result.SQL)
		}
	}
	before := calls
	compiler.Compile(`datatable(d:dynamic)[dynamic({"a":1})] | evaluate bag_unpack(d) : (a:long)`)
	if calls != before {
		t.Fatal("an output schema must not run discovery")
	}
	for _, tc := range []struct{ query, code string }{
		{`datatable(Name:string, d:dynamic)['x', dynamic({"Name": "y"})] | evaluate bag_unpack(d)`, "KQLB0416"},
		{`datatable(d:dynamic)[dynamic({"a":1})] | evaluate bag_unpack(d, columnsConflict='merge')`, "KQLB0416"},
		{`datatable(s:string)["x"] | evaluate bag_unpack(s)`, "KQLB0416"},
		{`datatable(d:dynamic)[dynamic({"a":1})] | evaluate bag_unpack(nope)`, "KQLB0401"},
		{`datatable(d:dynamic)[dynamic({"a":1})] | evaluate bag_unpack(d) : (a:widget)`, "KQLB0416"},
		{`datatable(d:dynamic)[dynamic({"a":1})] | evaluate pivot(d)`, "KQLL0391"},
		{`print x=1 | evaluate bag_unpack(`, "KQLP0353"},
	} {
		if result := compiler.Compile(tc.query); result.OK() || result.Diagnostics[0].Code != tc.code {
			t.Fatalf("%s: %#v want %s", tc.query, result.Diagnostics, tc.code)
		}
	}
	if result := ksql.New(dialect.SQLite()).Compile(`datatable(d:dynamic)[dynamic({"a":1})] | evaluate bag_unpack(d)`); result.OK() || result.Diagnostics[0].Code != "KQLL0392" {
		t.Fatalf("no resolver: %#v", result.Diagnostics)
	}
}
