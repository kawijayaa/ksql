package compiler_test

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/kawijayaa/ksql/sqlast"
	"github.com/mattn/go-sqlite3"
)

func TestConditionalCommonTypesAndCoalesce(t *testing.T) {
	db := openInvestigationDB(t)
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		compiler := ksql.New(dialect.SQLite(), ksql.WithParameterization(mode))
		for _, tc := range []struct {
			query, want string
			types       []ksql.ScalarType
		}{
			{`print a=iff(true,1,2.0)/2,b=iff(false,1,2.0)/2`, `[[0.5 1]]`, []ksql.ScalarType{ksql.TypeReal, ksql.TypeReal}},
			{`print a=case(true,1,false,2.0,3.0)/2`, `[[0.5]]`, []ksql.ScalarType{ksql.TypeReal}},
			{`print a=case(false,int(1),false,int(2),3)`, `[[3]]`, []ksql.ScalarType{ksql.TypeLong}},
			{`print a=iff(true,1,null),b=iff(false,1,null)`, `[[1 <nil>]]`, []ksql.ScalarType{ksql.TypeLong, ksql.TypeLong}},
			{`print a=iff(bool(null),true,false),b=case(bool(null),1,2)`, `[[0 2]]`, []ksql.ScalarType{ksql.TypeBool, ksql.TypeLong}},
			{`print a=coalesce("","second","last"),b=coalesce("",""),c=coalesce(null,"x")`, `[[second  x]]`, []ksql.ScalarType{ksql.TypeString, ksql.TypeString, ksql.TypeString}},
			{`print a=coalesce(long(null),42,33),b=coalesce(int(null),int(2)),c=coalesce(1,2.0)/2`, `[[42 2 0.5]]`, []ksql.ScalarType{ksql.TypeLong, ksql.TypeInt, ksql.TypeReal}},
			{`print a=coalesce(bool(null),false),b=coalesce(bool(null),bool(null))`, `[[0 <nil>]]`, []ksql.ScalarType{ksql.TypeBool, ksql.TypeBool}},
			{`print a=iff(true,dynamic([1,true]),dynamic({})),b=case(false,dynamic([]),dynamic({"ok":true}))`, `[[[1,true] {"ok":true}]]`, []ksql.ScalarType{ksql.TypeDynamic, ksql.TypeDynamic}},
			{`print a=iff(true,1,"x"),b=iff(false,1,"x"),c=case(true,true,"x"),d=case(true,false,1)`, `[[1 "x" true false]]`, []ksql.ScalarType{ksql.TypeDynamic, ksql.TypeDynamic, ksql.TypeDynamic, ksql.TypeDynamic}},
			{`print a=iff(true,long(null),"x"),b=iff(false,"x",bool(null))`, `[[<nil> <nil>]]`, []ksql.ScalarType{ksql.TypeDynamic, ksql.TypeDynamic}},
			{`print a=coalesce(dynamic(null),dynamic({"n":9223372036854775807}))`, `[[{"n":9223372036854775807}]]`, []ksql.ScalarType{ksql.TypeDynamic}},
			{`let x=iff(true,1,"x");print a=x,b=x`, `[[1 1]]`, []ksql.ScalarType{ksql.TypeDynamic, ksql.TypeDynamic}},
			{`let f=(x:bool){iff(x,1,2.0)};print f(true)/2`, `[[0.5]]`, []ksql.ScalarType{ksql.TypeReal}},
			{`datatable(n:int)[1]|project a=-n,b=+n`, `[[-1 1]]`, []ksql.ScalarType{ksql.TypeLong, ksql.TypeLong}},
			{`print a=-int(1),b=+int(2)`, `[[-1 2]]`, []ksql.ScalarType{ksql.TypeInt, ksql.TypeInt}},
			{`let x=int(1);print -x`, `[[-1]]`, []ksql.ScalarType{ksql.TypeInt}},
			{`print a=-9223372036854775808,b=iff(true,-9223372036854775808,"x")`, `[[-9223372036854775808 -9223372036854775808]]`, []ksql.ScalarType{ksql.TypeLong, ksql.TypeDynamic}},
			{`print a=-(-1),b=+(-2),c=-(1+2),d=not(not(true)),e=-long(null)`, `[[1 -2 -3 1 <nil>]]`, []ksql.ScalarType{ksql.TypeLong, ksql.TypeLong, ksql.TypeLong, ksql.TypeBool, ksql.TypeLong}},
		} {
			r := compiler.Compile(tc.query)
			if got := fmt.Sprint(queryRows(t, db, r)); got != tc.want {
				t.Fatalf("mode=%d %s: got=%s want=%s", mode, tc.query, got, tc.want)
			}
			types := []ksql.ScalarType{}
			for _, c := range r.Columns {
				types = append(types, c.Type)
			}
			if !reflect.DeepEqual(types, tc.types) {
				t.Fatalf("%s: types=%v want=%v", tc.query, types, tc.types)
			}
		}
	}
}

