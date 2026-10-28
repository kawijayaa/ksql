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
