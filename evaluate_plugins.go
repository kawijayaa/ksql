package ksql

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// maxPivotColumns bounds the columns pivot creates from data values.
const maxPivotColumns = 256

// narrow rotates each input row into (Row, Column, Value) rows, Row being
// the zero-based input row number and Value the text of the cell.
func (s *compileState) narrow(input Relation, spec kql.EvaluateSpec, feature string) Relation {
	if len(spec.Args) != 0 {
		s.bindError("KQLB0417", spec.Span, feature, "narrow takes no arguments")
		return Relation{}
	}
	if input.Schema.Unknown || len(input.Schema.Columns) == 0 {
		s.bindError("KQLB0417", spec.Span, feature, "narrow requires a known input schema")
		return Relation{}
	}
	numbered := &sqlast.Select{From: &sqlast.Subquery{Query: input.Query, Alias: s.nextAlias()}, Projections: []sqlast.SelectItem{
		{Expr: &sqlast.Star{}},
		{Expr: &sqlast.Binary{Left: &sqlast.Window{Expr: &sqlast.Call{Name: "row_number"}}, Operator: "-", Right: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "1"}}, Alias: "__ksql_narrow_row"},
	}}
	queries := make([]sqlast.Query, 0, len(input.Schema.Columns))
	for _, column := range input.Schema.Columns {
		value := textOf(&sqlast.Identifier{Parts: []string{column.Name}}, column.Type)
		queries = append(queries, &sqlast.Select{From: &sqlast.Subquery{Query: numbered, Alias: s.nextAlias()}, Projections: []sqlast.SelectItem{
			{Expr: &sqlast.Identifier{Parts: []string{"__ksql_narrow_row"}}, Alias: "Row"},
			{Expr: s.bind(column.Name), Alias: "Column"},
			{Expr: value, Alias: "Value"},
		}})
	}
	schema := Schema{Columns: []Column{{Name: "Row", Type: TypeLong}, {Name: "Column", Type: TypeString}, {Name: "Value", Type: TypeString}}}
	if len(queries) == 1 {
		return Relation{Query: queries[0], Schema: schema}
	}
	return Relation{Query: &sqlast.Union{All: true, Queries: queries}, Schema: schema}
}

// pivotLiteral writes a discovered value as a KQL literal.
func pivotLiteral(value any) (kql.Expression, string, bool) {
	switch v := value.(type) {
	case string:
		return searchString(v), v, true
	case int64:
		text := strconv.FormatInt(v, 10)
		return &kql.LiteralExpression{Text: text, Kind: kql.NumberToken}, text, true
	case float64:
		text := strconv.FormatFloat(v, 'g', -1, 64)
		if !strings.ContainsAny(text, ".eE") {
			text += ".0"
		}
		return &kql.LiteralExpression{Text: text, Kind: kql.NumberToken}, strconv.FormatFloat(v, 'g', -1, 64), true
	case bool:
		return &kql.LiteralExpression{Text: strconv.FormatBool(v), Kind: kql.IdentifierToken}, strconv.FormatBool(v), true
	}
	return nil, "", false
}

