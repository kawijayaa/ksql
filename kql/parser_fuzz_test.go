package kql

import (
	"testing"
	"time"
)

// Parse must never panic: malformed input returns errors instead.
func FuzzParseNeverPanics(f *testing.F) {
	for _, seed := range []string{
		`T | where x between (1 .. 2)`, `print x between (1 ..)`, `T | parse s with * "a=" a:long *`, `T | parse-kv s as (a) with (pair_delimiter=",", kv_delimiter="=")`,
		`T | summarize arg_max(n, *) by bin(t, 1h)`, `let f = (x:long) { x * 2 }; print f(1)`, `T | join kind=inner (U) on $left.a == $right.b`,
		`T | mv-apply x on (where x > 1)`, `datatable(a:long)[1,2] | render timechart with (title="x")`, `print time(1.02:03:04), datetime(2020-01-01 10:00)`,
		`declare query_parameters(a:long=1); T | where n == a`, `.show tables`, `T | extend p=prev(n), r=row_number(1, prev(a) != a)`, `print dynamic({"a":[1,2]})`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		if len(source) > 2048 {
			return
		}
		Parse(source)
	})
}

func TestMalformedRangesReportErrors(t *testing.T) {
	for _, query := range []string{`print x between (1 ..)`, `print x between (..)`, `print x between (1 .. 2`, `print x between (.. 2)`} {
		if _, errs := Parse(query); len(errs) == 0 {
			t.Errorf("%s: accepted", query)
		}
	}
	if _, errs := Parse(`print x between (1 .. 2)`); len(errs) != 0 {
		t.Fatalf("valid range: %v", errs)
	}
}

func TestQueryHintsAreIgnored(t *testing.T) {
	for _, query := range []string{
		`T | summarize hint.strategy = shuffle count() by Host`,
		`T | summarize hint.shufflekey = Host hint.num_partitions = 10 count() by Host`,
		`T | join kind=inner hint.strategy=broadcast (U) on Host`,
		`T | lookup hint.remote=auto (U) on Host`,
		`T | join hint.strategy=shuffle hint.shufflekey=Host (U | summarize hint.strategy=shuffle count() by Host) on Host`,
	} {
		if _, errs := Parse(query); len(errs) != 0 {
			t.Errorf("%s: %v", query, errs)
		}
	}
	script, errs := Parse(`T | as hint.materialized=true X`)
	if len(errs) != 0 || script.Statements[0].(*ExpressionStatement).Pipeline.Operators[0].Body.(AsSpec).Materialized == nil {
		t.Fatalf("as hint.materialized must stay semantic: %v", errs)
	}
}

// Unicode digits outside ASCII once sent the lexer into an endless loop of
// empty number tokens.
func TestLexerTerminatesOnUnicodeDigits(t *testing.T) {
	for _, source := range []string{"print ٦", "print x=1٦2", "٣", "print 0x", "print 1.e٦"} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			Parse(source)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("lexing %q did not terminate", source)
		}
	}
}
