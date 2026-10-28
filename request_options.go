package ksql

import (
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// Request options set by set statements that change query results. Other
// options tune Kusto's execution and are accepted with a warning.
var effectiveRequestOptions = map[string]bool{
	"query_now": true, "query_bin_auto_size": true, "query_bin_auto_at": true,
	"query_take_max_records": true, "truncationmaxrecords": true,
}

func (s *compileState) setOption(statement *kql.SetStatement) {
	name := strings.ToLower(statement.Name)
	if s.requestOptions == nil {
		s.requestOptions = map[string]kql.Expression{}
	}
	s.requestOptions[name] = statement.Value
	if !effectiveRequestOptions[name] {
		s.warn("KQLL0104", statement.Span, "statement.set", "set "+statement.Name+" tunes Kusto execution and has no effect here")
	}
}

// requestNow returns now([offset]) or ago(span) under set query_now =
// datetime(...).
func (s *compileState) requestNow(call *kql.CallExpression, ago bool) (boundExpr, bool) {
	now, ok := s.requestOptions["query_now"]
	if !ok {
		return boundExpr{}, false
	}
	if ago && len(call.Args) != 1 {
		return s.arity(call, "ago", 1), true
	}
	value := now
	if len(call.Args) == 1 {
		operator := "+"
		if ago {
			operator = "-"
		}
		value = &kql.BinaryExpression{Left: now, Operator: operator, Right: call.Args[0], Span: call.Span}
	}
	bound := s.expression(value)
	if bound.SQL != nil && bound.Type != TypeDateTime && bound.Type != TypeUnknown {
		s.bindError("KQLB0105", call.Span, "statement.set", "query_now must be a datetime")
		return boundExpr{}, true
	}
	return bound, true
}

// binAuto lowers bin_auto(x) to bin(x, size) or bin_at(x, size, at) using
// set query_bin_auto_size and query_bin_auto_at.
func (s *compileState) binAuto(call *kql.CallExpression) boundExpr {
	if len(call.Args) != 1 {
		return s.arity(call, "bin_auto", 1)
	}
	size, ok := s.requestOptions["query_bin_auto_size"]
	if !ok {
		s.bindError("KQLB0105", call.Span, "function.bin_auto", "bin_auto requires set query_bin_auto_size")
		return boundExpr{}
	}
	args := []kql.Expression{call.Args[0], size}
	function := "bin"
	if at, ok := s.requestOptions["query_bin_auto_at"]; ok {
		args, function = append(args, at), "bin_at"
	}
	return s.expression(&kql.CallExpression{Function: &kql.NameExpression{Name: function, Span: call.Span}, Args: args, Span: call.Span})
}

// limitResults applies set query_take_max_records (or truncationmaxrecords)
// to the final result.
func (s *compileState) limitResults(relation Relation) Relation {
	limit, ok := s.requestOptions["query_take_max_records"]
	if !ok {
		limit, ok = s.requestOptions["truncationmaxrecords"]
	}
	if !ok || relation.Query == nil {
		return relation
	}
	bound := s.expression(limit)
	if bound.SQL == nil || (bound.Type != TypeLong && bound.Type != TypeInt) {
		s.bindError("KQLB0105", limit.NodeSpan(), "statement.set", "query_take_max_records must be an integer")
		return Relation{}
	}
	limited := relation
	limited.Query = &sqlast.Select{From: &sqlast.Subquery{Query: relation.Query, Alias: s.nextAlias()}, Limit: bound.SQL}
	return limited
}
