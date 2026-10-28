package ksql

import (
	"github.com/kawijayaa/ksql/kql"
)

// slidingWindowCounts lowers evaluate sliding_window_counts(Id, Timeline,
// Start, End, Lookback, Bin [, Dim...]) with Kusto's sliding-window
// technique: records between Start and End fall in the Bin-aligned bin
// (from Start) of their time, are expanded to every window ending within
// Lookback (rounded down to whole bins) after it, and are counted per
// window up to End.
func (s *compileState) slidingWindowCounts(input Relation, spec kql.EvaluateSpec, operator kql.Operator, feature string) Relation {
	fail := func(message string) Relation {
		s.bindError("KQLB0417", spec.Span, feature, message)
		return Relation{}
	}
	if input.Schema.Unknown {
		return fail("sliding_window_counts requires a known input schema")
	}
	if len(spec.Args) < 6 {
		return fail("sliding_window_counts requires Id, Timeline, Start, End, LookbackWindow and Bin")
	}
	timelineName, ok := spec.Args[1].(*kql.NameExpression)
	if !ok {
		return fail("sliding_window_counts timeline must be a column")
	}
	timeline, _, found, _ := input.Schema.Lookup(unquoteKQL(timelineName.Name))
	if !found {
		s.bindError("KQLB0401", timelineName.Span, feature, "unknown column "+timelineName.Name)
		return Relation{}
	}
	switch timeline.Type {
	case TypeDateTime, TypeTimespan, TypeLong, TypeInt, TypeReal:
	default:
		return fail("sliding_window_counts timeline must be a datetime, timespan or numeric column")
	}
	if literal, ok := spec.Args[5].(*kql.LiteralExpression); ok && literal.Kind == kql.StringToken {
		return fail("sliding_window_counts calendar bins (week, month, year) are not supported")
	}
	name := func(name string) kql.Expression { return &kql.NameExpression{Name: name} }
	binary := func(left kql.Expression, op string, right kql.Expression) kql.Expression {
		return &kql.BinaryExpression{Left: left, Operator: op, Right: right}
	}
	call := func(function string, args ...kql.Expression) kql.Expression {
		return &kql.CallExpression{Function: name(function), Args: args}
	}
	at, start, end, lookback, step := spec.Args[1], spec.Args[2], spec.Args[3], spec.Args[4], spec.Args[5]
	run := func(relation Relation, kind string, body kql.OperatorBody) Relation {
		if relation.Query == nil {
			return Relation{}
		}
		return s.operator(relation, kql.Operator{Kind: kind, Body: body, Span: operator.Span})
	}
	where := func(relation Relation, condition kql.Expression) Relation {
		return run(relation, "where", kql.ExpressionSpec{Expressions: []kql.Expression{condition}})
	}
	rows := where(input, binary(binary(at, ">=", start), "and", binary(at, "<=", end)))
	rows = run(rows, "extend", kql.ExpressionSpec{Expressions: []kql.Expression{
		&kql.NamedExpression{Name: "__swc_bin", Value: call("bin_at", at, step, start)},
	}})
	rows = run(rows, "mv-expand", kql.MvExpandSpec{Items: []kql.MvExpansion{{
		Name: "__swc_window", Type: string(timeline.Type),
		Expression: call("range", name("__swc_bin"), binary(binary(name("__swc_bin"), "+", lookback), "-", step), step),
	}}})
	rows = where(rows, binary(name("__swc_window"), "<=", end))
	by := []kql.Expression{&kql.NamedExpression{Name: timeline.Name, Value: name("__swc_window")}}
	by = append(by, spec.Args[6:]...)
	rows = run(rows, "summarize", kql.SummarizeSpec{
		Aggregates: []kql.Expression{
			&kql.NamedExpression{Name: "Count", Value: call("count")},
			&kql.NamedExpression{Name: "dcount", Value: call("dcount", spec.Args[0])},
		},
		By: by,
	})
	return run(rows, "sort", kql.SortSpec{Expressions: []kql.OrderedExpression{{Expression: name(timeline.Name), Direction: "asc", Nulls: "first"}}})
}
