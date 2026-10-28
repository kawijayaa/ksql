package ksql

import (
	"fmt"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

func (s *compileState) call(value *kql.CallExpression) (result boundExpr) {
	name, ok := value.Function.(*kql.NameExpression)
	if !ok {
		s.error("KQLL0501", value.Span, "function.dynamic", "dynamic function references are not supported")
		return boundExpr{}
	}
	function := strings.ToLower(unquoteKQL(name.Name))
	if declaration, ok := s.functions[function]; ok {
		return s.callFunction(declaration, value)
	}
	if function == "materialize" {
		s.bindError("KQLB0524", value.Span, "function.materialize", "materialize returns a table and cannot be used as a scalar expression")
		return boundExpr{}
	}
	if function == "toscalar" {
		return s.toScalar(value)
	}
	if function == "column_names_of" {
		return s.columnNamesOf(value)
	}
	if function == "pack_all" {
		return s.packAll(value)
	}
	if function == "string" || function == "guid" {
		// string(null) and guid(null) are typed null literals.
		if len(value.Args) == 1 {
			if literal, ok := value.Args[0].(*kql.LiteralExpression); ok && literal.Kind != kql.StringToken && strings.EqualFold(literal.Text, "null") {
				return boundExpr{SQL: &sqlast.Literal{Kind: sqlast.NullLiteral}, Type: TypeString}
			}
		}
		s.bindError("KQLB0413", value.Span, "function."+function, function+"() takes null; use tostring() to convert values")
		return boundExpr{}
	}
	if function == "typeof" {
		// typeof(T) is a type literal, kept as an unbound string so
		// application rules such as extract can read the requested type.
		var name *kql.NameExpression
		ok := len(value.Args) == 1
		if ok {
			name, ok = value.Args[0].(*kql.NameExpression)
		}
		if !ok {
			s.bindError("KQLB0413", value.Span, "function.typeof", "typeof requires one scalar type name")
			return boundExpr{}
		}
		kind := parseScalarType(name.Name)
		if kind == TypeUnknown && !strings.EqualFold(name.Name, "guid") && !strings.EqualFold(name.Name, "decimal") {
			s.bindError("KQLB0413", value.Span, "function.typeof", "unknown scalar type "+name.Name)
			return boundExpr{}
		}
		return boundExpr{SQL: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: strings.ToLower(name.Name)}, Type: TypeString}
	}
	if function == "timespan" || function == "time" {
		if len(value.Args) != 1 {
			return s.arity(value, function, 1)
		}
		if literal, ok := value.Args[0].(*kql.LiteralExpression); ok && literal.Kind != kql.StringToken && strings.EqualFold(literal.Text, "null") {
			return s.timespanLiteral("null", value.Span)
		}
		return s.timespanLiteral(expressionText(value.Args[0]), value.Span)
	}
	if function == "datetime" || function == "date" {
		if len(value.Args) != 1 {
			return s.arity(value, function, 1)
		}
		if literal, ok := value.Args[0].(*kql.LiteralExpression); ok && literal.Kind != kql.StringToken && strings.EqualFold(literal.Text, "null") {
			kind := TypeUnknown
			if function == "datetime" || function == "date" {
				kind = TypeDateTime
			}
			return boundExpr{SQL: &sqlast.Literal{Kind: sqlast.NullLiteral}, Type: kind}
		}
		text := expressionText(value.Args[0])
		if s.compiler.parameterization == BoundParameters {
			resultType := TypeUnknown
			if function == "datetime" || function == "date" {
				resultType = TypeDateTime
			}
			return boundExpr{SQL: s.bind(text), Type: resultType}
		}
		kind, resultType := sqlast.IntervalLiteral, TypeUnknown
		if function == "datetime" || function == "date" {
			kind, resultType = sqlast.DateTimeLiteral, TypeDateTime
		}
		return boundExpr{SQL: &sqlast.Literal{Kind: kind, Value: text}, Type: resultType}
	}
	if _, _, window := s.isWindowCall(value); window {
		s.bindError("KQLB0391", value.Span, "function."+function, function+" is a window function; use it in extend, project, serialize or where over a serialized row set")
		return boundExpr{}
	}
	if s.isAggregate(function) {
		if !s.aggregationAllowed {
			s.bindError("KQLB0511", value.Span, "aggregate."+function, function+" requires a summarize aggregation context")
			return boundExpr{}
		}
		if s.compiler.functionRules[function] == nil && s.compiler.typedFunctionRules[function] == nil {
			if isCoreAggregate(function) && !s.aggregateArity(value, function) {
				return boundExpr{}
			}
			if function == "make_list" && len(value.Args) != 1 {
				return s.arity(value, function, 1)
			}
		}
		defer func() {
			if result.SQL != nil {
				if s.aggregateExpressions == nil {
					s.aggregateExpressions = make(map[sqlast.Expr]struct{})
				}
				s.aggregateExpressions[result.SQL] = struct{}{}
			}
		}()
		previous := s.aggregationAllowed
		s.aggregationAllowed = false
		defer func() { s.aggregationAllowed = previous }()
	}
	if s.compiler.functionRules[function] == nil && s.compiler.typedFunctionRules[function] == nil {
		// Reject bounded built-ins before expanding their arguments. Explicit
		// extensions keep precedence and their own signature policies.
		n := len(value.Args)
		switch function {
		case "coalesce":
			if n < 2 || n > 64 {
				s.bindError("KQLB0501", value.Span, "function.coalesce", "coalesce requires 2 to 64 arguments")
				return boundExpr{}
			}
		case "case":
			if n < 3 || n%2 == 0 || n > 129 {
				s.bindError("KQLB0502", value.Span, "function.case", "case requires 1 to 64 condition/result pairs and an else expression")
				return boundExpr{}
			}
		case "iff", "iif":
			if n != 3 {
				return s.arity(value, function, 3)
			}
		case "bool", "boolean":
			if n != 1 {
				return s.arity(value, function, 1)
			}
		}
	}
	args := make([]sqlast.Expr, 0, len(value.Args))
	argTypes := make([]ScalarType, 0, len(value.Args))
	firstName := ""
	if firstArgumentResultName(function) {
		defer func() {
			if result.SQL != nil {
				result.Name = firstName
			}
		}()
	}
	arguments := value.Args
	if function == "pack_array" && len(arguments) == 1 {
		if _, ok := arguments[0].(*kql.StarExpression); ok {
			if s.aggregationAllowed {
				s.bindError("KQLB0513", value.Span, "operator.summarize", "pack_array(*) cannot read input columns outside an aggregate function")
				return boundExpr{}
			}
			if s.input.Unknown || len(s.input.Columns) == 0 {
				s.bindError("KQLB0512", value.Span, "function.pack_array", "pack_array(*) requires a known non-empty input schema")
				return boundExpr{}
			}
			for _, column := range s.input.Columns {
				args = append(args, s.columnIdentifier(column.Name))
				argTypes = append(argTypes, column.Type)
			}
			arguments = nil
		}
	}
	argumentFailed := false
	for _, argument := range arguments {
		bound := s.expression(argument)
		if bound.SQL != nil {
			if len(args) == 0 {
				firstName = inferredName(argument, bound.Name)
			}
			args = append(args, bound.SQL)
			argTypes = append(argTypes, bound.Type)
		} else {
			argumentFailed = true
		}
	}
	if argumentFailed {
		return boundExpr{}
	}
	s.use("function."+function, "equivalent", value.Span)
	if rule, ok := s.compiler.typedFunctionRules[function]; ok {
		typed := make([]TypedExpression, len(args))
		for i := range args {
			typed[i] = TypedExpression{SQL: args[i], Type: argTypes[i]}
		}
		result, err := rule(typed)
		if err != nil {
			s.error("KQLL0504", value.Span, "function."+function, err.Error())
			return boundExpr{}
		}
		if result.SQL == nil {
			s.error("KQLL0505", value.Span, "function."+function, "custom function rule returned a nil expression")
			return boundExpr{}
		}
		if result.Type == "" {
			result.Type = TypeUnknown
		}
		return boundExpr{SQL: result.SQL, Type: result.Type}
	}
	if rule, ok := s.compiler.functionRules[function]; ok {
		result, err := rule(args)
		if err != nil {
			s.error("KQLL0504", value.Span, "function."+function, err.Error())
			return boundExpr{}
		}
		if result == nil {
			s.error("KQLL0505", value.Span, "function."+function, "custom function rule returned a nil expression")
		}
		return boundExpr{SQL: result, Type: functionType(function, argTypes)}
	}
	switch function {
	case "bool", "boolean":
		if len(args) != 1 {
			return s.arity(value, function, 1)
		}
		if argTypes[0] != TypeBool && !isNullExpression(args[0]) {
			s.bindError("KQLB0502", value.Span, "function."+function, "Boolean literals require true, false or null; use tobool for conversions")
			return boundExpr{}
		}
		return boundExpr{SQL: args[0], Type: TypeBool}
	case "not":
		if len(args) != 1 {
			return s.arity(value, function, 1)
		}
		if argTypes[0] != TypeBool && argTypes[0] != TypeUnknown {
			s.bindError("KQLB0502", value.Span, "function.not", "not requires a Boolean expression")
			return boundExpr{}
		}
		return boundExpr{SQL: &sqlast.Unary{Operator: "NOT ", Operand: args[0]}, Type: TypeBool}

	case "iif", "iff":
		if len(args) != 3 {
			return s.arity(value, function, 3)
		}
		if !s.booleanArgument(value, function, argTypes[0]) {
			return boundExpr{}
		}
		branches, typ, ok := s.commonBranches(value, function, args[1:], argTypes[1:], true)
		if !ok {
			return boundExpr{}
		}
		return boundExpr{SQL: &sqlast.Case{Branches: []sqlast.When{{Condition: args[0], Result: branches[0]}}, Else: branches[1]}, Type: typ}
	case "case":
		if len(args) < 3 || len(args)%2 == 0 || len(args) > 129 {
			s.error("KQLB0502", value.Span, "function.case", "case requires 1 to 64 condition/result pairs and an else expression")
			return boundExpr{}
		}
		values, types := []sqlast.Expr{}, []ScalarType{}
		for i := 0; i < len(args)-1; i += 2 {
			if !s.booleanArgument(value, function, argTypes[i]) {
				return boundExpr{}
			}
			values, types = append(values, args[i+1]), append(types, argTypes[i+1])
		}
		values, types = append(values, args[len(args)-1]), append(types, argTypes[len(args)-1])
		branches, typ, ok := s.commonBranches(value, function, values, types, true)
		if !ok {
			return boundExpr{}
		}
		result := &sqlast.Case{Else: branches[len(branches)-1]}
		for i := 0; i < len(args)-1; i += 2 {
			result.Branches = append(result.Branches, sqlast.When{Condition: args[i], Result: branches[i/2]})
		}
		return boundExpr{SQL: result, Type: typ}
	case "coalesce":
		if len(args) < 2 || len(args) > 64 {
			s.bindError("KQLB0501", value.Span, "function.coalesce", "coalesce requires 2 to 64 arguments")
			return boundExpr{}
		}
		branches, typ, ok := s.commonBranches(value, function, args, argTypes, false)
		if !ok {
			return boundExpr{}
		}
		if typ == TypeString {
			for i, expr := range branches {
				// NULLIF evaluates its argument once, including volatile functions.
				branches[i] = &sqlast.Call{Name: "nullif", Args: []sqlast.Expr{expr, &sqlast.Literal{Kind: sqlast.StringLiteral, Value: ""}}}
			}
			// KQL strings have an empty-string missing value rather than null.
			branches = append(branches, &sqlast.Literal{Kind: sqlast.StringLiteral, Value: ""})
		}
		return boundExpr{SQL: &sqlast.Call{Name: "coalesce", Args: branches}, Type: typ}
	case "int", "int32", "long", "int64", "real", "double", "decimal":
		if len(args) != 1 {
			return s.arity(value, function, 1)
		}
		types := map[string]string{"int": "INTEGER", "int32": "INTEGER", "long": "BIGINT", "int64": "BIGINT", "real": "DOUBLE PRECISION", "double": "DOUBLE PRECISION", "decimal": "DECIMAL"}
		return boundExpr{SQL: &sqlast.Cast{Expr: args[0], Type: types[function]}, Type: functionType(function, argTypes)}
	case "tostring":
		if len(args) != 1 {
			return s.arity(value, function, 1)
		}
		return boundExpr{SQL: textOf(args[0], argTypes[0]), Type: TypeString}
	case "toint", "tolong", "toreal", "todouble", "todatetime", "todecimal", "tobool", "toboolean", "toguid":
		if len(args) != 1 {
			return s.arity(value, function, 1)
		}
		types := map[string]string{"toint": "INTEGER", "tolong": "BIGINT", "toreal": "DOUBLE PRECISION", "todouble": "DOUBLE PRECISION", "todatetime": "TIMESTAMP", "todecimal": "DECIMAL", "tobool": "BOOLEAN", "toboolean": "BOOLEAN", "toguid": "UUID"}
		return boundExpr{SQL: &sqlast.Cast{Expr: args[0], Type: types[function], Safe: true}, Type: functionType(function, argTypes)}
	case "strcat":
		// strcat converts each argument with tostring, so nulls are empty.
		parts := make([]sqlast.Expr, len(args))
		for i, arg := range args {
			parts[i] = textOf(arg, argTypes[i])
		}
		return boundExpr{SQL: &sqlast.Call{Name: "concat", Args: parts}, Type: TypeString}
	case "strlen":
		return boundExpr{SQL: &sqlast.Call{Name: "length", Args: args}, Type: TypeLong}
	case "tolower":
		return boundExpr{SQL: &sqlast.Call{Name: "lower", Args: args}, Type: TypeString}
	case "toupper":
		return boundExpr{SQL: &sqlast.Call{Name: "upper", Args: args}, Type: TypeString}
	case "substring":
		return boundExpr{SQL: &sqlast.Call{Name: "substr", Args: args}, Type: TypeString}
	case "count", "countif", "sum", "sumif", "avg", "avgif", "min", "minif", "max", "maxif":
		return s.coreAggregate(value, function, args, argTypes)
	case "make_list":
		if len(args) != 1 {
			return s.arity(value, function, 1)
		}
		name := "json_group_array"
		if s.compiler.dialect.Name() == "postgresql" {
			name = "json_agg"
		} else if s.compiler.dialect.Name() != "sqlite" {
			s.error("KQLL0503", value.Span, "aggregate.make_list", "make_list requires an explicit dialect mapping")
			return boundExpr{}
		}
		element := args[0]
		if name == "json_group_array" {
			element = sqliteJSONElement(args[0], argTypes[0])
		}
		call := &sqlast.Call{Name: name, Args: []sqlast.Expr{element}}
		return boundExpr{SQL: &sqlast.Filtered{Expr: call, Where: &sqlast.Binary{Left: args[0], Operator: "IS NOT", Right: &sqlast.Literal{Kind: sqlast.NullLiteral}}}, Type: TypeDynamic}
	case "dcount":
		s.error("KQLL0510", value.Span, "aggregate.dcount", "dcount is approximate in KQL and requires an explicit compatible aggregate mapping")
		return boundExpr{}
	case "arg_min", "arg_max", "argmin", "argmax", "take_any", "any", "take_anyif", "anyif":
		s.bindError("KQLB0511", value.Span, "aggregate."+function, function+" requires a summarize aggregation context")
		return boundExpr{}
	case "abs", "ceiling", "ceil", "floor", "round", "pow", "sqrt", "exp", "ln", "log", "log10", "sin", "cos", "tan", "asin", "acos", "atan", "atan2", "sign":
		name := function
		if name == "log" {
			// KQL log is the natural logarithm; SQL log is base 10.
			name = "ln"
		}
		return boundExpr{SQL: &sqlast.Call{Name: name, Args: args}, Type: functionType(function, argTypes)}
	default:
		s.error("KQLL0503", value.Span, "function."+function, "function "+function+" is recognized syntactically but has no safe SQL mapping")
		return boundExpr{}
	}
}

