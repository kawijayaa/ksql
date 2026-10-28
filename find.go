package ksql

import (
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// findPackChunk bounds the key/value pairs of one bag_pack call; larger
// packs are merged with bag_merge.
const findPackChunk = 32

var findConversions = map[string]string{
	"string": "tostring", "long": "tolong", "int": "toint", "real": "toreal", "double": "toreal",
	"bool": "tobool", "boolean": "tobool", "datetime": "todatetime", "timespan": "totimespan",
	"dynamic": "todynamic", "guid": "toguid",
}

// referencedColumns lists the names a predicate refers to, in order of
// first appearance; function names are not columns.
func referencedColumns(expression kql.Expression, names *[]string, seen map[string]bool) {
	switch value := expression.(type) {
	case *kql.NameExpression:
		name := unquoteKQL(value.Name)
		if !seen[name] {
			seen[name] = true
			*names = append(*names, name)
		}
	case *kql.BinaryExpression:
		if value.Operator == ":" {
			referencedColumns(value.Left, names, seen)
			return
		}
		referencedColumns(value.Left, names, seen)
		referencedColumns(value.Right, names, seen)
	case *kql.UnaryExpression:
		referencedColumns(value.Operand, names, seen)
	case *kql.CallExpression:
		for _, arg := range value.Args {
			referencedColumns(arg, names, seen)
		}
	case *kql.IndexExpression:
		referencedColumns(value.Value, names, seen)
	}
}

type findLeg struct {
	name     string
	relation Relation
}

// findSource lowers find: each matching table is filtered with the search
// predicate and projected to the source column, the selected columns and a
// pack_ bag of its remaining columns, and the legs are concatenated.
func (s *compileState) findSource(source kql.Source) Relation {
	spec := *source.Find
	feature := "source.find"
	s.use(feature, "equivalent", source.Span)
	names, ok := s.resolveTablePatterns(spec.Tables, spec.HasTables, source.Span, feature)
	if !ok {
		return Relation{}
	}
	var referenced []string
	referencedColumns(spec.Term, &referenced, map[string]bool{})
	var legs []findLeg
	for _, name := range names {
		relation := s.source(kql.Source{Kind: "table", Name: searchName(name), Span: source.Span})
		if relation.Query == nil {
			return Relation{}
		}
		if relation.Schema.Unknown {
			s.bindError("KQLB0357", source.Span, feature, "find requires tables with known schemas")
			return Relation{}
		}
		// Tables without any column the predicate uses are skipped.
		keep := len(referenced) == 0
		for _, column := range referenced {
			if _, _, found, _ := relation.Schema.Lookup(column); found {
				keep = true
			}
		}
		if keep {
			legs = append(legs, findLeg{name: name, relation: relation})
		}
	}
	if len(legs) == 0 {
		s.bindError("KQLB0357", source.Span, feature, "no table has the columns the find predicate uses")
		return Relation{}
	}
	typeOf := func(name string) (ScalarType, bool) {
		for _, leg := range legs {
			if column, _, found, _ := leg.relation.Schema.Lookup(name); found {
				return column.Type, true
			}
		}
		return TypeUnknown, false
	}
	// Output columns: the project list, or (project-smart) the referenced
	// columns followed by the columns every table shares with one type.
	var outputs []Column
	conversions := map[string]string{}
	if spec.HasProject {
		for _, item := range spec.Project {
			name := unquoteKQL(item.Name)
			kind, _ := typeOf(name)
			if item.Type != "" {
				conversion, ok := findConversions[item.Type]
				if !ok {
					s.bindError("KQLB0357", item.Span, feature, "unknown find column type "+item.Type)
					return Relation{}
				}
				kind, conversions[strings.ToLower(name)] = parseScalarType(item.Type), conversion
			}
			if kind == TypeUnknown {
				kind = TypeString
			}
			outputs = append(outputs, Column{Name: name, Type: kind})
		}
	} else {
		for _, name := range referenced {
			if kind, ok := typeOf(name); ok {
				outputs = append(outputs, Column{Name: name, Type: kind})
			}
		}
		for _, column := range legs[0].relation.Schema.Columns {
			common := true
			for _, leg := range legs[1:] {
				other, _, found, _ := leg.relation.Schema.Lookup(column.Name)
				common = common && found && other.Type == column.Type
			}
			if common && !containsColumn(outputs, column.Name) {
				outputs = append(outputs, column)
			}
		}
	}
	pack := !spec.HasProject || spec.PackAll
	schema := Schema{Columns: []Column{{Name: spec.WithSource, Type: TypeString}}}
	schema.Columns = append(schema.Columns, outputs...)
	if pack {
		schema.Columns = append(schema.Columns, Column{Name: "pack_", Type: TypeDynamic})
	}
	if s.compiler.limits.MaxProjectionItems > 0 && len(schema.Columns) > s.compiler.limits.MaxProjectionItems {
		s.bindError("KQLB0344", source.Span, feature, "find output exceeds MaxProjectionItems")
		return Relation{}
	}
	s.validateUnique(schema.Columns, source.Span, feature)
	queries := make([]sqlast.Query, 0, len(legs))
	for _, leg := range legs {
		query, ok := s.findLegQuery(leg, spec, referenced, typeOf, outputs, conversions, pack)
		if !ok {
			return Relation{}
		}
		queries = append(queries, query)
	}
	if len(queries) == 1 {
		return Relation{Query: queries[0], Schema: schema}
	}
	return Relation{Query: &sqlast.Union{All: true, Queries: queries}, Schema: schema}
}

func containsColumn(columns []Column, name string) bool {
	for _, column := range columns {
		if strings.EqualFold(column.Name, name) {
			return true
		}
	}
	return false
}

func (s *compileState) findLegQuery(leg findLeg, spec kql.FindSpec, referenced []string, typeOf func(string) (ScalarType, bool), outputs []Column, conversions map[string]string, pack bool) (sqlast.Query, bool) {
	own := leg.relation.Schema
	// Referenced columns this table lacks are null, so the predicate binds.
	extended := own.Clone()
	items := []sqlast.SelectItem{{Expr: &sqlast.Star{}}}
	for _, name := range referenced {
		if _, _, found, _ := own.Lookup(name); found {
			continue
		}
		if kind, ok := typeOf(name); ok {
			extended.Columns = append(extended.Columns, Column{Name: name, Type: kind})
			items = append(items, sqlast.SelectItem{Expr: &sqlast.Literal{Kind: sqlast.NullLiteral}, Alias: name})
		}
	}
	from := &sqlast.Subquery{Query: &sqlast.Select{From: &sqlast.Subquery{Query: leg.relation.Query, Alias: s.nextAlias()}, Projections: items}, Alias: s.nextAlias()}
	predicate, ok := s.searchCondition(kql.SearchSpec{Term: spec.Term}, extended, spec.Term.NodeSpan())
	if !ok {
		return nil, false
	}
	previous := s.input
	s.input = extended
	defer func() { s.input = previous }()
	label := s.expression(searchString(leg.name))
	projections := []sqlast.SelectItem{{Expr: label.SQL, Alias: spec.WithSource}}
	for _, column := range outputs {
		if _, _, found, _ := own.Lookup(column.Name); !found {
			projections = append(projections, sqlast.SelectItem{Expr: &sqlast.Literal{Kind: sqlast.NullLiteral}, Alias: column.Name})
			continue
		}
		expression := kql.Expression(&kql.NameExpression{Name: searchName(column.Name)})
		if conversion, ok := conversions[strings.ToLower(column.Name)]; ok {
			expression = &kql.CallExpression{Function: &kql.NameExpression{Name: conversion}, Args: []kql.Expression{expression}}
		}
		bound := s.expression(expression)
		if bound.SQL == nil {
			return nil, false
		}
		projections = append(projections, sqlast.SelectItem{Expr: bound.SQL, Alias: column.Name})
	}
	if pack {
		bag, ok := s.findPack(own, outputs)
		if !ok {
			return nil, false
		}
		projections = append(projections, sqlast.SelectItem{Expr: bag, Alias: "pack_"})
	}
	return &sqlast.Select{From: from, Where: predicate, Projections: projections}, true
}

// findPack bags a table's columns that are not output columns, through the
// application's bag_pack (and bag_merge for wide tables).
func (s *compileState) findPack(own Schema, outputs []Column) (sqlast.Expr, bool) {
	var pairs []kql.Expression
	for _, column := range own.Columns {
		if containsColumn(outputs, column.Name) {
			continue
		}
		pairs = append(pairs, searchString(column.Name), &kql.NameExpression{Name: searchName(column.Name)})
	}
	if len(pairs) == 0 {
		return s.bind("{}"), true
	}
	var chunks []kql.Expression
	for start := 0; start < len(pairs); start += 2 * findPackChunk {
		end := min(start+2*findPackChunk, len(pairs))
		chunks = append(chunks, &kql.CallExpression{Function: &kql.NameExpression{Name: "bag_pack"}, Args: pairs[start:end]})
	}
	bag := chunks[0]
	if len(chunks) > 1 {
		bag = &kql.CallExpression{Function: &kql.NameExpression{Name: "bag_merge"}, Args: chunks}
	}
	bound := s.expression(bag)
	return bound.SQL, bound.SQL != nil
}

// packAll lowers pack_all([ignore_null_empty]) to the application's bag_pack
// over the input columns. With ignore_null_empty, each column contributes
// an empty bag when it is null (or an empty string), merged with bag_merge.
func (s *compileState) packAll(call *kql.CallExpression) boundExpr {
	if len(call.Args) > 1 {
		return s.arity(call, "pack_all", 1)
	}
	ignore := false
	if len(call.Args) == 1 {
		literal, ok := call.Args[0].(*kql.LiteralExpression)
		if !ok || (!strings.EqualFold(literal.Text, "true") && !strings.EqualFold(literal.Text, "false")) {
			s.bindError("KQLB0413", call.Span, "function.pack_all", "pack_all takes a constant Boolean")
			return boundExpr{}
		}
		ignore = strings.EqualFold(literal.Text, "true")
	}
	if s.input.Unknown || len(s.input.Columns) == 0 {
		s.bindError("KQLB0512", call.Span, "function.pack_all", "pack_all requires a known non-empty input schema")
		return boundExpr{}
	}
	if !ignore {
		bag, ok := s.findPack(s.input, nil)
		if !ok {
			return boundExpr{}
		}
		return boundExpr{SQL: bag, Type: TypeDynamic}
	}
	empty := &kql.DynamicExpression{Tokens: []kql.Token{{Kind: kql.PunctuationToken, Text: "{"}, {Kind: kql.PunctuationToken, Text: "}"}}}
	var parts []kql.Expression
	for _, column := range s.input.Columns {
		name := &kql.NameExpression{Name: searchName(column.Name)}
		test := "isnull"
		if column.Type == TypeString {
			test = "isempty"
		}
		parts = append(parts, &kql.CallExpression{Function: &kql.NameExpression{Name: "iff"}, Args: []kql.Expression{
			&kql.CallExpression{Function: &kql.NameExpression{Name: test}, Args: []kql.Expression{name}},
			empty,
			&kql.CallExpression{Function: &kql.NameExpression{Name: "bag_pack"}, Args: []kql.Expression{searchString(column.Name), name}},
		}})
	}
	for len(parts) > 1 {
		var merged []kql.Expression
		for start := 0; start < len(parts); start += findPackChunk {
			end := min(start+findPackChunk, len(parts))
			if end-start == 1 {
				merged = append(merged, parts[start])
				continue
			}
			merged = append(merged, &kql.CallExpression{Function: &kql.NameExpression{Name: "bag_merge"}, Args: parts[start:end]})
		}
		parts = merged
	}
	bound := s.expression(parts[0])
	if bound.SQL == nil {
		return boundExpr{}
	}
	return boundExpr{SQL: bound.SQL, Type: TypeDynamic}
}
