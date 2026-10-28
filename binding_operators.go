package ksql

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/kawijayaa/ksql/dialect"
	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

func supportsRegex(target dialect.Dialect) bool {
	_, ok := target.Regex("", "", true)
	return ok
}

func (s *compileState) binary(value *kql.BinaryExpression) boundExpr {
	op := strings.ToLower(value.Operator)
	left := s.expression(value.Left)
	right := boundExpr{}
	var members []boundExpr
	if list, ok := value.Right.(*kql.ListExpression); ok && len(list.Items) == 1 && (op == "in" || op == "!in" || op == "in~" || op == "!in~") {
		if pipeline, tabular := list.Items[0].(*kql.PipelineExpression); tabular {
			return s.tabularMembership(op, left, pipeline, value.Span)
		}
		// in (T) names a tabular let when no column or scalar let has the name.
		if name, ok := list.Items[0].(*kql.NameExpression); ok {
			key := strings.ToLower(unquoteKQL(name.Name))
			_, tabular := s.tabularLets[key]
			_, scalar := s.scalarLets[key]
			if tabular && !scalar && !s.hasInputName(unquoteKQL(name.Name)) {
				pipeline := &kql.PipelineExpression{Pipeline: &kql.Pipeline{Source: kql.Source{Kind: "table", Name: name.Name, Span: name.Span}}, Span: name.Span}
				return s.tabularMembership(op, left, pipeline, value.Span)
			}
		}
	}
	if list, ok := value.Right.(*kql.ListExpression); ok && (op == "in" || op == "!in" || op == "in~" || op == "!in~") {
		// Bind members individually: dynamic arrays expand to their elements.
		items := make([]sqlast.Expr, len(list.Items))
		for i, item := range list.Items {
			member := s.expression(item)
			members = append(members, member)
			items[i] = member.SQL
		}
		right = boundExpr{SQL: &sqlast.List{Items: items}, Type: TypeDynamic}
	} else if !isTermOperator(op) {
		right = s.expression(value.Right)
	}
	if expanded, ok := s.dynamicMembership(op, left, members); ok {
		return expanded
	}
	if op == "and" || op == "or" {
		for _, operand := range []boundExpr{left, right} {
			if operand.Type != TypeBool && operand.Type != TypeUnknown {
				s.bindError("KQLB0410", value.Span, scalarOperatorID(op), "logical operators require Boolean operands; dynamic overloads require a compatible runtime")
				return boundExpr{}
			}
		}
	}
	s.use(scalarOperatorID(op), "equivalent", value.Span)
	if op == "+" || op == "-" || op == "*" || op == "/" || op == "%" {
		if left.Type == TypeString || right.Type == TypeString || left.Type == TypeBool || right.Type == TypeBool {
			s.bindError("KQLB0408", value.Span, scalarOperatorID(op), "arithmetic operators do not accept string or Boolean operands")
			return boundExpr{}
		}
		if left.SQL != nil && right.SQL != nil {
			if result, handled := s.temporalArithmetic(op, left, right, value.Span); handled {
				return result
			}
		}
	}
	boolean := func(expression sqlast.Expr) boundExpr { return boundExpr{SQL: expression, Type: TypeBool} }
	if isNullExpression(right.SQL) && (op == "==" || op == "!=" || op == "<>") {
		operator := "IS"
		if op != "==" {
			operator = "IS NOT"
		}
		return boolean(&sqlast.Binary{Left: left.SQL, Operator: operator, Right: right.SQL})
	}
	if isNullExpression(left.SQL) && (op == "==" || op == "!=" || op == "<>") {
		operator := "IS"
		if op != "==" {
			operator = "IS NOT"
		}
		return boolean(&sqlast.Binary{Left: right.SQL, Operator: operator, Right: left.SQL})
	}
	switch op {
	case "==":
		op = "="
	case "!=", "<>":
		op = "<>"
	case "and", "or":
		op = strings.ToUpper(op)
	case "=~", "!~":
		comparison := "="
		if op == "!~" {
			comparison = "<>"
		}
		return boolean(&sqlast.Binary{Left: &sqlast.Call{Name: "lower", Args: []sqlast.Expr{left.SQL}}, Operator: comparison, Right: &sqlast.Call{Name: "lower", Args: []sqlast.Expr{right.SQL}}})
	case "in", "!in":
		if op == "!in" {
			op = "NOT IN"
		} else {
			op = "IN"
		}
	case "in~", "!in~":
		list, ok := right.SQL.(*sqlast.List)
		if !ok || len(list.Items) == 0 {
			s.error("KQLL0403", value.Span, "operator.scalar."+op, op+" requires a non-empty scalar value list")
			return boundExpr{}
		}
		items := make([]sqlast.Expr, len(list.Items))
		for i, item := range list.Items {
			items[i] = &sqlast.Call{Name: "lower", Args: []sqlast.Expr{item}}
		}
		operator := "IN"
		if op == "!in~" {
			operator = "NOT IN"
		}
		return boolean(&sqlast.Binary{Left: &sqlast.Call{Name: "lower", Args: []sqlast.Expr{left.SQL}}, Operator: operator, Right: &sqlast.List{Items: items}})
	case "between", "!between":
		list, ok := right.SQL.(*sqlast.List)
		if !ok || len(list.Items) != 2 {
			return boundExpr{}
		}
		between := &sqlast.Binary{Left: left.SQL, Operator: "BETWEEN", Right: &sqlast.Binary{Left: list.Items[0], Operator: "AND", Right: list.Items[1]}}
		if op == "!between" {
			return boolean(&sqlast.Unary{Operator: "NOT ", Operand: between})
		}
		return boolean(between)
	case "contains", "contains_cs", "containscs", "!contains", "!contains_cs", "notcontains", "notcontainscs":
		caseSensitive := strings.Contains(op, "_cs") || strings.HasSuffix(op, "cs")
		negative := strings.HasPrefix(op, "!") || strings.HasPrefix(op, "not")
		return boolean(stringMatch(left.SQL, right.SQL, "both", caseSensitive, negative))
	case "startswith", "startswith_cs", "!startswith", "!startswith_cs":
		return boolean(stringMatch(left.SQL, right.SQL, "suffix", strings.Contains(op, "_cs"), strings.HasPrefix(op, "!")))
	case "endswith", "endswith_cs", "!endswith", "!endswith_cs":
		return boolean(stringMatch(left.SQL, right.SQL, "prefix", strings.Contains(op, "_cs"), strings.HasPrefix(op, "!")))
	case "matches regex":
		return boolean(&sqlast.Regex{Value: left.SQL, Pattern: right.SQL, CaseSensitive: true})
	case "has", "!has", "has_cs", "!has_cs", "hasprefix", "!hasprefix", "hasprefix_cs", "!hasprefix_cs", "hassuffix", "!hassuffix", "hassuffix_cs", "!hassuffix_cs":
		if !supportsRegex(s.compiler.dialect) {
			s.error("KQLL0404", value.Span, "operator.scalar."+op, op+" requires a regex-capable SQL dialect")
			return boundExpr{}
		}
		mode := "term"
		if strings.Contains(op, "prefix") {
			mode = "prefix"
		} else if strings.Contains(op, "suffix") {
			mode = "suffix"
		}
		result, ok := s.termMatchExpression(left.SQL, value.Right, mode, strings.Contains(op, "_cs"), strings.HasPrefix(op, "!"))
		if !ok {
			s.error("KQLL0404", value.Span, "operator.scalar."+op, op+" currently requires one non-empty alphanumeric literal term")
			return boundExpr{}
		}
		return boolean(result)
	case "has_any", "has_all":
		if !supportsRegex(s.compiler.dialect) {
			s.error("KQLL0404", value.Span, "operator.scalar."+op, op+" requires a regex-capable SQL dialect")
			return boundExpr{}
		}
		list, ok := value.Right.(*kql.ListExpression)
		if !ok || len(list.Items) == 0 {
			s.error("KQLL0404", value.Span, "operator.scalar."+op, op+" requires a non-empty literal term list")
			return boundExpr{}
		}
		operator := "OR"
		if op == "has_all" {
			operator = "AND"
		}
		var result sqlast.Expr
		for _, item := range list.Items {
			match, matched := s.termMatchExpression(left.SQL, item, "term", false, false)
			if !matched {
				s.error("KQLL0404", value.Span, "operator.scalar."+op, op+" currently requires non-empty alphanumeric literal terms")
				return boundExpr{}
			}
			if result == nil {
				result = match
			} else {
				result = &sqlast.Binary{Left: result, Operator: operator, Right: match}
			}
		}
		return boolean(result)
	case ":":
		s.error("KQLL0404", value.Span, "operator.scalar.colon", ": search semantics require input schema information")
		return boundExpr{}
	}
	resultType := left.Type
	if op == "=" || op == "<>" || op == "<" || op == "<=" || op == ">" || op == ">=" || op == "AND" || op == "OR" || op == "IN" || op == "NOT IN" {
		resultType = TypeBool
	} else if left.Type == TypeReal || right.Type == TypeReal {
		resultType = TypeReal
	} else if left.Type == TypeLong || right.Type == TypeLong {
		resultType = TypeLong
	}
	return boundExpr{SQL: &sqlast.Binary{Left: left.SQL, Operator: op, Right: right.SQL}, Type: resultType}
}

