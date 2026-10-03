package compiler_test

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/kawijayaa/ksql/sqlast"
)

func TestSummarizeResultScopeAndCardinality(t *testing.T) {
	for _, mode := range []ksql.ParameterizationMode{ksql.LiteralParameters, ksql.BoundParameters} {
		c := ksql.New(dialect.SQLite(), ksql.WithParameterization(mode))
		for _, tc := range []struct{ query, want string }{
			{`datatable(n:long)[1,2,3]|summarize x=7`, `[[7]]`},
			{`datatable(n:long)[]|summarize x=7,b=true,d=dynamic({"a":1})`, `[[7 1 {"a":1}]]`},
			{`datatable(n:long)[1,2,3]|summarize x=toscalar(datatable(v:long)[4,5]|summarize sum(v))`, `[[9]]`},
			{`datatable(n:long)[]|summarize x=toscalar(datatable(v:long)[4,5]|summarize sum(v))`, `[[9]]`},
			{`let n=7;datatable(n:long)[1,2,3]|summarize x=n,agg=sum(n)`, `[[7 6]]`},
			{`let d=dynamic({"a":7});datatable(d:dynamic)[dynamic({"a":1})]|summarize x=d.a`, `[[7]]`},
			{`let f=(T:(n:long),n:long){T|summarize x=n,agg=sum(n)};f(datatable(n:long)[1,2],7)`, `[[7 3]]`},
			{`datatable(g:string,n:long)["a",1,"a",2,"b",3]|summarize x=7 by g|order by g asc`, `[[a 7] [b 7]]`},
			{`datatable(g:string,n:long)[]|summarize x=7 by g`, `[]`},
			{`datatable(n:long)[1,2]|summarize x=iff(sum(n)>2,max(n),min(n)),c=1`, `[[2 1]]`},
			{`datatable(n:long)[1,2]|summarize ['_k1_aggregate_anchor']=7|project *`, `[[7]]`},
		} {
			r := c.Compile(tc.query)
			if got := fmt.Sprint(queryRows(t, openInvestigationDB(t), r)); got != tc.want {
				t.Fatalf("mode=%d %s: got=%s want=%s", mode, tc.query, got, tc.want)
			}
		}
		for _, q := range []string{
			`datatable(n:long)[1,2]|summarize x=n,c=count()`,
			`datatable(n:long)[1,2]|summarize x=n by n`,
			`datatable(n:long)[1,2]|summarize x=iff(n>0,sum(n),0)`,
			`datatable(n:long)[1,2]|summarize x=sum(n)+n`,
			`datatable(n:long)[1,2]|summarize n,arg_max(n)`,
			`datatable(n:long)[1,2]|summarize x=arg_max(n),n`,
			`datatable(d:dynamic)[dynamic({"a":1})]|summarize x=d.a`,
			`datatable(d:dynamic)[dynamic({"a":1})]|summarize x=d["a"]`,
			`datatable(n:long)[1,2]|summarize *`,
			`datatable(n:long)[1,2]|summarize x=*`,
			`datatable(n:long)[1,2]|summarize x=pack_array(*)`,
			`UnknownTable|summarize x=notAColumn,c=count()`,
			`print make_list(1)`, `print 1|summarize sum(make_list(1))`,
			`print 1|summarize make_list(sum(1))`, `print 1|summarize make_list(*)`,
			`print sum(iff(true,*,1))`, `print iff(true,*,1)`,
		} {
			r := c.Compile(q)
			if r.OK() || r.SQL != "" || len(r.Diagnostics) == 0 || r.Diagnostics[0].Span.Column < 1 {
				t.Fatalf("accepted %s: %#v", q, r)
			}
		}
	}
}

