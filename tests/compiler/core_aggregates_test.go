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

func TestCoreAggregateExecutionAndTypes(t *testing.T) {
	db := openInvestigationDB(t)
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		c := ksql.New(dialect.SQLite(), ksql.WithParameterization(mode))
		for _, tc := range []struct {
			query, want string
			types       []ksql.ScalarType
		}{
			{`datatable(n:long,p:bool)[1,true,3,true,100,false,long(null),true,9,bool(null)]|summarize s=sumif(n,p),a=avgif(n,p),lo=minif(n,p),hi=maxif(n,p),c=countif(p),old=count(p),all=count()`, `[[4 2 1 3 3 3 5]]`, []ksql.ScalarType{ksql.TypeLong, ksql.TypeReal, ksql.TypeLong, ksql.TypeLong, ksql.TypeLong, ksql.TypeLong, ksql.TypeLong}},
			{`datatable(n:long,p:bool)[]|summarize s=sum(n),si=sumif(n,p),c=countif(p),a=avgif(n,p),lo=minif(n,p),hi=maxif(n,p)`, `[[0 0 0 <nil> <nil> <nil>]]`, []ksql.ScalarType{ksql.TypeLong, ksql.TypeLong, ksql.TypeLong, ksql.TypeReal, ksql.TypeLong, ksql.TypeLong}},
			{`datatable(n:real,p:bool)[real(null),true,7.0,false,8.0,bool(null)]|summarize s=sumif(n,p),c=countif(p),a=avgif(n,p),lo=minif(n,p),hi=maxif(n,p)`, `[[0 1 <nil> <nil> <nil>]]`, []ksql.ScalarType{ksql.TypeReal, ksql.TypeLong, ksql.TypeReal, ksql.TypeReal, ksql.TypeReal}},
			{`datatable(n:int)[int(null)]|summarize s=sum(n),a=avg(n),lo=min(n)`, `[[0 <nil> <nil>]]`, []ksql.ScalarType{ksql.TypeLong, ksql.TypeReal, ksql.TypeInt}},
			{`datatable(n:int,p:bool)[1,true,2,true]|summarize s=sumif(n,p),a=avgif(n,p),lo=minif(n,p)`, `[[3 1.5 1]]`, []ksql.ScalarType{ksql.TypeLong, ksql.TypeReal, ksql.TypeInt}},
			{`datatable(s:string,b:bool,p:bool)["z",true,true,"a",false,true,"excluded",true,false]|summarize lo=minif(s,p),hi=maxif(s,p),bmin=minif(b,p),bmax=maxif(b,p)`, `[[a z 0 1]]`, []ksql.ScalarType{ksql.TypeString, ksql.TypeString, ksql.TypeBool, ksql.TypeBool}},
			{`datatable(g:string,n:long,p:bool)["a",1,true,"a",3,true,"b",99,false]|summarize a=avgif(n,p),s=sumif(n,p),c=countif(p),chosen=take_any(n) by g|project g,a,s,c|order by g asc`, `[[a 2 4 2] [b <nil> 0 0]]`, []ksql.ScalarType{ksql.TypeString, ksql.TypeReal, ksql.TypeLong, ksql.TypeLong}},
			{`datatable(g:string,n:long)[]|summarize sum(n) by g`, `[]`, []ksql.ScalarType{ksql.TypeString, ksql.TypeLong}},
			{`let f=(T:(n:long,p:bool)){T|summarize x=avgif(n,p)};print x=toscalar(f(datatable(n:long,p:bool)[2,true,4,true]))`, `[[3]]`, []ksql.ScalarType{ksql.TypeReal}},
			{`datatable(n:long)[1,2]|summarize x=iff(true,sum(n),sumif(n,false))+countif(true)`, `[[5]]`, []ksql.ScalarType{ksql.TypeLong}},
			{`datatable(n:long)[1,2]|summarize x=sum(n+toscalar(datatable(v:long)[3,4]|summarize sum(v)))`, `[[17]]`, []ksql.ScalarType{ksql.TypeLong}},
		} {
			r := c.Compile(tc.query)
			if got := fmt.Sprint(queryRows(t, db, r)); got != tc.want {
				t.Fatalf("mode=%d %s: got=%s want=%s", mode, tc.query, got, tc.want)
			}
			types := []ksql.ScalarType{}
			for _, col := range r.Columns {
				types = append(types, col.Type)
			}
			if !reflect.DeepEqual(types, tc.types) {
				t.Fatalf("%s: types=%v want=%v", tc.query, types, tc.types)
			}
		}
	}
}

