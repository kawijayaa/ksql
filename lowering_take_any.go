package ksql

import (
	"fmt"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// Rank by presence only, never by the payload's value. Multiple expressions
// share one rank so their values always come from the same input record.
func (s *compileState) takeAnySelection(call *kql.CallExpression, function string, input Schema, byColumns map[string]bool, prepare func(boundExpr) string, assignName func(string, int) string, internalName func() string) ([]sqlast.SelectItem, []Column, rowSelection) {
	feature := "aggregate." + function
	conditional := function == "take_anyif" || function == "anyif"
	legacy := function == "any" || function == "anyif"
	selection := rowSelection{rank: internalName()}
	if len(call.Args) == 0 || len(call.Args) > 128 || conditional && len(call.Args) != 2 {
		message := "row-selection aggregate requires 1 to 128 scalar or wildcard arguments"
		if conditional {
			message = function + " requires an expression and a Boolean predicate"
		}
		s.bindError("KQLB0351", call.Span, feature, message)
		return nil, nil, selection
	}
	arguments := call.Args
	var predicate sqlast.Expr
	if conditional {
		if _, star := call.Args[0].(*kql.StarExpression); star {
			s.bindError("KQLB0351", call.Args[0].NodeSpan(), feature, function+" does not accept a wildcard")
			return nil, nil, selection
		}
		condition := s.expression(call.Args[1])
		if condition.SQL == nil {
			return nil, nil, selection
		}
		if condition.Type != TypeBool {
			s.bindError("KQLB0354", call.Args[1].NodeSpan(), feature, function+" predicate must have a known Boolean type")
			return nil, nil, selection
		}
		predicate = &sqlast.Identifier{Parts: []string{prepare(condition)}}
		// SQL CASE treats a null predicate as false. Order true before all
		// other records, and null out the result if none are eligible.
		eligibility := &sqlast.Case{Branches: []sqlast.When{{Condition: predicate, Result: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "0"}}}, Else: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "1"}}
		selection.orderBy = append(selection.orderBy, sqlast.Order{Expr: eligibility, Direction: "ASC"})
		arguments = call.Args[:1]
	}
	anyStar := false
	for _, arg := range arguments {
		if _, star := arg.(*kql.StarExpression); star {
			anyStar = true
		}
	}
	// The legacy any signatures allow either scalars or a single wildcard.
	if legacy && anyStar && len(arguments) != 1 {
		s.bindError("KQLB0351", call.Span, feature, "any wildcard must be its only argument")
		return nil, nil, selection
	}
	items := []sqlast.SelectItem{}
	columns := []Column{}
	selected := map[string]bool{}
	appendValue := func(value boundExpr, name string) {
		if value.SQL == nil {
			return
		}
		if value.Type == TypeUnknown {
			s.bindError("KQLB0352", call.Span, feature, "row-selection payload requires a known scalar type")
			return
		}
		if legacy {
			prefix := "any_"
			if conditional {
				prefix = "anyif_"
			}
			name = prefix + name
		}
		name = assignName(name, len(columns))
		preparedName := prepare(value)
		payload := sqlast.Expr(&sqlast.Identifier{Parts: []string{preparedName}})
		if len(arguments) == 1 && !anyStar {
			selection.orderBy = append(selection.orderBy, sqlast.Order{Expr: missingValueRank(payload, value.Type, true), Direction: "ASC"})
		}
		if predicate != nil {
			payload = &sqlast.Case{Branches: []sqlast.When{{Condition: predicate, Result: payload}}, Else: &sqlast.Literal{Kind: sqlast.NullLiteral}}
		}
		items = append(items, sqlast.SelectItem{Expr: &sqlast.RankedValue{Value: payload, Rank: &sqlast.Identifier{Parts: []string{selection.rank}}}, Alias: name})
		columns = append(columns, Column{Name: name, Type: value.Type})
	}
	for i, arg := range arguments {
		if _, star := arg.(*kql.StarExpression); star {
			for _, column := range input.Columns {
				key := strings.ToLower(column.Name)
				if !selected[key] && !byColumns[key] {
					selected[key] = true
					appendValue(boundExpr{SQL: s.columnIdentifier(column.Name), Type: column.Type}, column.Name)
				}
			}
			continue
		}
		expr := arg
		name := ""
		if named, ok := arg.(*kql.NamedExpression); ok {
			expr, name = named.Value, unquoteKQL(named.Name)
		}
		value := s.expression(expr)
		if id, ok := value.SQL.(*sqlast.Identifier); ok && len(id.Parts) == 1 {
			key := strings.ToLower(id.Parts[0])
			if anyStar && (selected[key] || byColumns[key]) {
				continue
			}
			selected[key] = true
		}
		if name == "" {
			name = inferredName(expr, value.Name)
		}
		if name == "" {
			name = fmt.Sprintf("arg%d", i)
			if !legacy && !conditional {
				name = "any_" + name
			}
		}
		appendValue(value, name)
	}
	if len(columns) == 0 {
		s.bindError("KQLB0355", call.Span, feature, "row-selection wildcard has no remaining result columns")
	}
	return items, columns, selection
}

func missingValueRank(value sqlast.Expr, typ ScalarType, includeEmpty bool) sqlast.Expr {
	var missing sqlast.Expr = &sqlast.Binary{Left: value, Operator: "IS", Right: &sqlast.Literal{Kind: sqlast.NullLiteral}}
	if includeEmpty && typ == TypeString {
		missing = &sqlast.Binary{Left: missing, Operator: "OR", Right: &sqlast.Binary{Left: value, Operator: "=", Right: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: ""}}}
	}
	return &sqlast.Case{Branches: []sqlast.When{{Condition: missing, Result: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "1"}}}, Else: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "0"}}
}