// pivot turns each distinct value of the pivot column into a column
// aggregating its rows, grouped by the remaining (or given) columns. With
// an output schema the pivot values are the schema's columns; otherwise
// they are discovered with the application's ValuesResolver.
func (s *compileState) pivot(input Relation, spec kql.EvaluateSpec, operator kql.Operator, feature string) Relation {
	fail := func(message string) Relation {
		s.bindError("KQLB0417", spec.Span, feature, message)
		return Relation{}
	}
	if input.Schema.Unknown {
		return fail("pivot requires a known input schema")
	}
	if len(spec.Args) == 0 {
		return fail("pivot requires a pivot column")
	}
	pivotName, ok := spec.Args[0].(*kql.NameExpression)
	if !ok {
		return fail("pivot's first argument must be a column")
	}
	pivotColumn, _, found, _ := input.Schema.Lookup(unquoteKQL(pivotName.Name))
	if !found {
		return fail("unknown pivot column " + pivotName.Name)
	}
	aggregate := &kql.CallExpression{Function: &kql.NameExpression{Name: "count"}}
	rest := spec.Args[1:]
	if len(rest) > 0 {
		if call, ok := rest[0].(*kql.CallExpression); ok {
			aggregate, rest = call, rest[1:]
		}
	}
	aggregateName, _ := aggregate.Function.(*kql.NameExpression)
	if aggregateName == nil {
		return fail("pivot's aggregation must be a function call")
	}
	conditional := map[string]string{
		"count": "countif", "sum": "sumif", "min": "minif", "max": "maxif", "avg": "avgif", "dcount": "dcountif",
		"take_any": "take_anyif", "any": "take_anyif", "stdev": "stdevif", "variance": "varianceif",
		"make_list": "make_list_if", "make_set": "make_set_if", "make_bag": "make_bag_if",
	}[strings.ToLower(aggregateName.Name)]
	if conditional == "" {
		return fail("pivot supports count, sum, min, max, avg, dcount, take_any, stdev, variance, make_list, make_set and make_bag")
	}
	// Group by the given columns, or every column except the pivot column and
	// the aggregated ones.
	excluded := map[string]bool{strings.ToLower(pivotColumn.Name): true}
	var aggregated []string
	for _, arg := range aggregate.Args {
		referencedColumns(arg, &aggregated, map[string]bool{})
	}
	for _, name := range aggregated {
		excluded[strings.ToLower(name)] = true
	}
	var by []kql.Expression
	if len(rest) > 0 {
		by = rest
	} else {
		for _, column := range input.Schema.Columns {
			if !excluded[strings.ToLower(column.Name)] {
				by = append(by, &kql.NameExpression{Name: searchName(column.Name)})
			}
		}
	}
	var values []any
	if spec.HasSchema {
		// The output schema lists the pivot values after the group columns.
		if len(spec.Schema) < len(by) {
			return fail("pivot output schema must list the group columns")
		}
		for _, column := range spec.Schema[len(by):] {
			values = append(values, unquoteKQL(column.Name))
		}
	} else {
		var ok bool
		if values, ok = s.discoverValues(input, pivotColumn.Name, maxPivotColumns, spec.Span, feature); !ok {
			return Relation{}
		}
	}
	column := &kql.NameExpression{Name: searchName(pivotColumn.Name)}
	var aggregates []kql.Expression
	seen := map[string]bool{}
	for _, value := range values {
		if value == nil {
			continue
		}
		literal, name, ok := pivotLiteral(value)
		if !ok || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		var predicate kql.Expression = &kql.BinaryExpression{Left: column, Operator: "==", Right: literal}
		if spec.HasSchema && pivotColumn.Type != TypeString {
			// Schema column names are text; compare the pivot column as text.
			text := &kql.CallExpression{Function: &kql.NameExpression{Name: "tostring"}, Args: []kql.Expression{column}}
			predicate = &kql.BinaryExpression{Left: text, Operator: "==", Right: searchString(name)}
		}
		args := append(append([]kql.Expression{}, aggregate.Args...), predicate)
		call := &kql.CallExpression{Function: &kql.NameExpression{Name: conditional}, Args: args, Span: spec.Span}
		aggregates = append(aggregates, &kql.NamedExpression{Name: name, Value: call, Span: spec.Span})
	}
	if len(aggregates) == 0 {
		return fail("pivot found no values to rotate")
	}
	summarize := kql.Operator{Kind: "summarize", Body: kql.SummarizeSpec{Aggregates: aggregates, By: by}, Span: operator.Span}
	return s.operator(input, summarize)
}

