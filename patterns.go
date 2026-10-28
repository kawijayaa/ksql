package ksql

import (
	"strings"

	"github.com/kawijayaa/ksql/kql"
)

// maxPatternDepth bounds patterns whose entries invoke patterns.
const maxPatternDepth = 16

func (s *compileState) declarePattern(statement *kql.PatternStatement) {
	if s.patterns == nil {
		s.patterns = map[string]*kql.PatternStatement{}
	}
	key := strings.ToLower(statement.Name)
	if _, exists := s.patterns[key]; exists {
		s.bindError("KQLB0106", statement.Span, "statement.pattern", "pattern "+statement.Name+" is already declared")
		return
	}
	s.patterns[key] = statement
}

// patternInvocation reads Name(args) or Name(args).Path / .["Path"].
func (s *compileState) patternInvocation(expression kql.Expression) (*kql.PatternStatement, []string, string, bool, bool) {
	path, hasPath := "", false
	if index, ok := expression.(*kql.IndexExpression); ok {
		literal, isLiteral := index.Index.(*kql.LiteralExpression)
		if !isLiteral {
			return nil, nil, "", false, false
		}
		path, hasPath, expression = stringLiteralValue(literal), true, index.Value
		if literal.Kind != kql.StringToken {
			path = literal.Text
		}
	}
	call, ok := expression.(*kql.CallExpression)
	if !ok {
		return nil, nil, "", false, false
	}
	name, ok := call.Function.(*kql.NameExpression)
	if !ok {
		return nil, nil, "", false, false
	}
	pattern := s.patterns[strings.ToLower(unquoteKQL(name.Name))]
	if pattern == nil {
		return nil, nil, "", false, false
	}
	var args []string
	for _, arg := range call.Args {
		literal, ok := arg.(*kql.LiteralExpression)
		if !ok || literal.Kind != kql.StringToken {
			s.bindError("KQLB0107", arg.NodeSpan(), "statement.pattern", "pattern arguments must be string literals")
			return pattern, nil, "", false, true
		}
		args = append(args, stringLiteralValue(literal))
	}
	return pattern, args, path, hasPath, true
}

// patternSource lowers a pattern invocation to its entry's query; a missing
// entry reports Kusto's error naming the reference, which applications use
// to supply definitions.
func (s *compileState) patternSource(expression kql.Expression, span kql.Span) (Relation, bool) {
	pattern, args, path, hasPath, ok := s.patternInvocation(expression)
	if !ok {
		return Relation{}, false
	}
	if args == nil && len(pattern.Params) > 0 {
		return Relation{}, true
	}
	for _, entry := range pattern.Entries {
		if len(entry.Args) != len(args) || hasPath != pattern.HasPath || (hasPath && entry.Path != path) {
			continue
		}
		matched := true
		for i := range args {
			matched = matched && entry.Args[i] == args[i]
		}
		if !matched {
			continue
		}
		if s.patternDepth >= maxPatternDepth {
			s.bindError("KQLB0108", span, "statement.pattern", "pattern invocations nest too deeply")
			return Relation{}, true
		}
		s.patternDepth++
		defer func() { s.patternDepth-- }()
		return s.pipeline(entry.Body), true
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "\\'") + "'"
	}
	reference := pattern.Name + "(" + strings.Join(quoted, ", ") + ")"
	if hasPath {
		reference += ".['" + strings.ReplaceAll(path, "'", "\\'") + "']"
	}
	s.bindError("KQLB0109", span, "statement.pattern", `One or more pattern references were not declared. Detected pattern references: ["`+reference+`"]`)
	return Relation{}, true
}
