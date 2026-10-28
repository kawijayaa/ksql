package compiler_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/kawijayaa/ksql/sqlast"
)

func TestQueryParametersExecutionAndMetadata(t *testing.T) {
	db := openInvestigationDB(t)
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		for _, tc := range []struct {
			query, want string
			values      map[string]any
			kind        ksql.ScalarType
		}{
			{`declare query_parameters(maxInjured:long=90); datatable(n:long)[80,100,150]|where n>maxInjured`, `[[100] [150]]`, nil, ksql.TypeLong},
			{`declare query_parameters(maxInjured:long=90); datatable(n:long)[80,100,150]|where n>maxInjured`, `[[150]]`, map[string]any{"maxInjured": 100}, ksql.TypeLong},
			{`declare query_parameters(x:int=2147483647);print x`, `[[2147483647]]`, nil, ksql.TypeInt},
			{`declare query_parameters(x:long=-9223372036854775808);print x`, `[[-9223372036854775808]]`, nil, ksql.TypeLong},
			{`declare query_parameters(x:long);print x`, `[[9223372036854775807]]`, map[string]any{"x": json.Number("9223372036854775807")}, ksql.TypeLong},
			{`declare query_parameters(x:real=5);print x/2`, `[[2.5]]`, nil, ksql.TypeReal},
			{`declare query_parameters(x:real);print x/2`, `[[2.5]]`, map[string]any{"x": 5.0}, ksql.TypeReal},
			{`declare query_parameters(x:bool=false);print x`, `[[0]]`, nil, ksql.TypeBool},
			{`declare query_parameters(x:bool);print x`, `[[1]]`, map[string]any{"x": true}, ksql.TypeBool},
			{`declare query_parameters(x:bool=bool(null));print x`, `[[<nil>]]`, nil, ksql.TypeBool},
			{`declare query_parameters(x:string="default",y:long); print result=x`, `[[actual]]`, map[string]any{"x": "actual", "y": 1}, ksql.TypeString},
			{`declare query_parameters(x:string);print x`, `[[O'Reilly; drop table events; --]]`, map[string]any{"x": "O'Reilly; drop table events; --"}, ksql.TypeString},
			{`declare query_parameters(['a.b']:long=2);print ['a.b']`, `[[2]]`, nil, ksql.TypeLong},
			{`declare query_parameters(x:datetime=datetime(2026-01-02));print x`, `[[2026-01-02T00:00:00Z]]`, nil, ksql.TypeDateTime},
			{`declare query_parameters(x:datetime);print x`, `[[2026-01-02T00:00:00Z]]`, map[string]any{"x": time.Date(2026, 1, 2, 1, 0, 0, 0, time.FixedZone("offset", 3600))}, ksql.TypeDateTime},
			{`declare query_parameters(x:dynamic);print result=x.n`, `[[9007199254740993]]`, map[string]any{"x": json.RawMessage(`{"n":9007199254740993}`)}, ksql.TypeDynamic},
			{`declare query_parameters(x:dynamic);print result=x[1]`, `[[1]]`, map[string]any{"x": json.RawMessage(`[null,true,"s"]`)}, ksql.TypeDynamic},
			{`declare query_parameters(x:long);let f=(){x+1};print f()+x`, `[[11]]`, map[string]any{"x": 5}, ksql.TypeLong},
			{`declare query_parameters(x:long);let y=toscalar(x+1);datatable(n:long)[1,2]|project n+y`, `[[7] [8]]`, map[string]any{"x": 5}, ksql.TypeLong},
			{`declare query_parameters(x:long);datatable(n:long)[1,2]|summarize total=sum(n)+x`, `[[8]]`, map[string]any{"x": 5}, ksql.TypeLong},
		} {
			compiler := ksql.New(dialect.SQLite(), ksql.WithParameterization(mode), ksql.WithQueryParameters(tc.values))
			result := compiler.Compile(tc.query)
			if !result.OK() || len(result.Columns) != 1 || result.Columns[0].Type != tc.kind {
				t.Fatalf("%s: %#v", tc.query, result)
			}
			if got := fmt.Sprint(queryRows(t, db, result)); got != tc.want {
				t.Fatalf("%s: got %s want %s", tc.query, got, tc.want)
			}
			if strings.Contains(result.SQL, "O'Reilly") {
				t.Fatal("request parameter rendered into SQL")
			}
		}
	}
}

