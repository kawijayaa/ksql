package ksql

import (
	"fmt"
	"strings"

	"github.com/kawijayaa/ksql/dialect"
	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

func (s *compileState) mvExpand(input Relation, spec kql.MvExpandSpec, operator kql.Operator) Relation {
	return s.expandJSON(input, spec.Items, spec.ItemIndex, spec.Limit, spec.Kind, operator)
}

// applyRowColumn numbers mv-apply's input rows so subquery operators that
// combine rows (summarize, take, top, distinct, count) work per input row.
const applyRowColumn = "__ksql_apply_row"

func applyRowWise(kind string) bool {
	switch kind {
	case "where", "extend", "serialize", "mv-expand", "parse", "parse-where", "sort", "project-rename", "project-reorder", "project-away":
		return true
	}
	return false
}

// mvApply expands its arrays and runs the subquery on each input row's
// expansion. Row-wise operators apply directly; operators that combine rows
// are scoped to the input row through a hidden row number, and the input's
// other columns are joined back to their results.
func (s *compileState) mvApply(input Relation, spec kql.MvApplySpec, operator kql.Operator) Relation {
	rowWise := true
	for _, inner := range spec.Operators {
		rowWise = rowWise && applyRowWise(inner.Kind)
	}
	if rowWise {
		relation := s.expandJSON(input, spec.Items, spec.ItemIndex, spec.Limit, "bag", operator)
		for _, inner := range spec.Operators {
			if relation.Query == nil {
				return Relation{}
			}
			relation = s.operator(relation, inner)
		}
		return relation
	}
	feature := "operator.mv-apply"
	if input.Schema.Unknown {
		s.bindError("KQLB0339", operator.Span, feature, "mv-apply with summarize, take or top requires a bound input schema")
		return Relation{}
	}
	numberedSchema := input.Schema.Clone()
	numberedSchema.Columns = append(numberedSchema.Columns, Column{Name: applyRowColumn, Type: TypeLong})
	numbered := Relation{Query: &sqlast.Select{From: &sqlast.Subquery{Query: input.Query, Alias: s.nextAlias()}, Projections: []sqlast.SelectItem{
		{Expr: &sqlast.Star{}},
		{Expr: &sqlast.Window{Expr: &sqlast.Call{Name: "row_number"}}, Alias: applyRowColumn},
	}}, Schema: numberedSchema, rowDependencies: input.rowDependencies}
	relation := s.expandJSON(numbered, spec.Items, spec.ItemIndex, spec.Limit, "bag", operator)
	row := &kql.NameExpression{Name: applyRowColumn, Span: operator.Span}
	grouped := false
	for _, inner := range spec.Operators {
		if relation.Query == nil {
			return Relation{}
		}
		switch inner.Kind {
		case "project", "project-keep":
			body := inner.Body.(kql.ExpressionSpec)
			body.Expressions = append(append([]kql.Expression{}, body.Expressions...), row)
			inner.Body = body
			relation = s.operator(relation, inner)
		case "summarize":
			body := inner.Body.(kql.SummarizeSpec)
			body.By = append([]kql.Expression{row}, body.By...)
			inner.Body = body
			relation, grouped = s.operator(relation, inner), true
		case "count":
			count := &kql.CallExpression{Function: &kql.NameExpression{Name: "count"}, Span: inner.Span}
			inner = kql.Operator{Kind: "summarize", Body: kql.SummarizeSpec{Aggregates: []kql.Expression{&kql.NamedExpression{Name: "Count", Value: count}}, By: []kql.Expression{row}}, Span: inner.Span}
			relation, grouped = s.operator(relation, inner), true
		case "distinct":
			body := inner.Body.(kql.ExpressionSpec)
			body.Expressions = append([]kql.Expression{row}, body.Expressions...)
			inner.Body = body
			relation, grouped = s.operator(relation, inner), true
		case "take", "limit":
			body := inner.Body.(kql.ExpressionSpec)
			relation = s.applyRowsPerInput(relation, nil, body.Expressions[0], inner)
		case "top":
			body := inner.Body.(kql.TopSpec)
			relation = s.applyRowsPerInput(relation, []kql.OrderedExpression{body.By}, body.Count, inner)
		default:
			if !applyRowWise(inner.Kind) {
				s.error("KQLL0334", inner.Span, "operator.mv-apply."+inner.Kind, "mv-apply does not support "+inner.Kind+" in its subquery")
				return Relation{}
			}
			relation = s.operator(relation, inner)
		}
	}
	if relation.Query == nil {
		return Relation{}
	}
	if !grouped {
		return dropColumn(relation, applyRowColumn, s.nextAlias())
	}
	// Join the input's columns (except the expanded names and names the
	// subquery produces) back to the per-row results.
	expanded := map[string]bool{}
	for _, item := range spec.Items {
		expanded[strings.ToLower(item.Name)] = true
	}
	left, right := s.nextAlias(), s.nextAlias()
	var projections []sqlast.SelectItem
	schema := Schema{}
	for _, column := range input.Schema.Columns {
		if _, _, produced, _ := relation.Schema.Lookup(column.Name); produced || expanded[strings.ToLower(column.Name)] {
			continue
		}
		projections = append(projections, sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{left, column.Name}}, Alias: column.Name})
		schema.Columns = append(schema.Columns, column)
	}
	for _, column := range relation.Schema.Columns {
		if column.Name == applyRowColumn {
			continue
		}
		projections = append(projections, sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{right, column.Name}}, Alias: column.Name})
		schema.Columns = append(schema.Columns, column)
	}
	join := &sqlast.Join{
		Left:  &sqlast.Subquery{Query: numbered.Query, Alias: left},
		Right: &sqlast.Subquery{Query: relation.Query, Alias: right},
		Kind:  "INNER",
		On:    &sqlast.Binary{Left: &sqlast.Identifier{Parts: []string{left, applyRowColumn}}, Operator: "=", Right: &sqlast.Identifier{Parts: []string{right, applyRowColumn}}},
	}
	return Relation{Query: &sqlast.Select{From: join, Projections: projections}, Schema: schema}
}

