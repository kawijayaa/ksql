package ksql

import (
	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// reduce groups By values into patterns through the host's
// reduce_patterns(value, threshold, characters) aggregate, which returns
// [[Pattern, Count, Representative], ...]. kind=source uses
// reduce_source(value, threshold, characters), returning [[value, Pattern],
// ...], and appends each input row's Pattern.
func (s *compileState) reduce(input Relation, operator kql.Operator) Relation {
	spec := operator.Body.(kql.ReduceSpec)
	feature := "operator.reduce"
	if spec.Source && input.Schema.Unknown {
		s.bindError("KQLB0364", operator.Span, feature, "reduce kind=source requires a bound input schema")
		return Relation{}
	}
	threshold, characters := spec.Threshold, spec.Characters
	if threshold == nil {
		threshold = &kql.LiteralExpression{Text: "0.1", Kind: kql.NumberToken}
	}
	if characters == nil {
		characters = &kql.LiteralExpression{Text: `""`, Kind: kql.StringToken}
	}
	name := func(name string) *kql.NameExpression { return &kql.NameExpression{Name: name} }
	call := func(function string, args ...kql.Expression) kql.Expression {
		return &kql.CallExpression{Function: name(function), Args: args}
	}
	index := func(i string) kql.Expression {
		return &kql.IndexExpression{Value: name("__reduce"), Index: &kql.LiteralExpression{Text: i, Kind: kql.NumberToken}}
	}
	run := func(relation Relation, kind string, body kql.OperatorBody) Relation {
		if relation.Query == nil {
			return Relation{}
		}
		return s.operator(relation, kql.Operator{Kind: kind, Body: body, Span: operator.Span})
	}
	value, aggregate := spec.By, "reduce_patterns"
	source := input
	if spec.Source {
		// Evaluate By once per row so the mapping joins on the same text.
		source = run(input, "extend", kql.ExpressionSpec{Expressions: []kql.Expression{
			&kql.NamedExpression{Name: "__reduce_value", Value: call("tostring", spec.By)},
		}})
		value, aggregate = name("__reduce_value"), "reduce_source"
	}
	rows := run(source, "summarize", kql.SummarizeSpec{Aggregates: []kql.Expression{
		&kql.NamedExpression{Name: "__reduce", Value: call(aggregate, value, threshold, characters)},
	}})
	rows = run(rows, "mv-expand", kql.MvExpandSpec{Items: []kql.MvExpansion{{Name: "__reduce", Expression: name("__reduce")}}})
	if !spec.Source {
		rows = run(rows, "project", kql.ExpressionSpec{Expressions: []kql.Expression{
			&kql.NamedExpression{Name: "Pattern", Value: call("tostring", index("0"))},
			&kql.NamedExpression{Name: "Count", Value: call("tolong", index("1"))},
			&kql.NamedExpression{Name: "Representative", Value: call("tostring", index("2"))},
		}})
		return run(rows, "sort", kql.SortSpec{Expressions: []kql.OrderedExpression{{Expression: name("Count"), Direction: "desc", Nulls: "last"}}})
	}
	rows = run(rows, "project", kql.ExpressionSpec{Expressions: []kql.Expression{
		&kql.NamedExpression{Name: "__reduce_key", Value: call("tostring", index("0"))},
		&kql.NamedExpression{Name: "Pattern", Value: call("tostring", index("1"))},
	}})
	if rows.Query == nil || source.Query == nil {
		return Relation{}
	}
	left, right := s.nextAlias(), s.nextAlias()
	query := &sqlast.Select{From: &sqlast.Join{
		Left: &sqlast.Subquery{Query: source.Query, Alias: left}, Right: &sqlast.Subquery{Query: rows.Query, Alias: right}, Kind: "LEFT",
		On: &sqlast.Binary{Left: &sqlast.Identifier{Parts: []string{left, "__reduce_value"}}, Operator: "=", Right: &sqlast.Identifier{Parts: []string{right, "__reduce_key"}}},
	}}
	schema := Schema{}
	for _, column := range input.Schema.Columns {
		if column.Name == "Pattern" {
			continue
		}
		query.Projections = append(query.Projections, sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{left, column.Name}}, Alias: column.Name})
		schema.Columns = append(schema.Columns, column)
	}
	query.Projections = append(query.Projections, sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{right, "Pattern"}}, Alias: "Pattern"})
	schema.Columns = append(schema.Columns, Column{Name: "Pattern", Type: TypeString})
	return Relation{Query: query, Schema: schema}
}
