package ksql

import (
	"fmt"
	"strings"

	"github.com/kawijayaa/ksql/kql"
)

// expandArraySorts spreads array_sort_asc/array_sort_desc over several
// arrays into one column per array, as Kusto projects them: unnamed calls
// name the columns <array>_sorted, and a tuple (a, b) = array_sort_asc(...)
// names them in order. An indexed call such as array_sort_asc(t, c)[1] keeps
// the array of sorted arrays.
func expandArraySorts(expressions []kql.Expression) []kql.Expression {
	var result []kql.Expression
	for _, expression := range expressions {
		var names []string
		value := expression
		switch named := expression.(type) {
		case *kql.CompoundNamedExpression:
			names, value = named.Names, named.Value
		case *kql.NamedExpression:
			result = append(result, expression)
			continue
		}
		call, ok := value.(*kql.CallExpression)
		function := ""
		if ok {
			if name, isName := call.Function.(*kql.NameExpression); isName {
				function = strings.ToLower(unquoteKQL(name.Name))
			}
		}
		if function != "array_sort_asc" && function != "array_sort_desc" {
			result = append(result, expression)
			continue
		}
		arrays := len(call.Args)
		if arrays > 0 {
			if literal, ok := call.Args[arrays-1].(*kql.LiteralExpression); ok && (strings.EqualFold(literal.Text, "true") || strings.EqualFold(literal.Text, "false")) {
				arrays--
			}
		}
		if arrays < 2 || (len(names) > 0 && len(names) != arrays) {
			result = append(result, expression)
			continue
		}
		for i := 0; i < arrays; i++ {
			name := ""
			if len(names) > 0 {
				name = names[i]
			} else if inferred := inferredName(call.Args[i], ""); inferred != "" {
				name = inferred + "_sorted"
			} else {
				name = fmt.Sprintf("array%d_sorted", i+1)
			}
			element := &kql.IndexExpression{Value: call, Index: &kql.LiteralExpression{Kind: kql.NumberToken, Text: fmt.Sprint(i)}, Span: call.Span}
			result = append(result, &kql.NamedExpression{Name: name, Value: element, Span: call.Span})
		}
	}
	return result
}
