package ksql

import (
	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

func (s *compileState) booleanArgument(call *kql.CallExpression, function string, typ ScalarType) bool {
	if typ == TypeBool || typ == TypeUnknown {
		return true
	}
	s.bindError("KQLB0502", call.Span, "function."+function, function+" predicates must be Boolean expressions")
	return false
}

// Match the represented common-type rules in Microsoft's TypeFacts: numeric
// widening, null accommodation, and dynamic results for heterogeneous scalar
// conditional branches. Unknown-schema expressions remain unknown.
func (s *compileState) commonBranches(call *kql.CallExpression, function string, args []sqlast.Expr, types []ScalarType, conditional bool) ([]sqlast.Expr, ScalarType, bool) {
	common := TypeUnknown
	unknown, dynamic := false, false
	for i, typ := range types {
		if typ == TypeUnknown {
			if !isNullExpression(args[i]) {
				unknown = true
			}
			continue
		}
		if conditional && typ == TypeDynamic {
			dynamic = true
			continue
		}
		if common == TypeUnknown || common == typ {
			common = typ
			continue
		}
		if numericType(common) && numericType(typ) {
			if common == TypeReal || typ == TypeReal {
				common = TypeReal
			} else {
				common = TypeLong
			}
			continue
		}
		// String is wider than dynamic in Microsoft's common scalar type
		// model. Full dynamic-to-string conversion still needs tagged values.
		if !conditional && (common == TypeDynamic && typ == TypeString || common == TypeString && typ == TypeDynamic) {
			common = TypeString
			continue
		}
		if conditional {
			common = TypeDynamic
			continue
		}
		s.bindError("KQLB0506", call.Span, "function."+function, function+" arguments have no common scalar type")
		return nil, TypeUnknown, false
	}
	if conditional && dynamic && common != TypeUnknown && common != TypeDynamic {
		s.error("KQLL0506", call.Span, "function."+function, "mixed dynamic and scalar conditional branches require tagged dynamic conversion")
		return nil, TypeUnknown, false
	}
	if common == TypeUnknown && dynamic {
		common = TypeDynamic
	}
	if unknown {
		common = TypeUnknown
	}
	result := append([]sqlast.Expr(nil), args...)
	for i, typ := range types {
		if common == TypeReal && (typ == TypeInt || typ == TypeLong) {
			result[i] = &sqlast.Cast{Expr: args[i], Type: "DOUBLE PRECISION"}
		} else if common == TypeLong && typ == TypeInt {
			result[i] = &sqlast.Cast{Expr: args[i], Type: "BIGINT"}
		} else if common == TypeString && typ == TypeDynamic {
			// Retain legacy JSON/text compatibility until dynamic storage has
			// unambiguous tags for extracted versus encoded string values.
			result[i] = &sqlast.Cast{Expr: args[i], Type: "VARCHAR"}
		} else if common == TypeDynamic && typ != TypeDynamic && typ != TypeUnknown {
			if typ != TypeBool && typ != TypeString && !numericType(typ) {
				s.error("KQLL0506", call.Span, "function."+function, "dynamic conditional temporal payloads require tagged dynamic encoding")
				return nil, TypeUnknown, false
			}
			result[i] = &sqlast.DynamicValue{Value: args[i], Kind: string(typ)}
		}
	}
	return result, common, true
}

func numericType(typ ScalarType) bool { return typ == TypeInt || typ == TypeLong || typ == TypeReal }