func (s *compileState) arity(value *kql.CallExpression, function string, expected int) boundExpr {
	s.error("KQLB0501", value.Span, "function."+function, fmt.Sprintf("%s requires %d arguments", function, expected))
	return boundExpr{}
}

func functionType(function string, arguments []ScalarType) ScalarType {
	switch function {
	case "int", "int32", "toint":
		return TypeInt
	case "long", "int64", "tolong", "strlen", "count", "countif":
		return TypeLong
	case "real", "double", "decimal", "toreal", "todouble", "todecimal", "avg", "avgif":
		return TypeReal
	case "tostring", "strcat", "tolower", "toupper", "substring", "toguid":
		return TypeString
	case "datetime", "date", "todatetime":
		return TypeDateTime
	case "not", "tobool", "toboolean":
		return TypeBool
	case "make_list", "make_set", "make_bag", "make_bag_if", "make_dictionary", "parse_json", "parsejson":
		return TypeDynamic
	case "sqrt", "exp", "ln", "log", "log10", "sin", "cos", "tan", "asin", "acos", "atan", "atan2", "pow":
		return TypeReal
	}
	if len(arguments) > 0 {
		if (function == "sum" || function == "sumif") && arguments[0] == TypeInt {
			return TypeLong
		}
		switch function {
		case "sum", "sumif", "min", "minif", "max", "maxif", "coalesce", "abs", "ceiling", "ceil", "floor", "round", "sign":
			return arguments[0]
		}
	}
	return TypeUnknown
}