func TestScalarTypeDiagnosticsAndResourceLimits(t *testing.T) {
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		compiler := ksql.New(dialect.SQLite(), ksql.WithParameterization(mode))
		for _, query := range []string{
			`print +"1"`, `print -true`, `print 1 and true`, `print false or "x"`,
			`print iff(1,"yes","no")`, `print iff("true",1,2)`, `print case(true,1,2,3,4)`,
			`print coalesce()`, `print coalesce(1)`, `print coalesce(1,"x")`, `print coalesce(true,1)`,
			`print iff(true,dynamic(1),1)`, `print -dynamic(1)`, `print dynamic(true) and true`,
			`print bool(1)`, `print bool("true")`,
		} {
			r := compiler.Compile(query)
			if r.OK() || r.SQL != "" || len(r.Diagnostics) == 0 || r.Diagnostics[0].Span.Line < 1 {
				t.Fatalf("accepted %s: %#v", query, r)
			}
		}
		q := `print coalesce(` + strings.TrimSuffix(strings.Repeat(`"",`, 63), ",") + `,"ok")`
		if got := fmt.Sprint(queryRows(t, openInvestigationDB(t), compiler.Compile(q))); got != `[[ok]]` {
			t.Fatal(got)
		}
		r := compiler.Compile(strings.TrimSuffix(q, ")") + `,"excess")`)
		if r.OK() || !strings.Contains(fmt.Sprint(r.Diagnostics), "64") {
			t.Fatal(r.Diagnostics)
		}
		q = `print case(` + strings.Repeat(`false,1,`, 64) + `2)`
		if got := fmt.Sprint(queryRows(t, openInvestigationDB(t), compiler.Compile(q))); got != `[[2]]` {
			t.Fatal(got)
		}
		r = compiler.Compile(strings.TrimSuffix(q, ")") + `,3,4)`)
		if r.OK() || !strings.Contains(fmt.Sprint(r.Diagnostics), "64") {
			t.Fatal(r.Diagnostics)
		}
	}
	for _, target := range []dialect.Dialect{dialect.ANSI(), dialect.MySQL(), dialect.SQLServer()} {
		r := ksql.New(target).Compile(`print iff(true,1,"x")`)
		if r.OK() || r.SQL != "" {
			t.Fatalf("unsupported dynamic renderer=%s: %#v", target.Name(), r)
		}
	}
	r := ksql.New(dialect.PostgreSQL(), ksql.WithParameters()).Compile(`print a=iff(true,1,"x"),b=case(false,false,"y"),c=coalesce("","ok"),d=iff(true,1,2.0)`)
	if !r.OK() || !strings.Contains(r.SQL, "to_jsonb(CAST(") || fmt.Sprint(r.Args) != `[true 1 x false false y  ok true 1 2]` {
		t.Fatalf("PostgreSQL: SQL=%s args=%v diagnostics=%v", r.SQL, r.Args, r.Diagnostics)
	}
}

func TestConditionalsEvaluateSelectedExpressionsOnce(t *testing.T) {
	var count atomic.Int64
	driver := "scalar_types_" + t.Name()
	sql.Register(driver, &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		return c.RegisterFunc("tick", func() int64 { return count.Add(1) }, false)
	}})
	db, err := sql.Open(driver, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		compiler := ksql.New(dialect.SQLite(), ksql.WithParameterization(mode), ksql.WithTypedFunction("tick", func([]ksql.TypedExpression) (ksql.TypedExpression, error) {
			return ksql.TypedExpression{SQL: &sqlast.Call{Name: "tick"}, Type: ksql.TypeLong}, nil
		}), ksql.WithTypedFunction("tick_bool", func([]ksql.TypedExpression) (ksql.TypedExpression, error) {
			return ksql.TypedExpression{SQL: &sqlast.Binary{Left: &sqlast.Call{Name: "tick"}, Operator: ">", Right: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "0"}}, Type: ksql.TypeBool}, nil
		}))
		for _, query := range []string{`print iff(true,tick(),"x")`, `print case(true,tick_bool(),"x")`, `print coalesce(tick(),tick())`, `print iff(true,tick(),2.0)`} {
			count.Store(0)
			r := compiler.Compile(query)
			rows := queryRows(t, db, r)
			if count.Load() != 1 || len(rows) != 1 {
				t.Fatalf("%s: calls=%d rows=%v", query, count.Load(), rows)
			}
		}
	}
}

func TestConditionalOverridesKeepPrecedence(t *testing.T) {
	for _, fn := range []string{"coalesce", "case", "iff", "bool"} {
		compiler := ksql.New(dialect.SQLite(), ksql.WithTypedFunction(fn, func(args []ksql.TypedExpression) (ksql.TypedExpression, error) { return args[0], nil }))
		r := compiler.Compile(`print ` + fn + `(7)`)
		if got := fmt.Sprint(queryRows(t, openInvestigationDB(t), r)); got != `[[7]]` {
			t.Fatalf("%s: %s", fn, got)
		}
	}
	r := ksql.New(dialect.SQLite()).Compile(`let iff=(x:long){x+1};print iff(2)`)
	if got := fmt.Sprint(queryRows(t, openInvestigationDB(t), r)); got != `[[3]]` {
		t.Fatal(got)
	}
}
