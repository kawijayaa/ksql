package compiler_test

import (
	"testing"
	"time"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
)

// Compile must never panic or run unbounded: invalid queries return
// diagnostics.
func FuzzCompileNeverPanics(f *testing.F) {
	for _, seed := range []string{
		`range x from 1 to 5 step 1 | scan declare (c:long=0) with (step s1: true => c = x + s1.c;)`,
		`datatable(e:string)["a"] | scan with_match_id=id with (step s1: e == "a"; step s2 output=last: e == "b";)`,
		`search "x" | take 1`, `find where * has "x" project a`, `declare pattern p = (a:string) { ("x") = { print 1 }; }; p("x")`,
		`set query_now = datetime(2020-01-01); print now()`, `T | partition by k (summarize count())`, `T | evaluate pivot(k, sum(v))`,
		`T | mv-apply v=a to typeof(long) on (top 1 by v | summarize s=sum(v))`, `print x=dynamic({"d": datetime(2020-01-01)})`,
		`T | summarize percentiles(x, 50, 99.9)`, `T | project-by-names "a*", dynamic(["b"])`, `print 0xFFFFFFFF`,
		`T | extend a = pack_all(true)`, `union withsource=s (T | as A), (U | as B)`, `T | evaluate narrow()`,
		`datatable(s:string)["a b"] | reduce kind=source by s with threshold=0.5, characters="-"`,
		`datatable(t:datetime, e:string, u:string)[] | evaluate sequence_detect(t, 1h, 2h, a=(e=="a"), e=="b", u)`,
		`datatable(u:string, t:datetime)[] | evaluate sliding_window_counts(u, t, datetime(2020-01-01), datetime(2020 - 01 - 07), 3d, 1d)`,
		`let V_1 = view () { print x=1 }; union isfuzzy=true V*, *X, (V_1 | where x > 0)`,
		`print guid({12e8b78d-55b4-46ae-b068-26d7a0080254}), f(a=1, 2)`,
		`print x=dynamic(["2020-01-01"]) | mv-expand x to typeof(datetime), y to typeof(guid)`,
		`let f = (c:string, n:long) { datatable(a:long)[1] | project v = column_ifexists(c, -1) | take n }; f("a", 2)`,
		`let T = datatable(x:long)[1]; let g = (t:string) { table(t, "hotcache") | count }; g("T")`,
		`print a = datetime(2020-01-02) between (datetime(2020-01-01) .. 1d), b = "x" !in~ ("X", ""), c = string(null) != "a"`,
		`datatable(s:string, t:string)["a b", "b"] | where s has t or s !hasprefix_cs t`,
		`print (p, q) = array_sort_asc(dynamic([2,1]), dynamic(["b","a"]), false), r = 1.0 / 0.0`,
		`let y = () { toscalar(rand()) }; print a = y, b = y, c = bag_pack_columns(a)`,
	} {
		f.Add(seed)
	}
	compiler := ksql.New(dialect.SQLite(dialect.WithRegexFunction("kql_regex")), ksql.WithParameters(), ksql.WithCatalog(listedCatalog{investigationCatalog()}))
	f.Fuzz(func(t *testing.T, source string) {
		if len(source) > 1024 {
			return
		}
		// Compilation is bounded work; a slow input is a denial of service.
		done := make(chan struct{})
		go func() {
			defer close(done)
			compiler.Compile(source)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("compiling %q took more than 5s", source)
		}
	})
}
