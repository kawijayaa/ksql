package ksql

import (
	"fmt"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// maxSeriesLength is Kusto's make-series array limit (2^20).
const maxSeriesLength = 1 << 20

// makeSeries follows Microsoft's make-series: rows are grouped by the by
// expressions and bin_at(Axis, step, start); each aggregation becomes an
// array over every bin of [start, end) with absent bins set to the default
// (0 unless given), followed by the array of bin values. Without from, each
// series starts at its first bin with data (bin-aligned); without to, it ends
// after its last bin with data.
func (s *compileState) makeSeries(input Relation, operator kql.Operator) Relation {
	feature := "operator.make-series"
	spec := operator.Body.(kql.MakeSeriesSpec)
	fail := func(code, message string) Relation {
		s.bindError(code, operator.Span, feature, message)
		return Relation{}
	}
	if s.compiler.dialect.Name() != "sqlite" {
		s.error("KQLL0394", operator.Span, feature, "make-series requires ordered JSON aggregation on SQLite")
		return Relation{}
	}
	if input.Schema.Unknown {
		return fail("KQLB0417", "make-series requires a known input schema")
	}
	previousInput, previousQualifier, previousAggregation := s.input, s.qualifier, s.aggregationAllowed
	defer func() {
		s.input, s.qualifier, s.aggregationAllowed = previousInput, previousQualifier, previousAggregation
	}()

	inputAlias, boundsAlias, groupsAlias, seriesAlias, aggregateAlias := s.nextAlias(), s.nextAlias(), s.nextAlias(), s.nextAlias(), s.nextAlias()
	s.input, s.qualifier = input.Schema, inputAlias
	axis := s.expression(spec.Axis)
	step := s.expression(spec.Step)
	if axis.SQL == nil || step.SQL == nil {
		return Relation{}
	}
	temporal := axis.Type == TypeDateTime
	switch {
	case temporal && step.Type != TypeTimespan:
		return fail("KQLB0417", "make-series over a datetime axis requires a timespan step")
	case axis.Type == TypeTimespan && step.Type != TypeTimespan:
		return fail("KQLB0417", "make-series over a timespan axis requires a timespan step")
	case !temporal && axis.Type != TypeTimespan && !isNumeric(axis.Type) && axis.Type != TypeUnknown:
		return fail("KQLB0417", "make-series axis must be datetime, timespan or numeric")
	}
	if temporal && !supportsDatetimeArithmetic(s.compiler.dialect) {
		s.error("KQLL0412", operator.Span, feature, "make-series over datetimes requires a dialect datetime capability")
		return Relation{}
	}
	bound := func(expression kql.Expression) boundExpr {
		if expression == nil {
			return boundExpr{}
		}
		s.qualifier = ""
		value := s.expression(expression)
		s.qualifier = inputAlias
		return value
	}
	from, to := bound(spec.From), bound(spec.To)
	if (spec.From != nil && from.SQL == nil) || (spec.To != nil && to.SQL == nil) {
		return Relation{}
	}

	number := func(text string) sqlast.Expr { return &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: text} }
	integer := func(expr sqlast.Expr) sqlast.Expr { return &sqlast.Cast{Expr: expr, Type: "INTEGER"} }
	// floorDiv and ceilDiv round a non-negative or negative quotient correctly.
	floorDiv := func(numerator, denominator sqlast.Expr) sqlast.Expr {
		quotient := integer(&sqlast.Binary{Left: &sqlast.Cast{Expr: numerator, Type: "REAL"}, Operator: "/", Right: denominator})
		below := &sqlast.Binary{Left: numerator, Operator: "<", Right: &sqlast.Binary{Left: quotient, Operator: "*", Right: denominator}}
		return &sqlast.Binary{Left: quotient, Operator: "-", Right: &sqlast.Case{Branches: []sqlast.When{{Condition: below, Result: number("1")}}, Else: number("0")}}
	}
	ceilDiv := func(numerator, denominator sqlast.Expr) sqlast.Expr {
		quotient := floorDiv(numerator, denominator)
		exact := &sqlast.Binary{Left: &sqlast.Binary{Left: quotient, Operator: "*", Right: denominator}, Operator: "=", Right: numerator}
		return &sqlast.Binary{Left: quotient, Operator: "+", Right: &sqlast.Case{Branches: []sqlast.When{{Condition: exact, Result: number("0")}}, Else: number("1")}}
	}
	difference := func(left, right sqlast.Expr) sqlast.Expr {
		if temporal {
			return &sqlast.DatetimeDiff{Left: left, Right: right}
		}
		return &sqlast.Binary{Left: left, Operator: "-", Right: right}
	}
	offset := func(start, bins sqlast.Expr) sqlast.Expr {
		span := &sqlast.Binary{Left: bins, Operator: "*", Right: step.SQL}
		if temporal {
			return &sqlast.DatetimeAdd{Datetime: start, Ticks: span}
		}
		return &sqlast.Binary{Left: start, Operator: "+", Right: span}
	}
	// bin() alignment: datetimes from 0001-01-01, other axes from zero.
	align := func(value sqlast.Expr) sqlast.Expr {
		if temporal {
			literal := &kql.LiteralExpression{Text: `"0001-01-01"`, Kind: kql.StringToken, Span: operator.Span}
			epoch := s.expression(&kql.CallExpression{Function: &kql.NameExpression{Name: "datetime", Span: operator.Span}, Args: []kql.Expression{literal}, Span: operator.Span}).SQL
			return offset(epoch, floorDiv(difference(value, epoch), step.SQL))
		}
		return &sqlast.Binary{Left: floorDiv(value, step.SQL), Operator: "*", Right: step.SQL}
	}

	// Bounds per group: start and bin count.
	groups, groupSchema := s.projectItems(spec.By, true, input.Schema, false)
	if !s.checkNonDynamicColumns(groupSchema.Columns, operator.Span, feature) {
		return Relation{}
	}
	groupExprs := make([]sqlast.Expr, len(groups))
	for i, item := range groups {
		groupExprs[i] = item.Expr
	}
	minimum := &sqlast.Call{Name: "min", Args: []sqlast.Expr{axis.SQL}}
	maximum := &sqlast.Call{Name: "max", Args: []sqlast.Expr{axis.SQL}}
	start := sqlast.Expr(nil)
	if from.SQL != nil {
		start = from.SQL
		if spec.Range {
			start = align(from.SQL)
		}
	} else {
		start = align(minimum)
	}
	var count sqlast.Expr
	switch {
	case to.SQL != nil && spec.Range:
		count = &sqlast.Binary{Left: floorDiv(difference(to.SQL, start), step.SQL), Operator: "+", Right: number("1")}
	case to.SQL != nil:
		count = ceilDiv(difference(to.SQL, start), step.SQL)
	default:
		count = &sqlast.Binary{Left: floorDiv(difference(maximum, start), step.SQL), Operator: "+", Right: number("1")}
	}
	limited := &sqlast.Case{Branches: []sqlast.When{
		{Condition: &sqlast.Binary{Left: count, Operator: ">", Right: number(fmt.Sprint(maxSeriesLength))}, Result: number(fmt.Sprint(maxSeriesLength))},
		{Condition: &sqlast.Binary{Left: count, Operator: "<", Right: number("0")}, Result: number("0")},
	}, Else: count}
	boundsItems := append([]sqlast.SelectItem{}, groups...)
	// count(*) keeps the bounds an aggregate, so ungrouped input yields a row.
	boundsItems = append(boundsItems, sqlast.SelectItem{Expr: start, Alias: "__ksql_series_start"}, sqlast.SelectItem{Expr: limited, Alias: "__ksql_series_count"},
		sqlast.SelectItem{Expr: &sqlast.Call{Name: "count", Args: []sqlast.Expr{&sqlast.Star{}}}, Alias: "__ksql_series_rows"})
	boundsQuery := &sqlast.Select{From: &sqlast.Subquery{Query: input.Query, Alias: inputAlias}, Projections: boundsItems, GroupBy: groupExprs}
	if len(groupExprs) == 0 {
		boundsQuery.GroupBy = []sqlast.Expr{&sqlast.Literal{Kind: sqlast.StringLiteral, Value: "series"}}
		if spec.Kind == "nonempty" && from.SQL != nil && to.SQL != nil {
			// kind=nonempty keeps one default-filled series for empty input.
			boundsQuery.GroupBy = nil
		}
	}

	// Aggregates per group and bin.
	column := func(alias, name string) sqlast.Expr { return &sqlast.Identifier{Parts: []string{alias, name}} }
	joinGroups := func(left, right string) sqlast.Expr {
		var condition sqlast.Expr = &sqlast.Literal{Kind: sqlast.BooleanLiteral, Value: "true"}
		for _, group := range groupSchema.Columns {
			equal := &sqlast.Binary{Left: column(left, group.Name), Operator: "IS", Right: column(right, group.Name)}
			condition = &sqlast.Binary{Left: condition, Operator: "AND", Right: equal}
		}
		return condition
	}
	expressions := make([]kql.Expression, len(spec.Items))
	for i, item := range spec.Items {
		expressions[i] = item.Aggregation
		if item.Name != "" {
			expressions[i] = &kql.NamedExpression{Name: item.Name, Value: item.Aggregation, Span: item.Span}
		}
	}
	aggregateItems, aggregateSchema := s.aggregateItems(expressions, input.Schema)
	if len(aggregateItems) != len(spec.Items) {
		return Relation{}
	}
	for _, aggregate := range aggregateSchema.Columns {
		if !isNumeric(aggregate.Type) && aggregate.Type != TypeUnknown && aggregate.Type != TypeTimespan {
			return fail("KQLB0417", "make-series aggregations must return numbers")
		}
	}
	binIndex := floorDiv(difference(axis.SQL, column(boundsAlias, "__ksql_series_start")), step.SQL)
	aggregateQuery := &sqlast.Select{
		From: &sqlast.Join{Left: &sqlast.Subquery{Query: input.Query, Alias: inputAlias}, Right: &sqlast.Subquery{Query: boundsQuery, Alias: boundsAlias}, Kind: "INNER", On: s.groupKeyJoin(groups, boundsAlias, groupSchema)},
		Where: &sqlast.Binary{
			Left:     &sqlast.Binary{Left: axis.SQL, Operator: ">=", Right: column(boundsAlias, "__ksql_series_start")},
			Operator: "AND",
			Right:    &sqlast.Binary{Left: binIndex, Operator: "<", Right: column(boundsAlias, "__ksql_series_count")},
		},
	}
	aggregateQuery.Projections = append(aggregateQuery.Projections, groups...)
	aggregateQuery.Projections = append(aggregateQuery.Projections, sqlast.SelectItem{Expr: binIndex, Alias: "__ksql_series_bin"})
	aggregateQuery.Projections = append(aggregateQuery.Projections, aggregateItems...)
	aggregateQuery.GroupBy = append(append([]sqlast.Expr{}, groupExprs...), binIndex)

	// Defaults, then the grid of every group and bin.
	defaults := make([]sqlast.Expr, len(spec.Items))
	for i, item := range spec.Items {
		defaults[i] = number("0")
		if item.Default != nil {
			value := bound(item.Default)
			if value.SQL == nil {
				return Relation{}
			}
			defaults[i] = value.SQL
		}
	}
	largest := &sqlast.ScalarSubquery{Query: &sqlast.Select{
		From:        &sqlast.Subquery{Query: boundsQuery, Alias: s.nextAlias()},
		Projections: []sqlast.SelectItem{{Expr: &sqlast.Call{Name: "max", Args: []sqlast.Expr{&sqlast.Identifier{Parts: []string{"__ksql_series_count"}}}}}},
	}}
	series := &sqlast.Series{Column: "__ksql_series_k", From: number("0"), To: &sqlast.Binary{Left: largest, Operator: "-", Right: number("1")}, Step: number("1"), Alias: seriesAlias}
	position := column(seriesAlias, "__ksql_series_k")
	grid := &sqlast.Join{
		Left:  &sqlast.Join{Left: &sqlast.Subquery{Query: boundsQuery, Alias: groupsAlias}, Right: series, Kind: "CROSS"},
		Right: &sqlast.Subquery{Query: aggregateQuery, Alias: aggregateAlias},
		Kind:  "LEFT",
		On: &sqlast.Binary{
			Left:     joinGroups(aggregateAlias, groupsAlias),
			Operator: "AND",
			Right:    &sqlast.Binary{Left: column(aggregateAlias, "__ksql_series_bin"), Operator: "=", Right: position},
		},
	}
	ordered := []sqlast.Order{{Expr: position, Direction: "asc"}}
	final := &sqlast.Select{From: grid, Where: &sqlast.Binary{Left: position, Operator: "<", Right: column(groupsAlias, "__ksql_series_count")}}
	output := Schema{}
	finalGroups := []sqlast.Expr{}
	for _, group := range groupSchema.Columns {
		final.Projections = append(final.Projections, sqlast.SelectItem{Expr: column(groupsAlias, group.Name), Alias: group.Name})
		finalGroups = append(finalGroups, column(groupsAlias, group.Name))
		output.Columns = append(output.Columns, group)
	}
	for i, aggregate := range aggregateSchema.Columns {
		value := &sqlast.Call{Name: "coalesce", Args: []sqlast.Expr{column(aggregateAlias, aggregate.Name), defaults[i]}}
		element := sqliteJSONElement(value, aggregate.Type)
		final.Projections = append(final.Projections, sqlast.SelectItem{Expr: &sqlast.OrderedAggregate{Name: "json_group_array", Args: []sqlast.Expr{element}, OrderBy: ordered}, Alias: aggregate.Name})
		output.Columns = append(output.Columns, Column{Name: aggregate.Name, Type: TypeDynamic})
	}
	axisName := inferredName(spec.Axis, axis.Name)
	if axisName == "" {
		axisName = "Axis"
	}
	axisValue := sqliteJSONElement(offset(column(groupsAlias, "__ksql_series_start"), position), axis.Type)
	final.Projections = append(final.Projections, sqlast.SelectItem{Expr: &sqlast.OrderedAggregate{Name: "json_group_array", Args: []sqlast.Expr{axisValue}, OrderBy: ordered}, Alias: axisName})
	output.Columns = append(output.Columns, Column{Name: axisName, Type: TypeDynamic})
	final.GroupBy = finalGroups
	if len(finalGroups) == 0 {
		final.GroupBy = []sqlast.Expr{&sqlast.Literal{Kind: sqlast.StringLiteral, Value: "series"}}
	}
	s.validateUnique(output.Columns, operator.Span, feature)
	if !s.checkProjectionWidth(len(output.Columns), operator.Span, feature) {
		return Relation{}
	}
	return Relation{Query: final, Schema: output}
}

// groupKeyJoin matches input rows to their group's bounds null-safely.
func (s *compileState) groupKeyJoin(groups []sqlast.SelectItem, alias string, schema Schema) sqlast.Expr {
	var condition sqlast.Expr = &sqlast.Literal{Kind: sqlast.BooleanLiteral, Value: "true"}
	for i, group := range groups {
		equal := &sqlast.Binary{Left: group.Expr, Operator: "IS", Right: &sqlast.Identifier{Parts: []string{alias, schema.Columns[i].Name}}}
		condition = &sqlast.Binary{Left: condition, Operator: "AND", Right: equal}
	}
	return condition
}