func TestCoreAggregatesEvaluateArgumentsOnce(t *testing.T) {
	var calls atomic.Int64
	driver := "core_aggregates_" + t.Name()
	sql.Register(driver, &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		return c.RegisterFunc("tick", func() int64 { return calls.Add(1) }, false)
	}})
	db, err := sql.Open(driver, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		c := ksql.New(dialect.SQLite(), ksql.WithParameterization(mode), ksql.WithTypedFunction("tick", func([]ksql.TypedExpression) (ksql.TypedExpression, error) {
			return ksql.TypedExpression{SQL: &sqlast.Call{Name: "tick"}, Type: ksql.TypeLong}, nil
		}))
		for _, tc := range []struct {
			expression, want string
			calls            int64
		}{
			{`tick()`, `[[1]]`, 1},
			{`toscalar(print tick())`, `[[1]]`, 1},
			{`sumif(tick(),n>1)`, `[[3]]`, 2}, {`avgif(tick(),n>1)`, `[[1.5]]`, 2},
			{`minif(tick(),n>1)`, `[[1]]`, 2}, {`maxif(tick(),n>1)`, `[[2]]`, 2},
			{`countif(tick()>0)`, `[[3]]`, 3}, {`sumif(tick(),false)`, `[[0]]`, 0}, {`avgif(tick(),false)`, `[[<nil>]]`, 0},
		} {
			r := c.Compile(`datatable(n:long)[1,2,3]|summarize x=` + tc.expression)
			for i := 0; i < 2; i++ {
				calls.Store(0)
				if got := fmt.Sprint(queryRows(t, db, r)); got != tc.want || calls.Load() != tc.calls {
					t.Fatalf("%s: got=%s calls=%d", tc.expression, got, calls.Load())
				}
			}
		}
	}
}

func TestCoreAggregateDiagnosticsAndOverrides(t *testing.T) {
	c := ksql.New(dialect.SQLite())
	for _, q := range []string{
		`print sum(1)`, `print avgif(1,true)`, `datatable(n:long)[1]|where count()>0`,
		`datatable(n:long)[1]|summarize sum(n) by count()`,
		`datatable(n:long)[1]|summarize sum(sum(n))`,
		`datatable(n:long)[1]|summarize countif(sum(n)>0)`,
		`datatable(n:long)[1]|summarize sum(toscalar(print sum(1)))`,
		`datatable(n:long)[1]|summarize take_any(sum(n))`,
		`let f=(){sum(1)};print 1`,
		`print 1|summarize count(1)`, `print 1|summarize count(*)`, `print 1|summarize count(true,false)`,
		`print 1|summarize sum()`, `print 1|summarize avg(1,2)`, `print 1|summarize minif(1)`, `print 1|summarize maxif(1,true,false)`,
		`print 1|summarize sum("1")`, `print 1|summarize avg(true)`, `print 1|summarize min(dynamic([]))`,
		`print 1|summarize sumif(1,1)`, `print 1|summarize avgif(1,"true")`, `print 1|summarize countif(1)`,
		`print 1|summarize sum(datetime(2026-01-01))`, `print 1|summarize avgif(datetime(2026-01-01),true)`,
	} {
		r := c.Compile(q)
		if r.OK() || r.SQL != "" || len(r.Diagnostics) == 0 || r.Diagnostics[0].Span.Line < 1 {
			t.Fatalf("accepted %s: %#v", q, r)
		}
	}
	for _, name := range []string{"sum", "avgif", "count"} {
		c := ksql.New(dialect.SQLite(), ksql.WithTypedFunction(name, func(args []ksql.TypedExpression) (ksql.TypedExpression, error) { return args[0], nil }))
		if got := fmt.Sprint(queryRows(t, openInvestigationDB(t), c.Compile("print "+name+"(7)"))); got != "[[7]]" {
			t.Fatal(got)
		}
	}
	if got := fmt.Sprint(queryRows(t, openInvestigationDB(t), c.Compile(`let sum=(x:long){x+1};print sum(7)`))); got != "[[8]]" {
		t.Fatal(got)
	}
	r := ksql.New(dialect.PostgreSQL(), ksql.WithParameters()).Compile(`datatable(n:long,p:bool)[1,true]|summarize s=sumif(n,p),a=avgif(n,p),lo=minif(p,p),hi=maxif(p,p),c=countif(p)`)
	if !r.OK() || !strings.Contains(r.SQL, "BOOL_AND") || !strings.Contains(r.SQL, "BOOL_OR") || fmt.Sprint(r.Args) != "[1 true]" {
		t.Fatalf("SQL=%s args=%v diagnostics=%v", r.SQL, r.Args, r.Diagnostics)
	}
	// Explicit extensions keep their signature and context policies.
	r = ksql.New(dialect.SQLite(), ksql.WithFunction("avgif", func(args []sqlast.Expr) (sqlast.Expr, error) { return args[0], nil })).Compile(`print avgif(1)`)
	if !r.OK() {
		t.Fatal(r.Diagnostics)
	}
}
