package kql

import "testing"

func TestParseOperatorSpec(t *testing.T) {
	script, errs := Parse(`T | parse kind=regex flags=Ui tostring(Data) with * "a=" a:long "b" "c" ['d e'] @'\d' *`)
	if len(errs) != 0 {
		t.Fatalf("Parse() errors = %v", errs)
	}
	spec := script.Statements[0].(*ExpressionStatement).Pipeline.Operators[0].Body.(ParseSpec)
	if spec.Kind != "regex" || spec.Flags != "Ui" {
		t.Fatalf("kind/flags = %q/%q", spec.Kind, spec.Flags)
	}
	if _, ok := spec.Source.(*CallExpression); !ok {
		t.Fatalf("source = %T", spec.Source)
	}
	want := []ParseItem{
		{Kind: ParseWildcard}, {Kind: ParseConstant, Text: "a="}, {Kind: ParseColumn, Name: "a", Type: "long"},
		{Kind: ParseConstant, Text: "bc"}, {Kind: ParseColumn, Name: "'d e'"}, {Kind: ParseConstant, Text: `\d`}, {Kind: ParseWildcard},
	}
	if len(spec.Pattern) != len(want) {
		t.Fatalf("pattern = %#v", spec.Pattern)
	}
	for i, item := range spec.Pattern {
		if item.Kind != want[i].Kind || item.Text != want[i].Text || item.Name != want[i].Name || item.Type != want[i].Type {
			t.Fatalf("pattern[%d] = %#v, want %#v", i, item, want[i])
		}
	}
	defaults, errs := Parse(`T | parse-where kind with "x" x`)
	if len(errs) != 0 || defaults.Statements[0].(*ExpressionStatement).Pipeline.Operators[0].Body.(ParseSpec).Kind != "simple" {
		t.Fatalf("column named kind: %v", errs)
	}
}

func TestParseOperatorSyntaxErrors(t *testing.T) {
	for _, query := range []string{
		`T | parse`, `T | parse s`, `T | parse s with`, `T | parse kind=other s with "a" a`,
		`T | parse kind=simple kind=regex s with "a" a`, `T | parse s with "a" a: `, `T | parse s with "a" 1`,
		`T | parse s with "a" a , "b" b`, `T | parse unknown=1 s with "a" a`,
	} {
		if _, errs := Parse(query); len(errs) == 0 || errs[0].Code != "KQLP0350" {
			t.Errorf("%s: errors = %v", query, errs)
		}
	}
}