func TestQueryParameterDiagnosticsAndPureDefaults(t *testing.T) {
	values := map[string]any{"x": 5, "extra": "ignored"}
	for _, query := range []string{
		`declare query_parameters(x:long=1+2);print x`,
		`declare query_parameters(x:long=rand());print x`,
		`declare query_parameters(x:long=x);print x`,
		`declare query_parameters(x:dynamic=dynamic({}));print x`,
		`declare query_parameters(x:int=2147483648);print x`,
		`declare query_parameters(x:long=9223372036854775808);print x`,
		`declare query_parameters(x:bool=1);print x`,
		`declare query_parameters(x:string=null);print x`,
		`declare query_parameters(x:datetime=datetime(bad));print x`,
		`declare query_parameters(x:guid);print x`,
		`declare query_parameters(x:timespan);print x`,
		`declare query_parameters(x:decimal);print x`,
		`declare query_parameters(x:long,x:long);print x`,
		`let x=1;declare query_parameters(x:long);print x`,
		`declare query_parameters(x:long);let x=1;print x`,
		`declare query_parameters(x:long=);print x`,
		`declare query_parameters(x:(*));print x`,
		`declare query_parameters(x:long) print x`,
		`declare query_parameters();print 1`,
	} {
		result := ksql.New(dialect.SQLite(), ksql.WithQueryParameters(values)).Compile(query)
		if result.OK() || result.SQL != "" || len(result.Diagnostics) == 0 || result.Diagnostics[0].Span.Line < 1 {
			t.Fatalf("invalid accepted: %s %#v", query, result)
		}
	}
	for _, value := range []any{nil, "5", true, 1.5, json.Number("9223372036854775808")} {
		query := `declare query_parameters(x:long);print x`
		if value == nil {
			query = `declare query_parameters(x:string);print x`
		}
		result := ksql.New(dialect.SQLite(), ksql.WithQueryParameters(map[string]any{"x": value})).Compile(query)
		if result.OK() || result.SQL != "" {
			t.Fatalf("invalid parameter accepted: %v", value)
		}
	}
	if result := ksql.New(dialect.SQLite()).Compile(`declare query_parameters(x:long); print 1`); result.OK() {
		t.Fatal("missing unused required parameter accepted")
	}
	for _, date := range []string{"9999-12-31T23:59:59-01:00", "0001-01-01T00:00:00+01:00"} {
		if result := ksql.New(dialect.SQLite(), ksql.WithQueryParameters(map[string]any{"x": date})).Compile(`declare query_parameters(x:datetime);print x`); result.OK() {
			t.Fatal("UTC date range exceeded")
		}
	}
	calls := 0
	compiler := ksql.New(dialect.SQLite(), ksql.WithFunction("long", func([]sqlast.Expr) (sqlast.Expr, error) {
		calls++
		return &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "999"}, nil
	}))
	result := compiler.Compile(`declare query_parameters(x:long=long(3));print x`)
	if !result.OK() || calls != 0 {
		t.Fatalf("default invoked an extension: %+v", result.Diagnostics)
	}
}

func TestQueryParameterSnapshotsPruningAndConcurrency(t *testing.T) {
	db := openInvestigationDB(t)
	raw := json.RawMessage(`{"x":2}`)
	values := map[string]any{"bag": raw, "unused": 42}
	compiler := ksql.New(dialect.SQLite(), ksql.WithQueryParameters(values))
	copy(raw, []byte(`{"x":9}`))
	values["bag"] = json.RawMessage(`{"x":99}`)
	query := `declare query_parameters(bag:dynamic,unused:long);print bag.x`
	result := compiler.Compile(query)
	if got := fmt.Sprint(queryRows(t, db, result)); got != "[[2]]" || len(result.Args) != 1 {
		t.Fatalf("snapshot/pruning failed: %s %v", got, result.Args)
	}
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result := compiler.Compile(query)
			if !result.OK() || len(result.Args) != 1 {
				t.Errorf("concurrent compile failed: %v", result.Diagnostics)
			}
		}()
	}
	wait.Wait()
	for _, values := range []map[string]any{{"x": make(chan int)}, {"x": strings.Repeat("x", (4<<20)+1)}, {"x": "\xff"}, {"": 1}} {
		if result := ksql.New(dialect.SQLite(), ksql.WithQueryParameters(values)).Compile("print 1"); result.OK() {
			t.Fatal("invalid configuration accepted")
		}
	}
}
