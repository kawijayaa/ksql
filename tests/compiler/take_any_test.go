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
	"github.com/kawijayaa/ksql/features"
	"github.com/kawijayaa/ksql/sqlast"
	"github.com/mattn/go-sqlite3"
)

func TestTakeAnyCoherentRowsAndSchema(t *testing.T) {
	db := openInvestigationDB(t)
	source := `datatable(g:string,n:long,s:string,d:dynamic,b:bool)["a",9,"first",dynamic({"id":9}),true,"a",1,"last",dynamic([1]),false,"b",4,"only",dynamic(null),null]`
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		compiler := ksql.New(dialect.SQLite(), ksql.WithParameterization(mode))
		for _, tc := range []struct {
			aggregate string
			names     []string
		}{
			{`take_any(n,s,d,b)`, []string{"g", "n", "s", "d", "b", "total"}},
			{`take_any(*)`, []string{"g", "n", "s", "d", "b", "total"}},
			{`take_any(n,*)`, []string{"g", "n", "s", "d", "b", "total"}},
			{`take_any(*,n)`, []string{"g", "n", "s", "d", "b", "total"}},
			{`(num,label,bag,enabled)=take_any(n,s,d,b)`, []string{"g", "num", "label", "bag", "enabled", "total"}},
			{`any(n,s,d,b)`, []string{"g", "any_n", "any_s", "any_d", "any_b", "total"}},
			{`any(*)`, []string{"g", "any_n", "any_s", "any_d", "any_b", "total"}},
		} {
			result := compiler.Compile(source + `|summarize ` + tc.aggregate + `,total=count() by g|order by g asc`)
			rows := queryRows(t, db, result)
			if len(rows) != 2 || rows[0][0] != "a" || rows[0][5] != int64(2) || fmt.Sprint(rows[1]) != `[b 4 only <nil> <nil> 1]` {
				t.Fatalf("%s: %#v", tc.aggregate, rows)
			}
			if fmt.Sprint(rows[0]) != `[a 9 first {"id":9} 1 2]` && fmt.Sprint(rows[0]) != `[a 1 last [1] 0 2]` {
				t.Fatalf("mixed input records: %#v", rows)
			}
			names := []string{}
			for _, c := range result.Columns {
				names = append(names, c.Name)
			}
			if !reflect.DeepEqual(names, tc.names) || result.Columns[3].Type != ksql.TypeDynamic || result.Columns[4].Type != ksql.TypeBool {
				t.Fatalf("%s: %#v", tc.aggregate, result.Columns)
			}
		}
	}
}

func TestTakeAnyPresenceConditionalAndEmptyGroups(t *testing.T) {
	db := openInvestigationDB(t)
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		compiler := ksql.New(dialect.SQLite(), ksql.WithParameterization(mode))
		for _, tc := range []struct {
			query, want string
			names       []string
		}{
			{`datatable(s:string)["","present",""]|summarize take_any(s)`, `[[present]]`, []string{"s"}},
			{`datatable(n:long)[long(null),7,long(null)]|summarize take_any(n)`, `[[7]]`, []string{"n"}},
			{`datatable(b:bool)[null,false,null]|summarize take_any(b)`, `[[0]]`, []string{"b"}},
			{`datatable(s:string,p:bool)["",true,"eligible",true,"other",false]|summarize take_anyif(s,p),anyif(s,p),total=count()`, `[[eligible eligible 3]]`, []string{"s", "anyif_s", "total"}},
			{`datatable(s:string,p:bool)["excluded",false,"null",null]|summarize take_anyif(s,p),total=count()`, `[[<nil> 2]]`, []string{"s", "total"}},
			{`datatable(n:long,p:bool)[long(null),true,7,false]|summarize take_anyif(n,p),total=count()`, `[[<nil> 2]]`, []string{"n", "total"}},
			{`datatable(g:long,n:long,p:bool)[1,10,false,1,20,true,2,30,null]|summarize v=take_anyif(n,p),total=count() by g|order by g asc`, `[[1 20 2] [2 <nil> 1]]`, []string{"g", "v", "total"}},
			{`datatable(n:long,s:string)[]|summarize take_any(n,s),total=count()`, `[[<nil> <nil> 0]]`, []string{"n", "s", "total"}},
			{`datatable(n:long,p:bool)[]|summarize take_anyif(n,p),total=count()`, `[[<nil> 0]]`, []string{"n", "total"}},
			{`datatable(g:long,n:long)[]|summarize take_any(*) by g`, `[]`, []string{"g", "n"}},
			{`print n=7,s="x"|summarize take_any(n,n,s)`, `[[7 7 x]]`, []string{"n", "n1", "s"}},
			{`print n=7,s="x"|summarize take_any(n+1,s)`, `[[8 x]]`, []string{"any_arg0", "s"}},
			{`print n=7,s="x"|summarize take_any(label=s,n)`, `[[x 7]]`, []string{"label", "n"}},
			{`print n=7,s="x"|summarize take_any(n,s),arg_max(n,s),total=sum(n)`, `[[7 x 7 x 7]]`, []string{"n", "s", "n1", "s1", "total"}},
			{`let f=(T:(n:long,s:string)){T|summarize take_any(*)};f((print n=7,s="x"))`, `[[7 x]]`, []string{"n", "s"}},
		} {
			result := compiler.Compile(tc.query)
			if !result.OK() {
				t.Fatalf("%s: %v", tc.query, result.Diagnostics)
			}
			if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
				t.Fatalf("mode=%d %s: got=%s want=%s", mode, tc.query, got, tc.want)
			}
			names := []string{}
			for _, c := range result.Columns {
				names = append(names, c.Name)
			}
			if !reflect.DeepEqual(names, tc.names) {
				t.Fatalf("%s: columns=%v want=%v", tc.query, names, tc.names)
			}
		}
	}
}