// applyRowsPerInput keeps the first count rows of each input row's group,
// in the given order (take has none).
func (s *compileState) applyRowsPerInput(relation Relation, orders []kql.OrderedExpression, count kql.Expression, operator kql.Operator) Relation {
	if !s.validateLimit(count, operator) {
		return Relation{}
	}
	previous := s.input
	s.input = relation.Schema
	defer func() { s.input = previous }()
	var sqlOrders []sqlast.Order
	for _, order := range orders {
		bound := s.expression(order.Expression)
		if bound.SQL == nil {
			return Relation{}
		}
		sqlOrders = append(sqlOrders, sqlast.Order{Expr: bound.SQL, Direction: order.Direction, Nulls: order.Nulls})
	}
	limit := s.expression(count)
	if limit.SQL == nil {
		return Relation{}
	}
	const rank = "__ksql_apply_rank"
	ranked := &sqlast.Select{From: &sqlast.Subquery{Query: relation.Query, Alias: s.nextAlias()}, Projections: []sqlast.SelectItem{
		{Expr: &sqlast.Star{}},
		{Expr: &sqlast.Window{Expr: &sqlast.Call{Name: "row_number"}, PartitionBy: []sqlast.Expr{&sqlast.Identifier{Parts: []string{applyRowColumn}}}, OrderBy: sqlOrders}, Alias: rank},
	}}
	projections := make([]sqlast.SelectItem, len(relation.Schema.Columns))
	for i, column := range relation.Schema.Columns {
		projections[i] = sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{column.Name}}, Alias: column.Name}
	}
	where := &sqlast.Binary{Left: &sqlast.Identifier{Parts: []string{rank}}, Operator: "<=", Right: limit.SQL}
	return Relation{Query: &sqlast.Select{From: &sqlast.Subquery{Query: ranked, Alias: s.nextAlias()}, Where: where, Projections: projections}, Schema: relation.Schema.Clone()}
}

func dropColumn(relation Relation, name, alias string) Relation {
	schema := Schema{}
	var projections []sqlast.SelectItem
	for _, column := range relation.Schema.Columns {
		if column.Name == name {
			continue
		}
		schema.Columns = append(schema.Columns, column)
		projections = append(projections, sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{column.Name}}, Alias: column.Name})
	}
	return Relation{Query: &sqlast.Select{From: &sqlast.Subquery{Query: relation.Query, Alias: alias}, Projections: projections}, Schema: schema}
}