// textOf converts a value as KQL tostring does: Booleans are true/false,
// timespans use [-][d.]hh:mm:ss[.fffffff], and null becomes an empty string.
func textOf(value sqlast.Expr, kind ScalarType) sqlast.Expr {
	text := sqlast.Expr(&sqlast.ScalarText{Value: value})
	switch kind {
	case TypeString:
		text = value
	case TypeTimespan:
		text = &sqlast.TimespanText{Value: value}
	case TypeBool:
		text = &sqlast.Case{Branches: []sqlast.When{
			{Condition: value, Result: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: "true"}},
			{Condition: &sqlast.Unary{Operator: "NOT ", Operand: value}, Result: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: "false"}},
		}}
	}
	return &sqlast.Call{Name: "coalesce", Args: []sqlast.Expr{text, &sqlast.Literal{Kind: sqlast.StringLiteral, Value: ""}}}
}

// sqliteJSONElement encodes a value for json_group_array: dynamic JSON text is
// embedded as JSON rather than a string, Booleans as true/false and timespans
// as Kusto text.
func sqliteJSONElement(value sqlast.Expr, kind ScalarType) sqlast.Expr {
	switch kind {
	case TypeDynamic:
		return &sqlast.Case{Branches: []sqlast.When{{Condition: &sqlast.Call{Name: "json_valid", Args: []sqlast.Expr{value}}, Result: &sqlast.Call{Name: "json", Args: []sqlast.Expr{value}}}}, Else: value}
	case TypeBool:
		return &sqlast.Case{Branches: []sqlast.When{
			{Condition: value, Result: &sqlast.Call{Name: "json", Args: []sqlast.Expr{&sqlast.Literal{Kind: sqlast.StringLiteral, Value: "true"}}}},
		}, Else: &sqlast.Call{Name: "json", Args: []sqlast.Expr{&sqlast.Literal{Kind: sqlast.StringLiteral, Value: "false"}}}}
	case TypeTimespan:
		return &sqlast.TimespanText{Value: value}
	}
	return value
}
