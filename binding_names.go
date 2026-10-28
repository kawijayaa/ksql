package ksql

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

func splitName(name string) []string {
	parts := strings.Split(name, ".")
	for i := range parts {
		parts[i] = unquoteKQL(parts[i])
	}
	return parts
}

func lastName(name string) string {
	parts := splitName(name)
	return parts[len(parts)-1]
}

// The default suffix is shared across binding the whole script. Only extend
// includes the row scope when avoiding names that already bind.
func (s *compileState) defaultColumnName(span kql.Span, includeRowScope bool) string {
	for attempt := 0; attempt < 4096; attempt++ {
		s.defaultColumn++
		name := fmt.Sprintf("Column%d", s.defaultColumn)
		key := strings.ToLower(name)
		if _, found := s.scalarLets[key]; found {
			continue
		}
		if _, found := s.tabularLets[key]; found {
			continue
		}
		if _, found := s.functions[key]; found {
			continue
		}
		if _, found := s.compiler.functionRules[key]; found {
			continue
		}
		if _, found := s.compiler.typedFunctionRules[key]; found {
			continue
		}
		if includeRowScope && s.hasInputName(name) {
			continue
		}
		if s.compiler.catalog != nil {
			if _, found := s.compiler.catalog.ResolveTable(name); found {
				continue
			}
		}
		return name
	}
	s.bindError("KQLB0407", span, "expression.name", "generated column name search exceeds 4096 collisions")
	return ""
}

func (s *compileState) hasInputName(name string) bool {
	if s.aggregationAllowed {
		return false
	}
	_, _, found, ambiguous := s.input.Lookup(name)
	return found || ambiguous
}

// hasExactInputName reports an input column spelled exactly like name. KQL
// names are case-sensitive, so a let named region is not shadowed by a column
// named Region.
func (s *compileState) hasExactInputName(name string) bool {
	if s.aggregationAllowed {
		return false
	}
	for _, column := range s.input.Columns {
		if column.Name == name {
			return true
		}
	}
	return false
}

func (s *compileState) name(value *kql.NameExpression) boundExpr {
	logicalName := unquoteKQL(value.Name)
	parts := []string{logicalName}
	if logicalName == value.Name {
		parts = splitName(logicalName)
	}
	if len(parts) > 1 {
		if bound, ok := s.scalarLets[strings.ToLower(parts[0])]; ok && !s.hasExactInputName(parts[0]) && !s.hasExactInputName(unquoteKQL(value.Name)) {
			if bound.Type != TypeDynamic && bound.Type != TypeUnknown {
				s.bindError("KQLB0403", value.Span, "expression.name", parts[0]+" is not dynamic")
				return boundExpr{}
			}
			s.captureRows(bound.rows)
			expr := bound.SQL
			for _, property := range parts[1:] {
				expr = &sqlast.Index{Value: expr, Index: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: property}}
			}
			return boundExpr{SQL: expr, Type: TypeDynamic, Name: strings.Join(parts, "_")}
		}
	}
	if s.aggregationAllowed {
		s.bindError("KQLB0513", value.Span, "operator.summarize", "input column "+value.Name+" must be inside an aggregate function")
		return boundExpr{}
	}
	if len(parts) > 1 && s.qualifiers != nil {
		if scope, ok := s.qualifiers[strings.ToLower(parts[0])]; ok {
			return s.qualifiedName(value, scope, parts[1:])
		}
	}
	if !s.input.Unknown {
		if column, _, ok, ambiguous := s.input.Lookup(unquoteKQL(value.Name)); ambiguous {
			s.bindError("KQLB0402", value.Span, "expression.name", "ambiguous column "+value.Name)
			return boundExpr{}
		} else if ok {
			return boundExpr{SQL: s.columnIdentifier(column.Name), Type: column.Type, Name: column.Name}
		}
		if len(parts) > 1 {
			if column, _, ok, ambiguous := s.input.Lookup(parts[0]); ambiguous {
				s.bindError("KQLB0402", value.Span, "expression.name", "ambiguous column "+parts[0])
				return boundExpr{}
			} else if ok {
				if column.Type != TypeDynamic {
					s.bindError("KQLB0403", value.Span, "expression.name", column.Name+" is not dynamic")
					return boundExpr{}
				}
				expr := s.columnIdentifier(column.Name)
				for _, property := range parts[1:] {
					expr = &sqlast.Index{Value: expr, Index: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: property}}
				}
				return boundExpr{SQL: expr, Type: TypeDynamic, Name: strings.Join(append([]string{column.Name}, parts[1:]...), "_")}
			}
		}
		if s.blockedRows[strings.ToLower(logicalName)] || s.blockedRows[strings.ToLower(parts[0])] {
			if s.blockedFeature == "function.materialize" {
				s.error("KQLL0524", value.Span, s.blockedFeature, "caller-row-dependent materialize requires correlated materialization, which this SQL lowering does not support")
			} else {
				s.bindError("KQLB0521", value.Span, "function.toscalar", "toscalar cannot use "+value.Name+" outside its row-context scope")
			}
			return boundExpr{}
		}
		s.bindError("KQLB0401", value.Span, "expression.name", "unknown column "+value.Name)
		return boundExpr{}
	}
	if len(parts) == 1 && s.qualifier != "" {
		parts = append([]string{s.qualifier}, parts...)
	}
	return boundExpr{SQL: s.rowIdentifier(parts), Type: TypeUnknown, Name: parts[len(parts)-1]}
}

