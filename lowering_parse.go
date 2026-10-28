package ksql

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

const maxParseColumns = 128

// parseConversions maps parse column types to the KQL conversion that binds
// the captured text, so application function overrides apply unchanged.
var parseConversions = map[string]string{
	"int": "toint", "int32": "toint", "long": "tolong", "int64": "tolong",
	"real": "toreal", "double": "toreal", "decimal": "todecimal",
	"datetime": "todatetime", "date": "todatetime", "timespan": "totimespan", "time": "totimespan",
	"bool": "tobool", "boolean": "tobool", "guid": "toguid", "uniqueid": "toguid", "dynamic": "todynamic",
}

const (
	parseIntegerPattern = `(\-?\d+)`
	parseRealPattern    = `(\-?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][\+\-]?\d+)?)`
)

type parseColumn struct {
	name       string
	conversion string
	group      int
	span       kql.Span
}

func supportsRegexCaptures(target any) bool {
	capability, ok := target.(interface {
		RegexCaptures(value, pattern string) (string, bool)
	})
	if !ok {
		return false
	}
	_, ok = capability.RegexCaptures("", "")
	return ok
}

// parseRegex translates a parse pattern to one anchored RE2 expression and
// records the capture group belonging to each extracted column. Literal
// constants are quoted except in regex mode, whose own groups are counted.
func (s *compileState) parseRegex(spec kql.ParseSpec, feature string) (string, []parseColumn, bool) {
	var pattern strings.Builder
	if spec.Flags != "" {
		pattern.WriteString("(?" + spec.Flags + ")")
	}
	pattern.WriteString(`\A`)
	group := 0
	columns := make([]parseColumn, 0)
	seen := map[string]struct{}{}
	for i, item := range spec.Pattern {
		switch item.Kind {
		case kql.ParseWildcard:
			if i > 0 && spec.Pattern[i-1].Kind == kql.ParseColumn && parseConversions[strings.ToLower(spec.Pattern[i-1].Type)] == "" {
				s.bindError("KQLB0371", item.Span, feature, "* cannot follow a string column in a parse pattern")
				return "", nil, false
			}
			pattern.WriteString(`.*?`)
		case kql.ParseConstant:
			if spec.Kind != "regex" {
				pattern.WriteString(regexp.QuoteMeta(item.Text))
				continue
			}
			parsed, err := syntax.Parse(item.Text, syntax.Perl)
			if err != nil {
				s.bindError("KQLB0372", item.Span, feature, "invalid parse regular expression: "+err.Error())
				return "", nil, false
			}
			group += parsed.MaxCap()
			pattern.WriteString("(?:" + item.Text + ")")
		case kql.ParseColumn:
			name := unquoteKQL(item.Name)
			typeName := strings.ToLower(item.Type)
			conversion, typed := parseConversions[typeName]
			if item.Type != "" && !typed && typeName != "string" {
				s.bindError("KQLB0373", item.Span, feature, "unsupported parse column type "+item.Type)
				return "", nil, false
			}
			key := strings.ToLower(name)
			if _, duplicate := seen[key]; duplicate {
				s.bindError("KQLB0374", item.Span, feature, "duplicate parse column "+name)
				return "", nil, false
			}
			seen[key] = struct{}{}
			group++
			columns = append(columns, parseColumn{name: name, conversion: conversion, group: group, span: item.Span})
			// Relaxed numeric columns prefer a number but fall back to any
			// text, which then fails conversion alone instead of the match.
			numeric := ""
			switch conversion {
			case "toint", "tolong":
				numeric = parseIntegerPattern
			case "toreal", "todecimal":
				numeric = parseRealPattern
			}
			switch {
			case numeric != "" && spec.Kind == "relaxed":
				pattern.WriteString("(" + numeric[1:len(numeric)-1] + "|.*?)")
			case numeric != "":
				pattern.WriteString(numeric)
			case spec.Kind == "regex":
				pattern.WriteString(`(.*)`)
			default:
				pattern.WriteString(`(.*?)`)
			}
		}
	}
	pattern.WriteString(`\z`)
	if len(columns) == 0 {
		s.bindError("KQLB0375", spec.Source.NodeSpan(), feature, "parse pattern requires at least one column")
		return "", nil, false
	}
	if len(columns) > maxParseColumns {
		s.bindError("KQLB0375", spec.Source.NodeSpan(), feature, fmt.Sprintf("parse pattern exceeds %d columns", maxParseColumns))
		return "", nil, false
	}
	if limit := s.compiler.limits.MaxParseCaptures; limit > 0 && group > limit {
		s.bindError("KQLB0375", spec.Source.NodeSpan(), feature, fmt.Sprintf("parse pattern exceeds MaxParseCaptures (%d)", limit))
		return "", nil, false
	}
	result := pattern.String()
	if limit := s.compiler.limits.MaxRegexBytes; limit > 0 && len(result) > limit {
		s.bindError("KQLB0376", spec.Source.NodeSpan(), feature, fmt.Sprintf("parse pattern exceeds MaxRegexBytes (%d)", limit))
		return "", nil, false
	}
	if _, err := regexp.Compile(result); err != nil {
		s.bindError("KQLB0372", spec.Source.NodeSpan(), feature, "invalid parse regular expression: "+err.Error())
		return "", nil, false
	}
	return result, columns, true
}