func isTermOperator(operator string) bool {
	switch operator {
	case "has", "!has", "has_cs", "!has_cs", "hasprefix", "!hasprefix", "hasprefix_cs", "!hasprefix_cs", "hassuffix", "!hassuffix", "hassuffix_cs", "!hassuffix_cs", "has_any", "has_all":
		return true
	default:
		return false
	}
}

func isNullExpression(expression sqlast.Expr) bool {
	literal, ok := expression.(*sqlast.Literal)
	return ok && literal.Kind == sqlast.NullLiteral
}

func (s *compileState) termMatchExpression(left sqlast.Expr, expression kql.Expression, mode string, caseSensitive, negative bool) (sqlast.Expr, bool) {
	literal, ok := expression.(*kql.LiteralExpression)
	if !ok || literal.Kind != kql.StringToken {
		return nil, false
	}
	term := stringLiteralValue(literal)
	if term == "" {
		return nil, false
	}
	// Terms with punctuation (such as an IP address) match as a phrase
	// bounded by non-alphanumeric characters; regex metacharacters are escaped.
	quoted := regexp.QuoteMeta(term)
	leftBoundary, rightBoundary := s.termBoundaries()
	pattern := leftBoundary + quoted + rightBoundary
	switch mode {
	case "prefix":
		pattern = leftBoundary + quoted + `[[:alnum:]]*`
	case "suffix":
		pattern = leftBoundary + `[[:alnum:]]*` + quoted + rightBoundary
	}
	if limit := s.compiler.limits.MaxRegexBytes; limit > 0 && len(pattern) > limit {
		s.bindError("KQLB0353", literal.Span, "operator.scalar.has", fmt.Sprintf("term pattern exceeds MaxRegexBytes (%d)", limit))
		return nil, false
	}
	result := sqlast.Expr(&sqlast.Regex{Value: left, Pattern: s.bind(pattern), CaseSensitive: caseSensitive})
	if negative {
		result = &sqlast.Unary{Operator: "NOT ", Operand: result}
	}
	return result, true
}