func TestAggregateExtensionRolesAndRegistrationPrecedence(t *testing.T) {
	var invoked atomic.Int32
	rule := func(args []ksql.TypedExpression) (ksql.TypedExpression, error) {
		invoked.Add(1)
		if len(args) != 1 {
			return ksql.TypedExpression{}, fmt.Errorf("one argument required")
		}
		return ksql.TypedExpression{SQL: &sqlast.Call{Name: "max", Args: []sqlast.Expr{args[0].SQL}}, Type: args[0].Type}, nil
	}
	c := ksql.New(dialect.SQLite(), ksql.WithTypedAggregate(" Pick ", rule))
	for _, q := range []string{
		`print pick(1)`, `print n=1|project pick(n)`, `print n=1|summarize sum(pick(n))`,
		`print n=1|summarize pick(sum(n))`, `print n=1|summarize count() by pick(n)`,
		`print n=1|summarize pick(missing)`,
	} {
		r := c.Compile(q)
		if r.OK() || r.SQL != "" || invoked.Load() != 0 {
			t.Fatalf("%s: %#v invoked=%d", q, r, invoked.Load())
		}
	}
	for _, q := range []string{
		`datatable(n:long)[1,2,3]|summarize x=pick(n)+count()`,
		`let f=(T:(n:long)){T|summarize x=pick(n)};f(datatable(n:long)[1,2,3])`,
		`datatable(n:long)[1,2,3]|summarize x=take_any(n),p=pick(n)`,
	} {
		if rows := queryRows(t, openInvestigationDB(t), c.Compile(q)); len(rows) != 1 {
			t.Fatal(rows)
		}
	}
	for i, options := range [][]ksql.Option{
		{ksql.WithFunction("f", ksql.SQLFunction("abs")), ksql.WithAggregate("f", ksql.SQLFunction("max"))},
		{ksql.WithTypedFunction("f", func(args []ksql.TypedExpression) (ksql.TypedExpression, error) { return args[0], nil }), ksql.WithTypedAggregate("f", rule)},
		{ksql.WithAggregate("f", ksql.SQLFunction("max")), ksql.WithTypedFunction("f", func(args []ksql.TypedExpression) (ksql.TypedExpression, error) { return args[0], nil })},
		{ksql.WithTypedAggregate("f", rule), ksql.WithFunction("f", ksql.SQLFunction("abs"))},
	} {
		c := ksql.New(dialect.SQLite(), options...)
		if i < 2 {
			if got := fmt.Sprint(queryRows(t, openInvestigationDB(t), c.Compile(`datatable(n:long)[1,2]|summarize x=f(n)`))); got != "[[2]]" {
				t.Fatal(got)
			}
			if c.Compile(`print f(1)`).OK() {
				t.Fatal("aggregate lost role")
			}
		} else {
			if !c.Compile(`print f(1)`).OK() {
				t.Fatal("scalar retained aggregate role")
			}
			if c.Compile(`print n=1|summarize x=f(n)`).OK() {
				t.Fatal("scalar consumed row outside aggregate")
			}
			if got := fmt.Sprint(queryRows(t, openInvestigationDB(t), c.Compile(`datatable(n:long)[1,2]|summarize x=f(7)`))); got != "[[7]]" {
				t.Fatal(got)
			}
		}
	}
	// Ignored registrations must not change the previously configured role.
	c = ksql.New(dialect.SQLite(), ksql.WithAggregate("f", ksql.SQLFunction("max")), ksql.WithTypedFunction("f", nil), ksql.WithAggregate("", ksql.SQLFunction("max")), ksql.WithTypedAggregate("f", nil))
	if c.Compile(`print f(1)`).OK() || !c.Compile(`print n=1|summarize f(n)`).OK() {
		t.Fatal("nil registration changed role")
	}
}

func TestConstantSummaryDialectParametersAndProjectionBound(t *testing.T) {
	for _, target := range []dialect.Dialect{dialect.SQLite(), dialect.PostgreSQL()} {
		c := ksql.New(target, ksql.WithParameters(), ksql.WithLimits(ksql.Limits{MaxProjectionItems: 1}))
		r := c.Compile(`datatable(n:long)[1,2]|summarize x=7`)
		if !r.OK() || len(r.Columns) != 1 || r.Columns[0].Name != "x" || fmt.Sprint(r.Args) != "[7 1 2]" || !strings.Contains(r.SQL, "COUNT(*)") {
			t.Fatalf("%s: %#v", target.Name(), r)
		}
		if c.Compile(`datatable(n:long)[1,2]|summarize x=7,y=8`).OK() {
			t.Fatal("hidden anchor bypassed projection bound")
		}
	}
}

func TestScalarExtensionDiscardingAggregateKeepsSingletonResult(t *testing.T) {
	c := ksql.New(dialect.SQLite(), ksql.WithTypedFunction("discard", func([]ksql.TypedExpression) (ksql.TypedExpression, error) {
		return ksql.TypedExpression{SQL: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "7"}, Type: ksql.TypeLong}, nil
	}))
	for _, q := range []string{
		`datatable(n:long)[1,2,3]|summarize x=discard(sum(n))`,
		`datatable(n:long)[]|summarize x=discard(sum(n))`,
	} {
		if got := fmt.Sprint(queryRows(t, openInvestigationDB(t), c.Compile(q))); got != "[[7]]" {
			t.Fatal(got)
		}
	}
}