func TestTakeAnyCachedNondeterministicPayloads(t *testing.T) {
	var count atomic.Int64
	driver := "take_any_" + t.Name()
	sql.Register(driver, &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		return c.RegisterFunc("test_tick", func() int64 { return count.Add(1) }, false)
	}})
	db, err := sql.Open(driver, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tick := ksql.WithTypedFunction("tick", func([]ksql.TypedExpression) (ksql.TypedExpression, error) {
		return ksql.TypedExpression{SQL: &sqlast.Call{Name: "test_tick"}, Type: ksql.TypeLong}, nil
	})
	compiler := ksql.New(dialect.SQLite(), tick)
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		compiler = ksql.New(dialect.SQLite(), tick, ksql.WithParameterization(mode))
		result := compiler.Compile(`datatable(n:long)[1,2,3]|extend x=tick()|summarize (selected,duplicate)=take_any(x,x),total=sum(x),last=arg_max(x)`)
		for execution := 0; execution < 2; execution++ {
			count.Store(0)
			rows := queryRows(t, db, result)
			if count.Load() != 3 || len(rows) != 1 || rows[0][0] != rows[0][1] || rows[0][2] != int64(6) || rows[0][3] != int64(3) {
				t.Fatalf("calls=%d rows=%v", count.Load(), rows)
			}
		}
		result = compiler.Compile(`datatable(n:long)[1,2,3]|summarize v=take_anyif(n,tick()==2),total=count()`)
		count.Store(0)
		if got := fmt.Sprint(queryRows(t, db, result)); got != `[[2 3]]` || count.Load() != 3 {
			t.Fatalf("got=%s calls=%d", got, count.Load())
		}
	}
}

func TestTakeAnyDiagnosticsLimitsAndOverrides(t *testing.T) {
	for _, q := range []string{
		`print take_any(1)`, `print anyif(1,true)`, `print n=1|summarize take_any()`,
		`print n=1|summarize take_anyif(n)`, `print n=1|summarize take_anyif(n,true,1)`,
		`print n=1|summarize take_anyif(*,true)`, `print n=1|summarize take_anyif(n,1)`,
		`print n=1|summarize take_anyif(n,"true")`, `print n=1|summarize any(*,n)`,
		`print n=1|summarize (a,b)=take_any(n)`, `print n=1|summarize take_any(missing)`,
		`print n=1|summarize x=take_any(n),x=any(n)`,
		`print n=1|summarize take_any(*) by n`, `Events|summarize take_any(*)`,
		`print n=1|summarize tostring(take_any(n))`,
	} {
		r := ksql.New(dialect.SQLite()).Compile(q)
		if r.OK() || r.SQL != "" || len(r.Diagnostics) == 0 || r.Diagnostics[0].Span.Line < 1 {
			t.Fatalf("accepted %s: %#v", q, r)
		}
	}
	for _, target := range []dialect.Dialect{dialect.ANSI(), dialect.MySQL(), dialect.SQLServer()} {
		r := ksql.New(target).Compile(`print n=1|summarize take_any(n)`)
		if r.OK() || r.SQL != "" {
			t.Fatalf("%s: %#v", target.Name(), r)
		}
	}
	r := ksql.New(dialect.SQLite(), ksql.WithLimits(ksql.Limits{MaxProjectionItems: 2})).Compile(`print a=1,b=2|summarize take_any(a,b),take_any(a)`)
	if r.OK() || !strings.Contains(fmt.Sprint(r.Diagnostics), "MaxProjectionItems") {
		t.Fatal(r.Diagnostics)
	}
	for _, fn := range []string{"take_any", "any", "take_anyif", "anyif"} {
		r := ksql.New(dialect.SQLite(), ksql.WithAggregate(fn, ksql.SQLFunction("max"))).Compile(`datatable(n:long)[1,2]|summarize v=` + fn + `(n)`)
		if got := fmt.Sprint(queryRows(t, openInvestigationDB(t), r)); got != `[[2]]` {
			t.Fatal(got)
		}
	}
	q := `print n=1|summarize take_any(` + strings.TrimSuffix(strings.Repeat("n,", 128), ",") + `)`
	r = ksql.New(dialect.SQLite(), ksql.WithParameters()).Compile(q)
	if len(queryRows(t, openInvestigationDB(t), r)[0]) != 128 {
		t.Fatal(r.Columns)
	}
	r = ksql.New(dialect.SQLite()).Compile(strings.TrimSuffix(q, ")") + `,n)`)
	if r.OK() || !strings.Contains(fmt.Sprint(r.Diagnostics), "128") {
		t.Fatal(r.Diagnostics)
	}
	r = ksql.New(dialect.PostgreSQL(), ksql.WithParameters()).Compile(`datatable(n:long,b:bool)[1,true,2,false]|summarize take_any(n,b),v=take_anyif(b,n==2)`)
	if !r.OK() || !strings.Contains(r.SQL, "ARRAY_AGG(") || !strings.Contains(r.SQL, "AS MATERIALIZED") || fmt.Sprint(r.Args) != `[1 true 2 false 2]` {
		t.Fatalf("PostgreSQL: %#v", r)
	}
	for _, fn := range []string{"take_any", "any", "take_anyif", "anyif"} {
		entry, ok := features.Lookup("aggregate." + fn)
		if !ok || entry.Support != features.Equivalent {
			t.Fatalf("ledger: %#v", entry)
		}
	}
}
