package ksql

import (
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

func (s *compileState) source(source kql.Source) Relation {
	s.use("source."+source.Kind, "equivalent", source.Span)
	if source.Kind == "management" && (!s.managementAllowed || s.pipelineDepth != 1) {
		s.bindError("KQLB0250", source.Span, "source.management", "management commands must be standalone top-level commands with optional result pipelines")
		return Relation{}
	}
	if source.Kind == "table" {
		if bound, ok := s.tabularLets[strings.ToLower(unquoteKQL(source.Name))]; ok {
			s.captureRows(bound.rowDependencies)
			return bound
		}
		if function := s.functions[strings.ToLower(unquoteKQL(source.Name))]; function != nil {
			if !function.tabular {
				s.bindError("KQLB0120", source.Span, "source.expression", "function does not return a table")
				return Relation{}
			}
			return s.callFunctionValue(function, &kql.CallExpression{Function: &kql.NameExpression{Name: source.Name, Span: source.Span}, Span: source.Span}, nil).table
		}
	}
	if rule, ok := s.compiler.sourceRules[strings.ToLower(source.Kind)]; ok {
		relation, err := rule(&loweringContext{state: s, span: source.Span}, source)
		if err != nil {
			s.error("KQLL0203", source.Span, "source."+source.Kind, err.Error())
			return Relation{}
		}
		if relation.Query == nil {
			s.error("KQLL0204", source.Span, "source."+source.Kind, "custom source rule returned a nil query")
		}
		return relation
	}
	switch source.Kind {
	case "management":
		return s.managementSource(source)
	case "table":
		name := source.Name
		schema := Schema{Unknown: true}
		if s.compiler.catalog != nil {
			table, ok := s.compiler.catalog.ResolveTable(source.Name)
			if !ok {
				s.bindError("KQLB0201", source.Span, "source.table", "unknown table "+source.Name)
				return Relation{}
			}
			name, schema = table.Name, table.Schema.Clone()
			if name == "" {
				name = source.Name
			}
		}
		return Relation{Query: &sqlast.Select{From: &sqlast.Table{Parts: splitName(name)}}, Schema: schema}
	case "union":
		return s.union(Relation{}, kql.Operator{Kind: "union", Body: *source.Union, Span: source.Span})
	case "search":
		return s.searchSource(source)
	case "print":
		items, schema := s.projectItems(source.Items, true, Schema{}, true)
		return Relation{Query: &sqlast.Select{Projections: items}, Schema: schema}
	case "datatable":
		columns := make([]string, len(source.Columns))
		for i, column := range source.Columns {
			columns[i] = unquoteKQL(column.Name)
		}
		rows := make([][]sqlast.Expr, len(source.Rows))
		for i, row := range source.Rows {
			rows[i] = s.expressions(row)
		}
		schema := Schema{Columns: make([]Column, len(source.Columns))}
		for i, column := range source.Columns {
			schema.Columns[i] = Column{Name: unquoteKQL(column.Name), Type: parseScalarType(column.Type)}
		}
		s.validateUnique(schema.Columns, source.Span, "source.datatable")
		return Relation{Query: &sqlast.Select{From: &sqlast.Values{Columns: columns, Rows: rows, Alias: s.nextAlias()}}, Schema: schema}
	case "range":
		from, to, step := s.expression(source.From), s.expression(source.To), s.expression(source.Step)
		if from.SQL == nil || to.SQL == nil || step.SQL == nil {
			return Relation{}
		}
		if from.Type == TypeDateTime || to.Type == TypeDateTime || from.Type == TypeTimespan || step.Type == TypeTimespan {
			return s.temporalRange(source.Name, from, to, step, source.Span)
		}
		columnType := TypeLong
		if s.compiler.dialect.Name() == "sqlite" {
			for _, value := range []boundExpr{from, to, step} {
				switch value.Type {
				case TypeInt, TypeLong:
				case TypeReal:
					columnType = TypeReal
				default:
					s.bindError("KQLB0216", source.Span, "source.range", "SQLite range currently requires numeric bounds and step")
					return Relation{}
				}
			}
			if columnType == TypeReal {
				from.SQL = &sqlast.Cast{Expr: from.SQL, Type: "REAL"}
				to.SQL = &sqlast.Cast{Expr: to.SQL, Type: "REAL"}
				step.SQL = &sqlast.Cast{Expr: step.SQL, Type: "REAL"}
			}
		} else if from.Type == TypeDateTime {
			columnType = TypeDateTime
		} else if from.Type == TypeReal || to.Type == TypeReal || step.Type == TypeReal {
			columnType = TypeReal
		}
		return Relation{Query: &sqlast.Select{From: &sqlast.Series{Column: source.Name, From: from.SQL, To: to.SQL, Step: step.SQL, Alias: s.nextAlias()}}, Schema: Schema{Columns: []Column{{Name: source.Name, Type: columnType}}}}
	case "expression":
		if call, ok := source.Expr.(*kql.CallExpression); ok {
			if name, ok := call.Function.(*kql.NameExpression); ok {
				if function := s.functions[strings.ToLower(unquoteKQL(name.Name))]; function != nil {
					if !function.tabular {
						s.bindError("KQLB0120", call.Span, "source.expression", "function does not return a table")
						return Relation{}
					}
					return s.callFunctionValue(function, call, nil).table
				}
				if strings.EqualFold(unquoteKQL(name.Name), "materialize") {
					return s.materializeCall(call)
				}
			}
			if name, ok := entityName(call); ok {
				// table("T") and its peers resolve exactly like the bare name T,
				// through application source rules and the catalog, so they can
				// never reach storage tables the catalog does not expose.
				return s.source(kql.Source{Kind: "table", Name: name, Span: call.Span})
			}
		}
		s.error("KQLL0201", source.Span, "source.expression", "expression is not a supported tabular source")
	default:
		s.error("KQLL0202", source.Span, "source."+source.Kind, source.Kind+" has no generic SQL source equivalent")
	}
	return Relation{}
}

// entityName reads the literal name in table("T"), external_table("T") or
// materialized_view("T").
func entityName(call *kql.CallExpression) (string, bool) {
	name, ok := call.Function.(*kql.NameExpression)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	function := strings.ToLower(name.Name)
	if function != "table" && function != "external_table" && function != "materialized_view" {
		return "", false
	}
	literal, ok := call.Args[0].(*kql.LiteralExpression)
	if !ok || literal.Kind != kql.StringToken {
		return "", false
	}
	return stringLiteralValue(literal), true
}

// temporalRange lowers datetime and timespan ranges onto the numeric series:
// timespans are ticks, and datetimes are offsets from the lower bound.
func (s *compileState) temporalRange(name string, from, to, step boundExpr, span kql.Span) Relation {
	feature := "source.range"
	switch {
	case from.Type == TypeTimespan && to.Type == TypeTimespan && step.Type == TypeTimespan:
		series := &sqlast.Series{Column: name, From: from.SQL, To: to.SQL, Step: step.SQL, Alias: s.nextAlias()}
		return Relation{Query: &sqlast.Select{From: series}, Schema: Schema{Columns: []Column{{Name: name, Type: TypeTimespan}}}}
	case from.Type == TypeDateTime && to.Type == TypeDateTime && step.Type == TypeTimespan:
		if !supportsDatetimeArithmetic(s.compiler.dialect) {
			s.error("KQLL0412", span, feature, "datetime range requires a dialect datetime capability")
			return Relation{}
		}
		offset := "__ksql_range_offset"
		if strings.EqualFold(name, offset) {
			offset += "1"
		}
		series := &sqlast.Series{Column: offset, From: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "0"}, To: &sqlast.DatetimeDiff{Left: to.SQL, Right: from.SQL}, Step: step.SQL, Alias: s.nextAlias()}
		value := &sqlast.DatetimeAdd{Datetime: from.SQL, Ticks: &sqlast.Identifier{Parts: []string{offset}}}
		query := &sqlast.Select{From: &sqlast.Subquery{Query: &sqlast.Select{From: series}, Alias: s.nextAlias()}, Projections: []sqlast.SelectItem{{Expr: value, Alias: name}}}
		return Relation{Query: query, Schema: Schema{Columns: []Column{{Name: name, Type: TypeDateTime}}}}
	}
	s.bindError("KQLB0216", span, feature, "range requires numeric bounds and step, timespan bounds and step, or datetime bounds with a timespan step")
	return Relation{}
}
