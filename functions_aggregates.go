package ksql

import (
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

func isCoreAggregate(name string) bool {
	switch name {
	case "count", "countif", "sum", "sumif", "avg", "avgif", "min", "minif", "max", "maxif":
		return true
	}
	return false
}

func (s *compileState) aggregateItems(expressions []kql.Expression, schema Schema) ([]sqlast.SelectItem, Schema) {
	previous := s.aggregationAllowed
	s.aggregationAllowed = true
	defer func() { s.aggregationAllowed = previous }()
	return s.projectItems(expressions, true, schema, false)
}

func (s *compileState) aggregateArity(call *kql.CallExpression, name string) bool {
	for _, arg := range call.Args {
		if _, star := arg.(*kql.StarExpression); star {
			s.bindError("KQLB0502", call.Span, "aggregate."+name, name+" requires scalar arguments; use count() to count records")
			return false
		}
	}
	n := len(call.Args)
	if name == "count" {
		if n <= 1 { // The predicate overload is deprecated in favor of countif.
			return true
		}
		s.bindError("KQLB0501", call.Span, "aggregate.count", "count requires zero arguments or one Boolean predicate (deprecated; use countif)")
		return false
	}
	expected := 1
	if strings.HasSuffix(name, "if") && name != "countif" {
		expected = 2
	}
	if n != expected {
		s.arity(call, name, expected)
		return false
	}
	return true
}

func (s *compileState) coreAggregate(call *kql.CallExpression, name string, args []sqlast.Expr, types []ScalarType) boundExpr {
	// A failed argument binding must never shift value/predicate positions.
	if len(args) != len(call.Args) {
		return boundExpr{}
	}
	if name == "count" || name == "countif" {
		arg := sqlast.Expr(&sqlast.Star{})
		if len(args) == 1 {
			if !s.booleanArgument(call, name, types[0]) {
				return boundExpr{}
			}
			// COUNT ignores the null produced for false/null predicates and
			// yields an exact long zero even for an empty input relation.
			arg = &sqlast.Case{Branches: []sqlast.When{{Condition: args[0], Result: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "1"}}}}
		}
		return boundExpr{SQL: &sqlast.Call{Name: "count", Args: []sqlast.Expr{arg}}, Type: TypeLong}
	}
	base := strings.TrimSuffix(name, "if")
	typ := types[0]
	if base == "sum" || base == "avg" {
		switch typ {
		case TypeInt, TypeLong, TypeReal, TypeUnknown:
		case TypeDateTime:
			s.error("KQLL0512", call.Span, "aggregate."+name, name+" datetime overload requires a compatible temporal runtime")
			return boundExpr{}
		default:
			s.bindError("KQLB0502", call.Span, "aggregate."+name, name+" requires a numeric expression or a supported temporal overload")
			return boundExpr{}
		}
	} else if typ == TypeDynamic {
		s.bindError("KQLB0502", call.Span, "aggregate."+name, name+" requires an orderable scalar expression")
		return boundExpr{}
	}
	arg := args[0]
	if len(args) == 2 {
		if !s.booleanArgument(call, name, types[1]) {
			return boundExpr{}
		}
		arg = &sqlast.Case{Branches: []sqlast.When{{Condition: args[1], Result: arg}}}
	}
	if s.compiler.dialect.Name() == "postgresql" && typ == TypeBool {
		if base == "min" {
			base = "bool_and"
		} else if base == "max" {
			base = "bool_or"
		}
	}
	var result sqlast.Expr = &sqlast.Call{Name: base, Args: []sqlast.Expr{arg}}
	if base == "sum" {
		zero := sqlast.Expr(&sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "0"})
		if typ == TypeReal {
			zero = &sqlast.Cast{Expr: zero, Type: "DOUBLE PRECISION"}
		}
		result = &sqlast.Call{Name: "coalesce", Args: []sqlast.Expr{result, zero}}
	}
	return boundExpr{SQL: result, Type: functionType(name, types)}
}
