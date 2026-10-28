package ksql

import (
	"fmt"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// maxSequenceSteps bounds sequence_detect's self-joins.
const maxSequenceSteps = 16

// sequenceDetect lowers evaluate sequence_detect(Timeline, MaxStepWindow,
// MaxSpan, Step1, Step2, ..., Dim1, Dim2, ...). Each step is the input
// filtered by its predicate; consecutive steps join on equal dimensions with
// a strictly later time no more than MaxStepWindow after the previous step
// and MaxSpan after the first. Like Kusto, every chain of matching records
// is an occurrence, with the dimensions, each step's time and the Duration.
func (s *compileState) sequenceDetect(input Relation, spec kql.EvaluateSpec, feature string) Relation {
	fail := func(message string) Relation {
		s.bindError("KQLB0417", spec.Span, feature, message)
		return Relation{}
	}
	if input.Schema.Unknown {
		return fail("sequence_detect requires a known input schema")
	}
	if len(spec.Args) < 4 {
		return fail("sequence_detect requires a timeline column, a step window, a sequence span and step predicates")
	}
	timelineName, ok := spec.Args[0].(*kql.NameExpression)
	if !ok {
		return fail("sequence_detect timeline must be a column")
	}
	timelineColumn, _, found, _ := input.Schema.Lookup(unquoteKQL(timelineName.Name))
	if !found {
		s.bindError("KQLB0401", timelineName.Span, feature, "unknown column "+timelineName.Name)
		return Relation{}
	}
	if timelineColumn.Type != TypeDateTime && timelineColumn.Type != TypeUnknown {
		return fail("sequence_detect timeline must be a datetime column")
	}
	previous := s.input
	s.input = input.Schema
	defer func() { s.input = previous }()
	for _, limit := range spec.Args[1:3] {
		bound := s.expression(limit)
		if bound.SQL == nil {
			return Relation{}
		}
		if bound.Type != TypeTimespan && bound.Type != TypeUnknown {
			return fail("sequence_detect windows must be timespans")
		}
	}
	type part struct {
		name string
		sql  sqlast.Expr
		kind ScalarType
	}
	var steps, dims []part
	for i, arg := range spec.Args[3:] {
		name, value := "", arg
		if named, ok := arg.(*kql.NamedExpression); ok {
			name, value = unquoteKQL(named.Name), named.Value
		}
		bound := s.expression(value)
		if bound.SQL == nil {
			return Relation{}
		}
		// Steps are the leading Boolean predicates; dimensions follow.
		if bound.Type == TypeBool && len(dims) == 0 {
			if name == "" {
				name = fmt.Sprintf("Expr%d", i+1)
			}
			steps = append(steps, part{name, bound.SQL, bound.Type})
			continue
		}
		if name == "" {
			name = inferredName(value, "")
		}
		if name == "" {
			return fail("sequence_detect dimensions must be columns or named expressions")
		}
		dims = append(dims, part{name, bound.SQL, bound.Type})
	}
	if len(steps) == 0 {
		return fail("sequence_detect requires at least one Boolean step predicate")
	}
	if len(steps) > maxSequenceSteps {
		return fail(fmt.Sprintf("sequence_detect supports at most %d steps", maxSequenceSteps))
	}
	column := func(step int, field string) string { return fmt.Sprintf("__seq%d_%s", step, field) }
	ref := func(step int, field string) kql.Expression { return &kql.NameExpression{Name: column(step, field)} }
	combined := Schema{}
	var from sqlast.Source
	for k, step := range steps {
		query := &sqlast.Select{From: &sqlast.Subquery{Query: input.Query, Alias: s.nextAlias()}, Where: step.sql, Projections: []sqlast.SelectItem{
			{Expr: &sqlast.Identifier{Parts: []string{timelineColumn.Name}}, Alias: column(k, "t")},
		}}
		combined.Columns = append(combined.Columns, Column{Name: column(k, "t"), Type: timelineColumn.Type})
		for j, dim := range dims {
			query.Projections = append(query.Projections, sqlast.SelectItem{Expr: dim.sql, Alias: column(k, fmt.Sprint("d", j))})
			combined.Columns = append(combined.Columns, Column{Name: column(k, fmt.Sprint("d", j)), Type: dim.kind})
		}
		source := &sqlast.Subquery{Query: query, Alias: s.nextAlias()}
		if k == 0 {
			from = source
			continue
		}
		var on kql.Expression = &kql.BinaryExpression{Left: ref(k, "t"), Operator: ">", Right: ref(k-1, "t")}
		and := func(right kql.Expression) { on = &kql.BinaryExpression{Left: on, Operator: "and", Right: right} }
		and(&kql.BinaryExpression{Left: &kql.BinaryExpression{Left: ref(k, "t"), Operator: "-", Right: ref(k-1, "t")}, Operator: "<=", Right: spec.Args[1]})
		and(&kql.BinaryExpression{Left: &kql.BinaryExpression{Left: ref(k, "t"), Operator: "-", Right: ref(0, "t")}, Operator: "<=", Right: spec.Args[2]})
		s.input = combined
		bound := s.expression(on)
		if bound.SQL == nil {
			return Relation{}
		}
		condition := bound.SQL
		// Dimensions match when equal or both null.
		for j := range dims {
			left := &sqlast.Identifier{Parts: []string{column(k, fmt.Sprint("d", j))}}
			right := &sqlast.Identifier{Parts: []string{column(0, fmt.Sprint("d", j))}}
			isNull := func(e sqlast.Expr) sqlast.Expr {
				return &sqlast.Binary{Left: e, Operator: "IS", Right: &sqlast.Literal{Kind: sqlast.NullLiteral}}
			}
			equal := &sqlast.Binary{
				Left: &sqlast.Binary{Left: left, Operator: "=", Right: right}, Operator: "OR",
				Right: &sqlast.Binary{Left: isNull(left), Operator: "AND", Right: isNull(right)},
			}
			condition = &sqlast.Binary{Left: condition, Operator: "AND", Right: equal}
		}
		from = &sqlast.Join{Left: from, Right: source, Kind: "INNER", On: condition}
	}
	s.input = combined
	schema := Schema{}
	query := &sqlast.Select{From: from}
	add := func(name string, expression kql.Expression) bool {
		bound := s.expression(expression)
		if bound.SQL == nil {
			return false
		}
		query.Projections = append(query.Projections, sqlast.SelectItem{Expr: bound.SQL, Alias: name})
		schema.Columns = append(schema.Columns, Column{Name: name, Type: bound.Type})
		return true
	}
	for j, dim := range dims {
		if !add(dim.name, ref(0, fmt.Sprint("d", j))) {
			return Relation{}
		}
	}
	for k, step := range steps {
		if !add(step.name+"_"+timelineColumn.Name, ref(k, "t")) {
			return Relation{}
		}
	}
	if !add("Duration", &kql.BinaryExpression{Left: ref(len(steps)-1, "t"), Operator: "-", Right: ref(0, "t")}) {
		return Relation{}
	}
	s.validateUnique(schema.Columns, spec.Span, feature)
	query.OrderBy = []sqlast.Order{{Expr: &sqlast.Identifier{Parts: []string{steps[0].name + "_" + timelineColumn.Name}}, Direction: "ASC"}}
	return Relation{Query: query, Schema: schema}
}