func (s *compileState) termBoundaries() (string, string) {
	if s.compiler.dialect.Name() == "sqlite" {
		return `(^|[^A-Za-z0-9])`, `([^A-Za-z0-9]|$)`
	}
	return `(^|[^[:alnum:]])`, `([^[:alnum:]]|$)`
}

// stringMatch compares literally: the search text is never a LIKE pattern,
// and case-sensitive operators compare exact characters.
func stringMatch(left, right sqlast.Expr, wildcard string, caseSensitive, negative bool) sqlast.Expr {
	mode := map[string]string{"both": "contains", "suffix": "startswith", "prefix": "endswith"}[wildcard]
	result := sqlast.Expr(&sqlast.StringMatch{Value: left, Pattern: right, Mode: mode, CaseSensitive: caseSensitive})
	if negative {
		result = &sqlast.Unary{Operator: "NOT ", Operand: result}
	}
	return result
}

func termMatch(left, right sqlast.Expr, mode string, caseSensitive, negative bool) (sqlast.Expr, bool) {
	literal, ok := right.(*sqlast.Literal)
	if !ok || literal.Kind != sqlast.StringLiteral || literal.Value == "" {
		return nil, false
	}
	for _, r := range literal.Value {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			return nil, false
		}
	}
	term := literal.Value
	leftBoundary, rightBoundary := `(^|[^[:alnum:]])`, `([^[:alnum:]]|$)`
	pattern := leftBoundary + term + rightBoundary
	switch mode {
	case "prefix":
		pattern = leftBoundary + term + `[[:alnum:]]*`
	case "suffix":
		pattern = leftBoundary + `[[:alnum:]]*` + term + rightBoundary
	}
	result := sqlast.Expr(&sqlast.Regex{
		Value:         left,
		Pattern:       &sqlast.Literal{Kind: sqlast.StringLiteral, Value: pattern},
		CaseSensitive: caseSensitive,
	})
	if negative {
		result = &sqlast.Unary{Operator: "NOT ", Operand: result}
	}
	return result, true
}

