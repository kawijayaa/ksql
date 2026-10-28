package kql

import "strings"

const maxParsePatternItems = 256

// ParseItemKind identifies one element of a parse operator pattern.
type ParseItemKind uint8

const (
	ParseWildcard ParseItemKind = iota + 1
	ParseConstant
	ParseColumn
)

// ParseItem is a wildcard, decoded string constant, or extracted column.
// Column names keep their source spelling; Type is empty for string.
type ParseItem struct {
	Kind ParseItemKind
	Text string
	Name string
	Type string
	Span Span
}

// ParseSpec is the typed body of parse and parse-where.
type ParseSpec struct {
	Kind    string
	Flags   string
	Source  Expression
	Pattern []ParseItem
}

func (ParseSpec) operatorBody() {}

func parseParseOperator(op Operator, tokens []Token) (Operator, []ParseError) {
	spec := ParseSpec{Kind: "simple"}
	op.Body = spec
	fail := func(message string, span Span) (Operator, []ParseError) {
		return op, []ParseError{{Code: "KQLP0350", Message: message, Span: span}}
	}
	seenKind, seenFlags := false, false
	for len(tokens) >= 3 && tokens[1].Text == "=" && tokens[0].Kind == IdentifierToken {
		switch lower(tokens[0]) {
		case "kind":
			if seenKind || tokens[2].Kind != IdentifierToken {
				return fail(op.Kind+" kind requires one of simple, regex or relaxed", tokens[0].Span)
			}
			spec.Kind, seenKind = strings.ToLower(tokens[2].Text), true
			if spec.Kind != "simple" && spec.Kind != "regex" && spec.Kind != "relaxed" {
				return fail(op.Kind+" kind requires one of simple, regex or relaxed", tokens[2].Span)
			}
		case "flags":
			if seenFlags || !seenKind || tokens[2].Kind != IdentifierToken {
				return fail(op.Kind+" flags must follow kind and be regex flag letters", tokens[0].Span)
			}
			spec.Flags, seenFlags = tokens[2].Text, true
		default:
			return fail("unknown "+op.Kind+" property "+tokens[0].Text, tokens[0].Span)
		}
		tokens = tokens[3:]
	}
	with := topLevelKeyword(tokens, "with")
	if with <= 0 {
		return fail(op.Kind+" requires an expression followed by with and a pattern", op.Span)
	}
	source, errs := parseExpressionTokens(tokens[:with])
	if len(errs) > 0 {
		return op, errs
	}
	spec.Source = source
	pattern := tokens[with+1:]
	if len(pattern) == 0 {
		return fail(op.Kind+" pattern is empty", tokens[with].Span)
	}
	for i := 0; i < len(pattern); {
		token := pattern[i]
		if len(spec.Pattern) >= maxParsePatternItems {
			return fail(op.Kind+" pattern exceeds 256 items", token.Span)
		}
		switch {
		case token.Text == "*":
			spec.Pattern = append(spec.Pattern, ParseItem{Kind: ParseWildcard, Span: token.Span})
			i++
		case token.Kind == StringToken:
			start := i
			parts := []string{}
			for i < len(pattern) && pattern[i].Kind == StringToken {
				parts = append(parts, pattern[i].Text)
				i++
			}
			text, err := StringValue(&LiteralExpression{Parts: parts})
			if err != nil {
				return fail("invalid "+op.Kind+" string constant", pattern[start].Span)
			}
			spec.Pattern = append(spec.Pattern, ParseItem{Kind: ParseConstant, Text: text, Span: tokensSpan(pattern[start:i])})
		case token.Kind == IdentifierToken || token.Text == "[":
			end := i + 1
			if token.Text == "[" {
				for end < len(pattern) && pattern[end-1].Text != "]" {
					end++
				}
			}
			name, valid := declarationName(pattern[i:end])
			if !valid {
				return fail(op.Kind+" column requires an identifier", token.Span)
			}
			item := ParseItem{Kind: ParseColumn, Name: name, Span: tokensSpan(pattern[i:end])}
			i = end
			if i < len(pattern) && pattern[i].Text == ":" {
				if i+1 >= len(pattern) || pattern[i+1].Kind != IdentifierToken {
					return fail(op.Kind+" column type requires a scalar type name", pattern[i].Span)
				}
				item.Type = pattern[i+1].Text
				item.Span = tokensSpan(pattern[i-1 : i+2])
				i += 2
			}
			spec.Pattern = append(spec.Pattern, item)
		default:
			return fail("unexpected "+token.Text+" in "+op.Kind+" pattern", token.Span)
		}
	}
	op.Body = spec
	return op, nil
}