// ipLookup implements ipv4_lookup and ipv6_lookup: a lookup join whose key
// matches when the source address is in the lookup row's range, plus equal
// extra keys; return_unmatched keeps unmatched source rows.
func (s *compileState) ipLookup(input Relation, spec kql.EvaluateSpec, feature, matcher string) Relation {
	fail := func(message string) Relation {
		s.bindError("KQLB0417", spec.Span, feature, message)
		return Relation{}
	}
	if input.Schema.Unknown {
		return fail(spec.Plugin + " requires a known input schema")
	}
	args := spec.Args
	unmatched := false
	if n := len(args); n > 0 {
		if named, ok := args[n-1].(*kql.NamedExpression); ok && strings.EqualFold(named.Name, "return_unmatched") {
			literal, ok := named.Value.(*kql.LiteralExpression)
			if !ok || (!strings.EqualFold(literal.Text, "true") && !strings.EqualFold(literal.Text, "false")) {
				return fail("return_unmatched must be true or false")
			}
			unmatched, args = strings.EqualFold(literal.Text, "true"), args[:n-1]
		} else if literal, ok := args[n-1].(*kql.LiteralExpression); ok && (strings.EqualFold(literal.Text, "true") || strings.EqualFold(literal.Text, "false")) {
			unmatched, args = strings.EqualFold(literal.Text, "true"), args[:n-1]
		}
	}
	if len(args) < 3 {
		return fail(spec.Plugin + " requires a lookup table, a source key and a lookup key")
	}
	lookup, ok := s.tabularLetValue(args[0])
	if !ok {
		if name, isName := args[0].(*kql.NameExpression); isName {
			lookup = s.source(kql.Source{Kind: "table", Name: name.Name, Span: name.Span})
		}
	}
	if lookup.Query == nil {
		if ok {
			return Relation{}
		}
		return fail(spec.Plugin + " lookup table must be a table or tabular expression")
	}
	if lookup.Schema.Unknown {
		return fail(spec.Plugin + " requires a lookup table with a known schema")
	}
	columnName := func(expression kql.Expression, schema Schema, side string) (Column, bool) {
		name, ok := expression.(*kql.NameExpression)
		if !ok {
			return Column{}, false
		}
		column, _, found, _ := schema.Lookup(unquoteKQL(name.Name))
		if !found {
			s.bindError("KQLB0401", expression.NodeSpan(), feature, "unknown "+side+" column "+name.Name)
		}
		return column, found
	}
	sourceKey, okSource := columnName(args[1], input.Schema, "source")
	lookupKey, okLookup := columnName(args[2], lookup.Schema, "lookup")
	if !okSource || !okLookup {
		return Relation{}
	}
	var extra []Column
	extraNames := map[string]bool{}
	for _, arg := range args[3:] {
		left, okLeft := columnName(arg, input.Schema, "source")
		_, okRight := columnName(arg, lookup.Schema, "lookup")
		if !okLeft || !okRight {
			return Relation{}
		}
		extra = append(extra, left)
		extraNames[strings.ToLower(left.Name)] = true
	}
	// Prefix the lookup's columns so both sides bind unambiguously.
	const prefix = "__ksql_lookup_"
	leftAlias, rightAlias := s.nextAlias(), s.nextAlias()
	renamed := &sqlast.Select{From: &sqlast.Subquery{Query: lookup.Query, Alias: s.nextAlias()}}
	combined := input.Schema.Clone()
	for _, column := range lookup.Schema.Columns {
		renamed.Projections = append(renamed.Projections, sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{column.Name}}, Alias: prefix + column.Name})
		combined.Columns = append(combined.Columns, Column{Name: prefix + column.Name, Type: column.Type})
	}
	var predicate kql.Expression = &kql.CallExpression{Function: &kql.NameExpression{Name: matcher}, Args: []kql.Expression{
		&kql.NameExpression{Name: searchName(sourceKey.Name)}, &kql.NameExpression{Name: searchName(prefix + lookupKey.Name)},
	}}
	for _, column := range extra {
		predicate = &kql.BinaryExpression{Left: predicate, Operator: "and", Right: &kql.BinaryExpression{
			Left: &kql.NameExpression{Name: searchName(column.Name)}, Operator: "==", Right: &kql.NameExpression{Name: searchName(prefix + column.Name)},
		}}
	}
	previous := s.input
	s.input = combined
	bound := s.expression(predicate)
	s.input = previous
	if bound.SQL == nil {
		return Relation{}
	}
	if bound.Type != TypeBool && bound.Type != TypeUnknown {
		return fail(matcher + " must return a Boolean")
	}
	kind := "INNER"
	if unmatched {
		kind = "LEFT"
	}
	join := &sqlast.Join{
		Left:  &sqlast.Subquery{Query: input.Query, Alias: leftAlias},
		Right: &sqlast.Subquery{Query: renamed, Alias: rightAlias},
		Kind:  kind,
		On:    bound.SQL,
	}
	schema := input.Schema.Clone()
	var projections []sqlast.SelectItem
	for _, column := range input.Schema.Columns {
		projections = append(projections, sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{leftAlias, column.Name}}, Alias: column.Name})
	}
	for _, column := range lookup.Schema.Columns {
		if extraNames[strings.ToLower(column.Name)] {
			continue
		}
		name := column.Name
		if containsColumn(schema.Columns, name) {
			name = fmt.Sprintf("%s1", name)
		}
		projections = append(projections, sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{rightAlias, prefix + column.Name}}, Alias: name})
		schema.Columns = append(schema.Columns, Column{Name: name, Type: column.Type})
	}
	return Relation{Query: &sqlast.Select{From: join, Projections: projections}, Schema: schema}
}
