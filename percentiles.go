package ksql

import (
	"strings"

	"github.com/kawijayaa/ksql/kql"
)

// expandPercentiles rewrites summarize's percentiles(x, p1, ...) and
// percentilesw(x, w, p1, ...) into one percentile(x, p) or
// percentilew(x, w, p) column per percentile, and names unnamed percentile
// columns as Kusto does: percentile_x_50, percentile_x_99_9, percentiles_x
// for percentiles_array and percentile_x for percentilesw_array. A tuple
// name (p50, p95) = percentiles(...) names the columns in order.
func (s *compileState) expandPercentiles(expressions []kql.Expression) []kql.Expression {
	result := make([]kql.Expression, 0, len(expressions))
	for _, expression := range expressions {
		var names []string
		value := expression
		switch named := expression.(type) {
		case *kql.NamedExpression:
			names, value = []string{named.Name}, named.Value
		case *kql.CompoundNamedExpression:
			names, value = named.Names, named.Value
		}
		call, ok := value.(*kql.CallExpression)
		var function string
		if ok {
			if name, isName := call.Function.(*kql.NameExpression); isName {
				function = strings.ToLower(unquoteKQL(name.Name))
			}
		}
		fixed := map[string]int{"percentile": 1, "percentiles": 1, "percentilew": 2, "percentilesw": 2}[function]
		switch {
		case fixed > 0 && len(call.Args) > fixed:
			single := map[string]string{"percentile": "percentile", "percentiles": "percentile", "percentilew": "percentilew", "percentilesw": "percentilew"}[function]
			points := call.Args[fixed:]
			if len(names) > 0 && len(names) != len(points) {
				s.bindError("KQLB0514", call.Span, "aggregate."+function, function+" names must match its percentiles")
				return nil
			}
			for i, point := range points {
				args := append(append([]kql.Expression{}, call.Args[:fixed]...), point)
				expanded := &kql.CallExpression{Function: &kql.NameExpression{Name: single, Span: call.Span}, Args: args, Span: call.Span}
				name := percentileName(call.Args[0], point)
				if len(names) > 0 {
					name = names[i]
				}
				result = append(result, &kql.NamedExpression{Name: name, Value: expanded, Span: call.Span})
			}
		case len(names) == 0 && (function == "percentiles_array" || function == "percentilesw_array") && len(call.Args) > 0:
			prefix := "percentiles_"
			if function == "percentilesw_array" {
				prefix = "percentile_"
			}
			result = append(result, &kql.NamedExpression{Name: prefix + inferredName(call.Args[0], ""), Value: call, Span: call.Span})
		default:
			result = append(result, expression)
		}
	}
	return result
}

func percentileName(value, point kql.Expression) string {
	text := ""
	switch p := point.(type) {
	case *kql.LiteralExpression:
		text = p.Text
	case *kql.UnaryExpression:
		if literal, ok := p.Operand.(*kql.LiteralExpression); ok {
			text = literal.Text
		}
	case *kql.NameExpression:
		text = unquoteKQL(p.Name)
	}
	return "percentile_" + inferredName(value, "") + "_" + strings.ReplaceAll(text, ".", "_")
}
