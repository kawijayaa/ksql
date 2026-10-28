package ksql

import (
	"encoding/json"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// constantValue reads the value of a bound constant: a literal, a bound
// parameter, or a null literal.
func (s *compileState) constantValue(expr sqlast.Expr) (any, bool) {
	switch value := expr.(type) {
	case *sqlast.Literal:
		if value.Kind == sqlast.NullLiteral {
			return nil, true
		}
		return value.Value, value.Kind == sqlast.StringLiteral || value.Kind == sqlast.JSONLiteral
	case *sqlast.Parameter:
		if value.Index >= 1 && value.Index <= len(s.args) {
			return s.args[value.Index-1], true
		}
	}
	return nil, false
}

// constantNames evaluates a project-by-names specifier at compile time: a
// string (a name or a * pattern), a dynamic array of strings, or
// column_names_of(T). Arguments bound while evaluating are discarded, since
// the specifier does not appear in the SQL.
func (s *compileState) constantNames(expression kql.Expression) ([]string, bool) {
	saved := len(s.args)
	defer func() { s.args = s.args[:saved] }()
	bound := s.expression(expression)
	if bound.SQL == nil {
		return nil, false
	}
	value, ok := s.constantValue(bound.SQL)
	if !ok {
		return nil, false
	}
	text, isText := value.(string)
	switch {
	case value == nil:
		return nil, true
	case !isText:
		return nil, false
	case bound.Type == TypeString:
		return []string{text}, true
	case bound.Type == TypeDynamic:
		var names []any
		if json.Unmarshal([]byte(text), &names) != nil {
			var single string
			if json.Unmarshal([]byte(text), &single) == nil {
				return []string{single}, true
			}
			return nil, false
		}
		result := make([]string, 0, len(names))
		for _, name := range names {
			if text, ok := name.(string); ok {
				result = append(result, text)
			}
		}
		return result, true
	}
	return nil, false
}

// projectByNames keeps the columns named by constant specifiers, in the
// order specified or matched. Names that match no column are ignored.
func (s *compileState) projectByNames(from *sqlast.Subquery, input Schema, operator kql.Operator) Relation {
	if input.Unknown {
		s.bindError("KQLB0340", operator.Span, "operator.project-by-names", "project-by-names requires a bound input schema")
		return Relation{}
	}
	spec := operator.Body.(kql.ExpressionSpec)
	if !s.checkProjectionLimit(spec.Expressions, operator) {
		return Relation{}
	}
	output := Schema{}
	used := map[string]bool{}
	deferred := false
	add := func(name string) {
		column, _, found, ambiguous := input.Lookup(name)
		if !found || ambiguous || used[strings.ToLower(column.Name)] {
			return
		}
		used[strings.ToLower(column.Name)] = true
		output.Columns = append(output.Columns, column)
	}
	for _, expression := range spec.Expressions {
		if pattern, ok := expression.(*kql.ColumnPatternExpression); ok {
			for _, name := range matchingColumns(input.Columns, pattern.Pattern, pattern.Order) {
				add(name)
			}
			continue
		}
		names, ok := s.constantNames(expression)
		if !ok && s.functionValidation {
			// Function parameters are placeholders while a declaration is
			// checked; the call site binds their constant values.
			deferred = true
			continue
		}
		if !ok {
			s.bindError("KQLB0345", expression.NodeSpan(), "operator.project-by-names", "project-by-names requires constant column names, dynamic arrays of names, or column_names_of()")
			continue
		}
		for _, name := range names {
			if strings.Contains(name, "*") {
				for _, match := range matchingColumns(input.Columns, name, "") {
					add(match)
				}
				continue
			}
			add(name)
		}
	}
	if deferred {
		return functionTablePlaceholder(Schema{})
	}
	if len(output.Columns) == 0 {
		s.bindError("KQLB0343", operator.Span, "operator.project-by-names", "projection cannot remove every column")
		return Relation{}
	}
	if maximum := s.compiler.limits.MaxProjectionItems; maximum > 0 && len(output.Columns) > maximum {
		s.bindError("KQLB0344", operator.Span, "operator.project-by-names", "expanded projection exceeds MaxProjectionItems")
		return Relation{}
	}
	items := make([]sqlast.SelectItem, 0, len(output.Columns))
	for _, column := range output.Columns {
		items = append(items, sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{column.Name}}, Alias: column.Name})
	}
	return Relation{Query: &sqlast.Select{From: from, Projections: items}, Schema: output}
}

// columnNamesOf returns the column names of a tabular expression as a
// dynamic array, resolved at compile time.
func (s *compileState) columnNamesOf(call *kql.CallExpression) boundExpr {
	var name *kql.NameExpression
	ok := len(call.Args) == 1
	if ok {
		name, ok = call.Args[0].(*kql.NameExpression)
	}
	if !ok {
		s.bindError("KQLB0413", call.Span, "function.column_names_of", "column_names_of requires one table name")
		return boundExpr{}
	}
	saved := len(s.args)
	relation := s.source(kql.Source{Kind: "table", Name: name.Name, Span: name.Span})
	s.args = s.args[:saved]
	if relation.Query == nil || relation.Schema.Unknown {
		if relation.Query != nil {
			s.bindError("KQLB0413", call.Span, "function.column_names_of", "column_names_of requires a table with a known schema")
		}
		return boundExpr{}
	}
	names := make([]string, len(relation.Schema.Columns))
	for i, column := range relation.Schema.Columns {
		names[i] = column.Name
	}
	encoded, _ := json.Marshal(names)
	return boundExpr{SQL: s.bind(string(encoded)), Type: TypeDynamic}
}
