package kql

import "testing"

func TestQueryParameterSyntaxAndTemporalLiterals(t *testing.T) {
	script, errs := Parse(`declare query_parameters(['a b']:int=3,d:datetime=datetime(2026-01-02T03:04:05.123Z),n:datetime=datetime(null));print d`)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	declaration, ok := script.Statements[0].(*QueryParametersStatement)
	if !ok || len(declaration.Parameters) != 3 {
		t.Fatalf("missing typed declaration: %#v", script.Statements)
	}
	call := declaration.Parameters[1].Default.(*CallExpression)
	literal, ok := call.Args[0].(*LiteralExpression)
	if !ok {
		t.Fatalf("date treated as arithmetic: %T", call.Args[0])
	}
	text, err := StringValue(literal)
	if err != nil || text != "2026-01-02T03:04:05.123Z" {
		t.Fatalf("date changed: %q %v", text, err)
	}
	for _, query := range []string{`declare query_parameters(x:long,)`, `declare query_parameters(x:long=)`, `declare query_parameters(x)`, `declare query_parameters(x:(*))`, `declare query_parameters x:long`, `declare query_parameters(x:long) print x`} {
		if _, errs := Parse(query); len(errs) == 0 {
			t.Fatalf("invalid declaration accepted: %s", query)
		}
	}
}
