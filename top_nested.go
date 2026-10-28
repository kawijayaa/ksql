package ksql

import (
	"fmt"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// topHitters lowers top-hitters exactly, as Kusto's approximation of
// summarize count() (or sum(By)) by Value | top Count by that desc.
func (s *compileState) topHitters(input Relation, operator kql.Operator) Relation {
	spec := operator.Body.(kql.TopHittersSpec)
	value := spec.Value
	name := inferredName(value, "")
	if named, ok := value.(*kql.NamedExpression); ok {
		name = unquoteKQL(named.Name)
	}
	if name == "" {
		s.bindError("KQLB0362", operator.Span, "operator.top-hitters", "top-hitters requires a named value expression")
		return Relation{}
	}
	var aggregate kql.Expression = &kql.CallExpression{Function: &kql.NameExpression{Name: "count"}}
	total := "approximate_count_" + name
	if spec.By != nil {
		aggregate = &kql.CallExpression{Function: &kql.NameExpression{Name: "sum"}, Args: []kql.Expression{spec.By}}
		total = "approximate_sum_" + inferredName(spec.By, "")
	}
	if _, ok := value.(*kql.NamedExpression); !ok {
		value = &kql.NamedExpression{Name: name, Value: value}
	}
	summarized := s.operator(input, kql.Operator{Kind: "summarize", Body: kql.SummarizeSpec{
		Aggregates: []kql.Expression{&kql.NamedExpression{Name: total, Value: aggregate}},
		By:         []kql.Expression{value},
	}, Span: operator.Span})
	if summarized.Query == nil {
		return Relation{}
	}
	return s.operator(summarized, kql.Operator{Kind: "top", Body: kql.TopSpec{
		Count: spec.Count,
		By:    kql.OrderedExpression{Expression: &kql.NameExpression{Name: searchName(total)}, Direction: "desc", Nulls: "last"},
	}, Span: operator.Span})
}

// topNested lowers top-nested levels: level i summarizes by the first i
// values, ranks each row within its parent group (the first i-1 values)
// and keeps the top N whose parent was kept by level i-1.
func (s *compileState) topNested(input Relation, operator kql.Operator) Relation {
	spec := operator.Body.(kql.TopNestedSpec)
	feature := "operator.top-nested"
	fail := func(span kql.Span, message string) Relation {
		s.bindError("KQLB0362", span, feature, message)
		return Relation{}
	}
	if len(spec.Levels) > 16 {
		return fail(operator.Span, "top-nested supports at most 16 levels")
	}
	var values, aggregates []string
	var groups []kql.Expression
	var previous Relation
	for i, level := range spec.Levels {
		if level.Others != nil {
			return fail(level.Span, "top-nested with others is not supported")
		}
		value := level.Value
		name := inferredName(value, "")
		if named, ok := value.(*kql.NamedExpression); ok {
			name = unquoteKQL(named.Name)
			value = named.Value
		}
		if name == "" {
			name = fmt.Sprintf("Column%d", i+1)
		}
		aggregate := level.Aggregation
		aggregateName := "aggregated_" + name
		if named, ok := aggregate.(*kql.NamedExpression); ok {
			aggregateName, aggregate = unquoteKQL(named.Name), named.Value
		}
		values = append(values, name)
		aggregates = append(aggregates, aggregateName)
		groups = append(groups, &kql.NamedExpression{Name: name, Value: value})
		summarized := s.operator(input, kql.Operator{Kind: "summarize", Body: kql.SummarizeSpec{
			Aggregates: []kql.Expression{&kql.NamedExpression{Name: aggregateName, Value: aggregate}},
			By:         append([]kql.Expression{}, groups...),
		}, Span: level.Span})
		if summarized.Query == nil {
			return Relation{}
		}
		// Rank within the parent group.
		var partition []sqlast.Expr
		for _, parent := range values[:i] {
			partition = append(partition, &sqlast.Identifier{Parts: []string{parent}})
		}
		direction := "DESC"
		if level.Ascending {
			direction = "ASC"
		}
		rank := &sqlast.Window{Expr: &sqlast.Call{Name: "row_number"}, PartitionBy: partition,
			OrderBy: []sqlast.Order{{Expr: &sqlast.Identifier{Parts: []string{aggregateName}}, Direction: direction, Nulls: "last"}}}
		ranked := &sqlast.Select{From: &sqlast.Subquery{Query: summarized.Query, Alias: s.nextAlias()}, Projections: []sqlast.SelectItem{
			{Expr: &sqlast.Star{}}, {Expr: rank, Alias: "__top_nested_rank"},
		}}
		var where sqlast.Expr
		if level.Count != nil {
			if !s.validateLimit(level.Count, operator) {
				return Relation{}
			}
			limit := s.expression(level.Count)
			if limit.SQL == nil {
				return Relation{}
			}
			where = &sqlast.Binary{Left: &sqlast.Identifier{Parts: []string{"__top_nested_rank"}}, Operator: "<=", Right: limit.SQL}
		}
		current := s.nextAlias()
		levelQuery := &sqlast.Select{From: &sqlast.Subquery{Query: ranked, Alias: current}, Where: where}
		schema := Schema{}
		if i == 0 {
			for _, column := range summarized.Schema.Columns {
				levelQuery.Projections = append(levelQuery.Projections, sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{current, column.Name}}, Alias: column.Name})
				schema.Columns = append(schema.Columns, column)
			}
			previous = Relation{Query: levelQuery, Schema: schema}
			continue
		}
		// Keep rows whose parent the previous level kept, adding its columns.
		parentAlias := s.nextAlias()
		var on sqlast.Expr
		for _, parent := range values[:i] {
			left := &sqlast.Identifier{Parts: []string{current, parent}}
			right := &sqlast.Identifier{Parts: []string{parentAlias, parent}}
			equal := &sqlast.Binary{
				Left: &sqlast.Binary{Left: left, Operator: "=", Right: right}, Operator: "OR",
				Right: &sqlast.Binary{Left: &sqlast.Binary{Left: left, Operator: "IS", Right: &sqlast.Literal{Kind: sqlast.NullLiteral}}, Operator: "AND",
					Right: &sqlast.Binary{Left: right, Operator: "IS", Right: &sqlast.Literal{Kind: sqlast.NullLiteral}}},
			}
			if on == nil {
				on = equal
			} else {
				on = &sqlast.Binary{Left: on, Operator: "AND", Right: equal}
			}
		}
		levelQuery.From = &sqlast.Join{Left: &sqlast.Subquery{Query: ranked, Alias: current}, Right: &sqlast.Subquery{Query: previous.Query, Alias: parentAlias}, Kind: "INNER", On: on}
		for _, column := range previous.Schema.Columns {
			levelQuery.Projections = append(levelQuery.Projections, sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{parentAlias, column.Name}}, Alias: column.Name})
			schema.Columns = append(schema.Columns, column)
		}
		for _, column := range summarized.Schema.Columns {
			if column.Name == name || column.Name == aggregateName {
				levelQuery.Projections = append(levelQuery.Projections, sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{current, column.Name}}, Alias: column.Name})
				schema.Columns = append(schema.Columns, column)
			}
		}
		previous = Relation{Query: levelQuery, Schema: schema}
	}
	// Kusto's column order: each level's value then its aggregation.
	output := Schema{}
	var projections []sqlast.SelectItem
	for i := range values {
		for _, name := range []string{values[i], aggregates[i]} {
			column, _, _, _ := previous.Schema.Lookup(name)
			output.Columns = append(output.Columns, column)
			projections = append(projections, sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{name}}, Alias: name})
		}
	}
	s.validateUnique(output.Columns, operator.Span, feature)
	return Relation{Query: &sqlast.Select{From: &sqlast.Subquery{Query: previous.Query, Alias: s.nextAlias()}, Projections: projections}, Schema: output}
}
