package ksql

import (
	"strings"

	"github.com/kawijayaa/ksql/kql"
)

// functionAliases are deprecated or alternate names Kusto documents as
// equivalent to another function.
var functionAliases = map[string]string{
	"array_strcat":   "strcat_array",
	"arraylength":    "array_length",
	"hash_xxhash64":  "hash",
	"columnifexists": "column_ifexists",
}

// constantString evaluates expression to a compile-time string, such as a
// literal or a function parameter bound to one. Arguments bound while
// evaluating are discarded.
func (s *compileState) constantString(expression kql.Expression) (string, bool) {
	saved, mark := len(s.args), len(s.diagnostics)
	defer func() { s.args = s.args[:saved] }()
	bound := s.expression(expression)
	if bound.SQL == nil || bound.Type != TypeString {
		s.diagnostics = s.diagnostics[:mark]
		return "", false
	}
	value, ok := s.constantValue(bound.SQL)
	text, isText := value.(string)
	return text, ok && isText
}

// columnIfExists binds column_ifexists(Name, Default): the column Name when
// the input has it, else Default. Name must be a constant string.
func (s *compileState) columnIfExists(call *kql.CallExpression) boundExpr {
	if len(call.Args) != 2 {
		return s.arity(call, "column_ifexists", 2)
	}
	name, ok := s.constantString(call.Args[0])
	if !ok && s.functionValidation {
		// A parameter's value is known per call; validate the default.
		return s.expression(call.Args[1])
	}
	if !ok {
		s.bindError("KQLB0413", call.Span, "function.column_ifexists", "column_ifexists requires a constant column name")
		return boundExpr{}
	}
	if _, _, found, _ := s.input.Lookup(name); found || (s.input.Unknown && name != "") {
		return s.expression(&kql.NameExpression{Name: searchName(name), Span: call.Args[0].NodeSpan()})
	}
	return s.expression(call.Args[1])
}

// bagPackColumns binds bag_pack_columns(c1, c2, ...) as
// bag_pack("c1", c1, "c2", c2, ...).
func (s *compileState) bagPackColumns(call *kql.CallExpression) boundExpr {
	if len(call.Args) == 0 {
		s.bindError("KQLB0413", call.Span, "function.bag_pack_columns", "bag_pack_columns requires at least one column")
		return boundExpr{}
	}
	var args []kql.Expression
	for _, arg := range call.Args {
		name, ok := arg.(*kql.NameExpression)
		if !ok {
			s.bindError("KQLB0413", arg.NodeSpan(), "function.bag_pack_columns", "bag_pack_columns arguments must be columns")
			return boundExpr{}
		}
		args = append(args, searchString(lastName(unquoteKQL(name.Name))), arg)
	}
	return s.expression(&kql.CallExpression{Function: &kql.NameExpression{Name: "bag_pack"}, Args: args, Span: call.Span})
}

// tableReference resolves table(Name [, DataScope]) and its peers to a
// table name. Name may be any compile-time string, such as a function
// parameter; the data scope only selects cache tiers, so it is validated
// and ignored.
func (s *compileState) tableReference(call *kql.CallExpression) (string, bool) {
	name, ok := call.Function.(*kql.NameExpression)
	if !ok || len(call.Args) < 1 || len(call.Args) > 2 {
		return "", false
	}
	function := strings.ToLower(name.Name)
	if function != "table" && function != "external_table" && function != "materialized_view" {
		return "", false
	}
	if len(call.Args) == 2 {
		scope, ok := s.constantString(call.Args[1])
		if !ok || function != "table" || (scope != "hotcache" && scope != "all" && scope != "default") {
			return "", false
		}
	}
	return s.constantString(call.Args[0])
}