func (s *compileState) expandJSON(input Relation, items []kql.MvExpansion, itemIndex string, limit kql.Expression, expansionKind string, operator kql.Operator) Relation {
	if !supportsJSONEach(s.compiler.dialect) {
		s.error("KQLL0330", operator.Span, "operator."+operator.Kind, "dialect "+s.compiler.dialect.Name()+" has no JSON array expansion")
		return Relation{}
	}
	if s.compiler.limits.MaxExpansionItems > 0 && len(items) > s.compiler.limits.MaxExpansionItems {
		s.bindError("KQLB0337", operator.Span, "operator."+operator.Kind, fmt.Sprintf("expansion exceeds MaxExpansionItems (%d)", s.compiler.limits.MaxExpansionItems))
		return Relation{}
	}
	if len(items) == 0 {
		s.error("KQLL0331", operator.Span, "operator."+operator.Kind, operator.Kind+" requires an array expression")
		return Relation{}
	}
	if len(items) > 1 {
		return s.expandArrays(input, items, itemIndex, limit, expansionKind, operator)
	}
	item := items[0]
	sourceName := ""
	if name, ok := item.Expression.(*kql.NameExpression); ok {
		sourceName = name.Name
	}
	if item.Name == "" {
		s.error("KQLL0332", item.Span, "operator."+operator.Kind, operator.Kind+" requires an explicit output name for non-column expressions")
		return Relation{}
	}
	if input.Schema.Unknown && strings.EqualFold(item.Name, sourceName) {
		s.error("KQLL0333", item.Span, "operator."+operator.Kind, "column replacement requires a bound input schema")
		return Relation{}
	}

	inputAlias, expansionAlias := s.nextAlias(), s.nextAlias()
	previousQualifier := s.qualifier
	s.qualifier = inputAlias
	previousInput := s.input
	s.input = input.Schema
	value := s.expression(item.Expression)
	s.input = previousInput
	s.qualifier = previousQualifier
	if value.Type != TypeDynamic && value.Type != TypeUnknown {
		s.bindError("KQLB0338", item.Span, "operator."+operator.Kind, "expanded expression must be dynamic")
		return Relation{}
	}
	expansion := &sqlast.JSONEach{Value: value.SQL, Alias: expansionAlias}
	from := &sqlast.Join{
		Left:  &sqlast.Subquery{Query: input.Query, Alias: inputAlias},
		Right: expansion,
		Kind:  "CROSS",
	}
	element := sqlast.Expr(&sqlast.Identifier{Parts: []string{expansionAlias, "value"}})
	expansionType := sqlast.Expr(&sqlast.Identifier{Parts: []string{expansionAlias, "type"}})
	isNestedJSON := &sqlast.Binary{
		Left:     &sqlast.Binary{Left: expansionType, Operator: "=", Right: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: "object"}},
		Operator: "OR",
		Right:    &sqlast.Binary{Left: expansionType, Operator: "=", Right: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: "array"}},
	}
	jsonValue := sqlast.Expr(&sqlast.Case{Branches: []sqlast.When{{Condition: isNestedJSON, Result: &sqlast.Call{Name: "json", Args: []sqlast.Expr{element}}}}, Else: element})
	isObject := &sqlast.Binary{Left: &sqlast.Call{Name: "json_type", Args: []sqlast.Expr{value.SQL}}, Operator: "=", Right: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: "object"}}
	objectElement := sqlast.Expr(&sqlast.Call{Name: "json_object", Args: []sqlast.Expr{&sqlast.Identifier{Parts: []string{expansionAlias, "key"}}, jsonValue}})
	if expansionKind == "array" {
		objectElement = &sqlast.Call{Name: "json_array", Args: []sqlast.Expr{&sqlast.Identifier{Parts: []string{expansionAlias, "key"}}, jsonValue}}
	}
	element = &sqlast.Case{Branches: []sqlast.When{{Condition: isObject, Result: objectElement}}, Else: element}
	if item.Type != "" {
		types := map[string]string{
			"long": "BIGINT", "int": "INTEGER", "real": "DOUBLE PRECISION", "double": "DOUBLE PRECISION",
			"decimal": "DECIMAL", "string": "VARCHAR", "bool": "BOOLEAN", "boolean": "BOOLEAN",
			"datetime": "TIMESTAMP", "guid": "UUID",
		}
		target, ok := types[item.Type]
		if !ok {
			s.error("KQLL0335", item.Span, "operator."+operator.Kind, "unsupported expansion type "+item.Type)
			return Relation{}
		}
		s.warn("KQLL0336", item.Span, "operator."+operator.Kind, "SQLite casts invalid expanded values instead of returning null")
		element = &sqlast.Cast{Expr: element, Type: target}
	}
	output := input.Schema.Clone()
	columnType := TypeDynamic
	if item.Type != "" {
		columnType = parseScalarType(item.Type)
	}
	_, replaceAt, replacing, _ := output.Lookup(item.Name)
	if replacing {
		output.Columns[replaceAt] = Column{Name: output.Columns[replaceAt].Name, Type: columnType}
	} else {
		output.Columns = append(output.Columns, Column{Name: item.Name, Type: columnType})
	}
	projections := make([]sqlast.SelectItem, 0, len(output.Columns)+1)
	if output.Unknown {
		projections = append(projections, sqlast.SelectItem{Expr: &sqlast.QualifiedStar{Parts: []string{inputAlias}}})
		projections = append(projections, sqlast.SelectItem{Expr: element, Alias: item.Name})
	} else {
		for _, column := range input.Schema.Columns {
			expr := sqlast.Expr(&sqlast.Identifier{Parts: []string{inputAlias, column.Name}})
			if strings.EqualFold(column.Name, item.Name) {
				expr = element
			}
			projections = append(projections, sqlast.SelectItem{Expr: expr, Alias: column.Name})
		}
		if !replacing {
			projections = append(projections, sqlast.SelectItem{Expr: element, Alias: item.Name})
		}
	}
	if itemIndex != "" {
		projections = append(projections, sqlast.SelectItem{
			Expr: &sqlast.Case{Branches: []sqlast.When{{
				Condition: &sqlast.Binary{Left: &sqlast.Call{Name: "json_type", Args: []sqlast.Expr{value.SQL}}, Operator: "=", Right: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: "array"}},
				Result:    &sqlast.Cast{Expr: &sqlast.Identifier{Parts: []string{expansionAlias, "key"}}, Type: "INTEGER"},
			}}, Else: &sqlast.Literal{Kind: sqlast.NullLiteral}},
			Alias: itemIndex,
		})
		output.Columns = append(output.Columns, Column{Name: itemIndex, Type: TypeLong})
	}
	query := &sqlast.Select{From: from, Projections: projections}
	if limit != nil {
		query.Where = &sqlast.Binary{
			Left:     &sqlast.Cast{Expr: &sqlast.Identifier{Parts: []string{expansionAlias, "key"}}, Type: "INTEGER"},
			Operator: "<",
			Right:    s.expression(limit).SQL,
		}
	}
	return Relation{Query: query, Schema: output}
}