func (s *compileState) parseOperator(input Relation, operator kql.Operator) Relation {
	feature := "operator." + operator.Kind
	spec := operator.Body.(kql.ParseSpec)
	if spec.Source == nil {
		return Relation{}
	}
	if operator.Kind == "parse-where" && spec.Kind == "relaxed" {
		s.bindError("KQLB0370", operator.Span, feature, "parse-where supports kind=simple or kind=regex")
		return Relation{}
	}
	if spec.Flags != "" {
		if spec.Kind != "regex" {
			s.bindError("KQLB0370", operator.Span, feature, "parse flags require kind=regex")
			return Relation{}
		}
		for _, flag := range spec.Flags {
			if !strings.ContainsRune("imsU", flag) {
				s.bindError("KQLB0370", operator.Span, feature, "parse flags must be among i, m, s and U")
				return Relation{}
			}
		}
	}
	if input.Schema.Unknown {
		s.bindError("KQLB0377", operator.Span, feature, operator.Kind+" requires a known input schema")
		return Relation{}
	}
	pattern, columns, ok := s.parseRegex(spec, feature)
	if !ok {
		return Relation{}
	}
	if !supportsRegexCaptures(s.compiler.dialect) {
		s.error("KQLL0370", operator.Span, feature, operator.Kind+" requires a regex capture capable SQL dialect")
		return Relation{}
	}
	previousInput := s.input
	defer func() { s.input = previousInput }()

	reserved := map[string]struct{}{}
	for _, column := range input.Schema.Columns {
		reserved[strings.ToLower(column.Name)] = struct{}{}
	}
	for _, column := range columns {
		reserved[strings.ToLower(column.name)] = struct{}{}
	}
	hidden := func(base string) string {
		for i := 0; ; i++ {
			name := "__ksql_parse_" + base + strconv.Itoa(i)
			key := strings.ToLower(name)
			if _, used := reserved[key]; used {
				continue
			}
			if _, let := s.scalarLets[key]; let {
				continue
			}
			reserved[key] = struct{}{}
			return name
		}
	}
	name := func(text string) kql.Expression { return &kql.NameExpression{Name: text, Span: operator.Span} }
	call := func(function string, args ...kql.Expression) kql.Expression {
		return &kql.CallExpression{Function: name(function), Args: args, Span: operator.Span}
	}
	passThrough := func(schema Schema, extra ...sqlast.SelectItem) []sqlast.SelectItem {
		items := make([]sqlast.SelectItem, 0, len(schema.Columns)+len(extra))
		for _, column := range schema.Columns {
			items = append(items, sqlast.SelectItem{Expr: s.columnIdentifier(column.Name), Alias: column.Name})
		}
		return append(items, extra...)
	}

	// Stage 1: one regex evaluation per row, kept as a dynamic capture array.
	s.input = input.Schema
	source := s.expression(spec.Source)
	if source.SQL == nil {
		return Relation{}
	}
	if source.Type != TypeString {
		source = s.expression(call("tostring", spec.Source))
	}
	matchName := hidden("match")
	matchSchema := input.Schema.Clone()
	matchSchema.Columns = append(matchSchema.Columns, Column{Name: matchName, Type: TypeDynamic})
	query := sqlast.Query(&sqlast.Select{
		Fence:       true,
		From:        &sqlast.Subquery{Query: input.Query, Alias: s.nextAlias()},
		Projections: passThrough(input.Schema, sqlast.SelectItem{Expr: &sqlast.RegexCaptures{Value: source.SQL, Pattern: s.bind(pattern)}, Alias: matchName}),
	})

	// Stage 2: typed values. Strict kinds bind hidden values first, so one
	// failed conversion can null every extracted column without reconversion.
	strict := spec.Kind != "relaxed"
	gated := false
	for _, column := range columns {
		gated = gated || (strict && column.conversion != "")
	}
	s.input = matchSchema
	output := input.Schema.Clone()
	type typedValue struct {
		column parseColumn
		name   string
		value  boundExpr
	}
	values := make([]typedValue, len(columns))
	for i, column := range columns {
		capture := call("tostring", &kql.IndexExpression{Value: name(matchName), Index: &kql.LiteralExpression{Text: strconv.Itoa(column.group - 1), Kind: kql.NumberToken, Span: column.span}, Span: column.span})
		if column.conversion != "" {
			capture = call(column.conversion, capture)
		}
		value := s.expression(capture)
		if value.SQL == nil {
			return Relation{}
		}
		values[i] = typedValue{column: column, name: column.name, value: value}
		if gated {
			values[i].name = hidden("value")
		}
	}
	// Ungated values replace same-named input columns; gated values stay
	// hidden until the final stage, so the inputs pass through unchanged.
	valueSchema := Schema{}
	for _, column := range input.Schema.Columns {
		replaced := false
		for _, value := range values {
			replaced = replaced || strings.EqualFold(value.column.name, column.Name)
		}
		if !replaced || gated {
			valueSchema.Columns = append(valueSchema.Columns, column)
		}
	}
	for _, value := range values {
		if _, index, found, _ := output.Lookup(value.column.name); found {
			output.Columns[index] = Column{Name: output.Columns[index].Name, Type: value.value.Type}
		} else {
			output.Columns = append(output.Columns, Column{Name: value.column.name, Type: value.value.Type})
		}
	}
	if !s.checkProjectionWidth(len(valueSchema.Columns)+len(values), operator.Span, feature) {
		return Relation{}
	}
	items := passThrough(valueSchema)
	for _, value := range values {
		items = append(items, sqlast.SelectItem{Expr: value.value.SQL, Alias: value.name})
		valueSchema.Columns = append(valueSchema.Columns, Column{Name: value.name, Type: value.value.Type})
	}
	stage := &sqlast.Select{From: &sqlast.Subquery{Query: query, Alias: s.nextAlias()}, Projections: items, Fence: gated}
	if operator.Kind == "parse-where" {
		stage.Where = notNull(s.columnIdentifier(matchName))
	}
	query = stage
	if !gated {
		return Relation{Query: s.orderParseOutput(query, valueSchema, output), Schema: output}
	}

	// Stage 3: strict kinds require every typed conversion to succeed.
	s.input = valueSchema
	var condition sqlast.Expr
	for _, value := range values {
		if value.column.conversion == "" {
			continue
		}
		present := notNull(s.columnIdentifier(value.name))
		if condition == nil {
			condition = present
		} else {
			condition = &sqlast.Binary{Left: condition, Operator: "AND", Right: present}
		}
	}
	final := &sqlast.Select{From: &sqlast.Subquery{Query: query, Alias: s.nextAlias()}}
	if operator.Kind == "parse-where" {
		final.Where = condition
	}
	for _, column := range output.Columns {
		expr := s.columnIdentifier(column.Name)
		for _, value := range values {
			if strings.EqualFold(value.column.name, column.Name) {
				expr = s.columnIdentifier(value.name)
				if operator.Kind != "parse-where" {
					expr = &sqlast.Case{Branches: []sqlast.When{{Condition: condition, Result: expr}}}
				}
				break
			}
		}
		final.Projections = append(final.Projections, sqlast.SelectItem{Expr: expr, Alias: column.Name})
	}
	return Relation{Query: final, Schema: output}
}

// orderParseOutput projects the ungated stage in the output schema order,
// replacing same-named input columns in place as extend does.
func (s *compileState) orderParseOutput(query sqlast.Query, stage Schema, output Schema) sqlast.Query {
	s.input = stage
	items := make([]sqlast.SelectItem, len(output.Columns))
	for i, column := range output.Columns {
		items[i] = sqlast.SelectItem{Expr: s.columnIdentifier(column.Name), Alias: column.Name}
	}
	return &sqlast.Select{From: &sqlast.Subquery{Query: query, Alias: s.nextAlias()}, Projections: items}
}

func notNull(expr sqlast.Expr) sqlast.Expr {
	return &sqlast.Binary{Left: expr, Operator: "IS NOT", Right: &sqlast.Literal{Kind: sqlast.NullLiteral}}
}