func (s *compileState) qualifiedName(value *kql.NameExpression, scope relationScope, parts []string) boundExpr {
	if s.aggregationAllowed {
		s.bindError("KQLB0513", value.Span, "operator.summarize", "input column "+value.Name+" must be inside an aggregate function")
		return boundExpr{}
	}
	if len(parts) == 0 {
		s.bindError("KQLB0401", value.Span, "expression.name", "missing qualified column")
		return boundExpr{}
	}
	if scope.schema.Unknown {
		return boundExpr{SQL: s.rowIdentifier([]string{scope.alias, parts[0]}), Type: TypeUnknown, Name: parts[0]}
	}
	column, _, ok, ambiguous := scope.schema.Lookup(parts[0])
	if ambiguous {
		s.bindError("KQLB0402", value.Span, "expression.name", "ambiguous column "+parts[0])
		return boundExpr{}
	}
	if !ok {
		s.bindError("KQLB0401", value.Span, "expression.name", "unknown column "+parts[0])
		return boundExpr{}
	}
	expr := s.rowIdentifier([]string{scope.alias, column.Name})
	if len(parts) > 1 {
		if column.Type != TypeDynamic {
			s.bindError("KQLB0403", value.Span, "expression.name", column.Name+" is not dynamic")
			return boundExpr{}
		}
		for _, property := range parts[1:] {
			expr = &sqlast.Index{Value: expr, Index: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: property}}
		}
		return boundExpr{SQL: expr, Type: TypeDynamic, Name: strings.Join(append([]string{column.Name}, parts[1:]...), "_")}
	}
	return boundExpr{SQL: expr, Type: column.Type, Name: column.Name}
}

func (s *compileState) columnIdentifier(name string) sqlast.Expr {
	parts := []string{name}
	if s.qualifier != "" {
		parts = append([]string{s.qualifier}, parts...)
	}
	return s.rowIdentifier(parts)
}

// Dynamic accessors compose the base expression's name with a literal
// string/integer selector. Computed selectors have no inferred name.
func indexedResultName(left, right string) string {
	if strings.TrimSpace(left) != "" {
		return left + "_" + right
	}
	return right
}

func literalIndexName(expression kql.Expression) string {
	literal, ok := expression.(*kql.LiteralExpression)
	if !ok {
		return ""
	}
	if literal.Kind == kql.StringToken {
		return stringLiteralValue(literal)
	}
	if literal.Kind == kql.NumberToken {
		if value, err := strconv.ParseInt(literal.Text, 10, 64); err == nil {
			return strconv.FormatInt(value, 10)
		}
	}
	return ""
}

// ResultNameKind.FirstArgument in Microsoft's scalar function catalog.
func firstArgumentResultName(function string) bool {
	switch function {
	case "parse_json", "parsejson", "todynamic", "toobject", "parse_csv", "parse_xml",
		"tostring", "tohex", "tolong", "toint", "toreal", "todouble", "todatetime",
		"totimespan", "totime", "tobool", "toboolean", "todecimal", "toguid":
		return true
	}
	return false
}

// inferredName applies Kusto's result-name rules: a bound name (dynamic paths,
// propagated first arguments) wins, a column reference keeps its name, and a
// call follows its function's ResultNameKind. Calls without a rule have no
// name, so projection binding generates ColumnN.
func inferredName(expression kql.Expression, boundName string) string {
	if boundName != "" {
		return boundName
	}
	switch value := expression.(type) {
	case *kql.NameExpression:
		return lastName(value.Name)
	case *kql.CallExpression:
		function, ok := value.Function.(*kql.NameExpression)
		if !ok {
			return ""
		}
		name := strings.ToLower(lastName(function.Name))
		rule, ok := resultNameRules[name]
		if !ok {
			return ""
		}
		first := ""
		if len(value.Args) > 0 {
			first = inferredName(value.Args[0], "")
		}
		// Kusto names covariance results after both arguments, a custom rule
		// not expressed by ResultNameKind: covariance_x_y.
		if strings.HasPrefix(name, "covariance") && len(value.Args) >= 2 {
			if second := inferredName(value.Args[1], ""); first != "" && second != "" {
				return rule.prefix + "_" + first + "_" + second
			}
		}
		switch rule.kind {
		case resultFirstArgument:
			return first
		case resultOnlyArgument:
			if len(value.Args) == 1 {
				return first
			}
		case resultPrefixAndFirstArgument:
			return rule.prefix + "_" + first
		case resultNameAndFirstArgument:
			if first != "" {
				return name + "_" + first
			}
		case resultPrefixOnly:
			return rule.prefix
		}
	}
	return ""
}

func nameSet(names []string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[strings.ToLower(name)] = struct{}{}
	}
	return result
}

func nameSetFromColumns(columns []Column) map[string]struct{} {
	result := make(map[string]struct{}, len(columns))
	for _, column := range columns {
		result[strings.ToLower(column.Name)] = struct{}{}
	}
	return result
}