func supportsJSONEach(target dialect.Dialect) bool {
	capability, ok := target.(interface {
		JSONEach(string, string) (string, bool)
	})
	if !ok {
		return false
	}
	_, ok = capability.JSONEach("", "")
	return ok
}

// expandArrays expands several arrays in lockstep: row i holds the i-th
// element of each array, and shorter arrays (or non-array values) give null.
// The longest array drives the expansion.
func (s *compileState) expandArrays(input Relation, items []kql.MvExpansion, itemIndex string, limit kql.Expression, expansionKind string, operator kql.Operator) Relation {
	feature := "operator." + operator.Kind
	if input.Schema.Unknown {
		s.error("KQLL0333", operator.Span, feature, "multi-column expansion requires a bound input schema")
		return Relation{}
	}
	inputAlias, driverAlias := s.nextAlias(), s.nextAlias()
	previousQualifier, previousInput := s.qualifier, s.input
	s.qualifier, s.input = inputAlias, input.Schema
	values := make([]boundExpr, len(items))
	seen := map[string]bool{}
	for i, item := range items {
		if item.Name == "" {
			s.qualifier, s.input = previousQualifier, previousInput
			s.error("KQLL0332", item.Span, feature, operator.Kind+" requires an explicit output name for non-column expressions")
			return Relation{}
		}
		if seen[strings.ToLower(item.Name)] {
			s.qualifier, s.input = previousQualifier, previousInput
			s.bindError("KQLB0406", item.Span, feature, "duplicate expansion column "+item.Name)
			return Relation{}
		}
		seen[strings.ToLower(item.Name)] = true
		values[i] = s.expression(item.Expression)
		if values[i].SQL == nil {
			s.qualifier, s.input = previousQualifier, previousInput
			return Relation{}
		}
		if values[i].Type != TypeDynamic && values[i].Type != TypeUnknown {
			s.qualifier, s.input = previousQualifier, previousInput
			s.bindError("KQLB0338", item.Span, feature, "expanded expression must be dynamic")
			return Relation{}
		}
		values[i].SQL = &sqlast.ExpansionArray{Value: values[i].SQL, Pairs: expansionKind == "array"}
	}
	s.qualifier, s.input = previousQualifier, previousInput
	length := func(value sqlast.Expr) sqlast.Expr {
		array := &sqlast.Case{Branches: []sqlast.When{{
			Condition: &sqlast.Call{Name: "json_valid", Args: []sqlast.Expr{value}},
			Result: &sqlast.Case{Branches: []sqlast.When{{
				Condition: &sqlast.Binary{Left: &sqlast.Call{Name: "json_type", Args: []sqlast.Expr{value}}, Operator: "=", Right: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: "array"}},
				Result:    value,
			}}},
		}}}
		return &sqlast.Call{Name: "coalesce", Args: []sqlast.Expr{&sqlast.Call{Name: "json_array_length", Args: []sqlast.Expr{array}}, &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "0"}}}
	}
	// The driver is the first array whose length is at least every other's.
	driver := &sqlast.Case{}
	for i, value := range values {
		var condition sqlast.Expr
		for j, other := range values {
			if i == j {
				continue
			}
			test := &sqlast.Binary{Left: length(value.SQL), Operator: ">=", Right: length(other.SQL)}
			if condition == nil {
				condition = test
			} else {
				condition = &sqlast.Binary{Left: condition, Operator: "AND", Right: test}
			}
		}
		// Ties pick the first value, so all-empty arrays expand to no rows
		// and null values to one null row, as for single-column expansion.
		driver.Branches = append(driver.Branches, sqlast.When{Condition: condition, Result: value.SQL})
	}
	position := &sqlast.Identifier{Parts: []string{driverAlias, "key"}}
	from := &sqlast.Join{
		Left:  &sqlast.Subquery{Query: input.Query, Alias: inputAlias},
		Right: &sqlast.JSONEach{Value: driver, Alias: driverAlias},
		Kind:  "CROSS",
	}
	output := input.Schema.Clone()
	elements := map[string]sqlast.Expr{}
	for i, item := range items {
		element := sqlast.Expr(&sqlast.Index{Value: values[i].SQL, Index: position, Numeric: true})
		columnType := TypeDynamic
		if item.Type != "" {
			columnType = parseScalarType(item.Type)
			if columnType == TypeUnknown {
				s.error("KQLL0335", item.Span, feature, "unsupported expansion type "+item.Type)
				return Relation{}
			}
			element = &sqlast.Cast{Expr: element, Type: map[ScalarType]string{TypeLong: "BIGINT", TypeInt: "INTEGER", TypeReal: "DOUBLE PRECISION", TypeString: "VARCHAR", TypeBool: "BOOLEAN", TypeDateTime: "TIMESTAMP", TypeTimespan: "BIGINT", TypeDynamic: "VARCHAR"}[columnType]}
		}
		elements[strings.ToLower(item.Name)] = element
		if _, at, replacing, _ := output.Lookup(item.Name); replacing {
			output.Columns[at] = Column{Name: output.Columns[at].Name, Type: columnType}
		} else {
			output.Columns = append(output.Columns, Column{Name: item.Name, Type: columnType})
		}
	}
	projections := make([]sqlast.SelectItem, 0, len(output.Columns)+1)
	for _, column := range output.Columns {
		expr := sqlast.Expr(&sqlast.Identifier{Parts: []string{inputAlias, column.Name}})
		if element, ok := elements[strings.ToLower(column.Name)]; ok {
			expr = element
		}
		projections = append(projections, sqlast.SelectItem{Expr: expr, Alias: column.Name})
	}
	if itemIndex != "" {
		projections = append(projections, sqlast.SelectItem{Expr: &sqlast.Cast{Expr: position, Type: "INTEGER"}, Alias: itemIndex})
		output.Columns = append(output.Columns, Column{Name: itemIndex, Type: TypeLong})
	}
	if !s.checkProjectionWidth(len(projections), operator.Span, feature) {
		return Relation{}
	}
	query := &sqlast.Select{From: from, Projections: projections}
	if limit != nil {
		query.Where = &sqlast.Binary{Left: &sqlast.Cast{Expr: position, Type: "INTEGER"}, Operator: "<", Right: s.expression(limit).SQL}
	}
	return Relation{Query: query, Schema: output}
}