func scalarOperatorID(operator string) string {
	names := map[string]string{
		"+": "add", "-": "subtract", "*": "multiply", "/": "divide", "%": "modulo",
		"<": "less_than", "<=": "less_than_or_equal", ">": "greater_than", ">=": "greater_than_or_equal",
		"==": "equal", "!=": "not_equal", "<>": "not_equal", "=~": "equal_tilde", "!~": "bang_tilde",
		"!has": "not_has", "!has_cs": "not_has_cs", "!hasprefix": "not_hasprefix",
		"!hasprefix_cs": "not_hasprefix_cs", "!hassuffix": "not_hassuffix",
		"!hassuffix_cs": "not_hassuffix_cs", "!contains": "not_contains",
		"!contains_cs": "not_contains_cs", "!startswith": "not_startswith",
		"!startswith_cs": "not_startswith_cs", "!endswith": "not_endswith",
		"!endswith_cs": "not_endswith_cs", "matches regex": "matches_regex",
		"!in": "not_in", "in~": "in_cs", "!in~": "not_in_cs", "!between": "not_between",
	}
	if name, ok := names[operator]; ok {
		return "operator.scalar." + name
	}
	return "operator.scalar." + strings.ReplaceAll(operator, " ", "_")
}

func andExpression(left, right sqlast.Expr) sqlast.Expr {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	return &sqlast.Binary{Left: left, Operator: "AND", Right: right}
}

func isAlphaNumeric(value string) bool {
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			return false
		}
	}
	return value != ""
}

// dynamicMembership lowers in/!in/in~/!in~ when a member is a dynamic array,
// testing scalar members directly and array members element by element.
func (s *compileState) dynamicMembership(op string, left boundExpr, members []boundExpr) (boundExpr, bool) {
	dynamic := false
	for _, member := range members {
		dynamic = dynamic || member.Type == TypeDynamic
	}
	if !dynamic || left.SQL == nil {
		return boundExpr{}, false
	}
	insensitive := strings.HasSuffix(op, "~")
	var scalars []sqlast.Expr
	var result sqlast.Expr
	add := func(part sqlast.Expr) {
		if result == nil {
			result = part
		} else {
			result = &sqlast.Binary{Left: result, Operator: "OR", Right: part}
		}
	}
	for _, member := range members {
		if member.SQL == nil {
			return boundExpr{}, true
		}
		if member.Type == TypeDynamic {
			add(&sqlast.ArrayContains{Array: member.SQL, Value: left.SQL, CaseInsensitive: insensitive})
			continue
		}
		if insensitive {
			scalars = append(scalars, &sqlast.Call{Name: "lower", Args: []sqlast.Expr{member.SQL}})
		} else {
			scalars = append(scalars, member.SQL)
		}
	}
	if len(scalars) > 0 {
		needle := left.SQL
		if insensitive {
			needle = &sqlast.Call{Name: "lower", Args: []sqlast.Expr{needle}}
		}
		add(&sqlast.Binary{Left: needle, Operator: "IN", Right: &sqlast.List{Items: scalars}})
	}
	if strings.HasPrefix(op, "!") {
		result = &sqlast.Unary{Operator: "NOT ", Operand: result}
	}
	return boundExpr{SQL: result, Type: TypeBool}, true
}

// tabularMembership lowers x in (T | project c) to an IN subquery over the
// first column of the tabular expression. Null set members never match, so
// !in keeps rows whose value is absent from the non-null members.
func (s *compileState) tabularMembership(op string, left boundExpr, pipeline *kql.PipelineExpression, span kql.Span) boundExpr {
	feature := "operator.scalar." + op
	if left.SQL == nil {
		return boundExpr{}
	}
	previousInput, previousAggregation := s.input, s.aggregationAllowed
	relation := s.pipeline(pipeline.Pipeline)
	s.input, s.aggregationAllowed = previousInput, previousAggregation
	if relation.Query == nil {
		return boundExpr{}
	}
	if relation.Schema.Unknown || len(relation.Schema.Columns) == 0 {
		s.bindError("KQLB0414", span, feature, op+" with a tabular expression requires a known first column")
		return boundExpr{}
	}
	if len(relation.rowDependencies) != 0 {
		s.bindError("KQLB0414", span, feature, op+" subqueries cannot reference the outer row")
		return boundExpr{}
	}
	result := sqlast.Expr(&sqlast.InSubquery{Value: left.SQL, Query: relation.Query, Column: relation.Schema.Columns[0].Name, CaseInsensitive: strings.HasSuffix(op, "~")})
	if strings.HasPrefix(op, "!") {
		result = &sqlast.Unary{Operator: "NOT ", Operand: result}
	}
	return boundExpr{SQL: result, Type: TypeBool}
}
