package kql

import "strings"

// parseSearchSpec parses [kind=CaseSensitivity] [in (Tables)] Predicate.
func parseSearchSpec(tokens []Token, span Span) (SearchSpec, []ParseError) {
	spec := SearchSpec{}
	fail := func(message string, at Span) (SearchSpec, []ParseError) {
		return spec, []ParseError{{Code: "KQLP0356", Message: message, Span: at}}
	}
	if len(tokens) >= 3 && lower(tokens[0]) == "kind" && tokens[1].Text == "=" {
		switch lower(tokens[2]) {
		case "default", "case_insensitive":
		case "case_sensitive":
			spec.Kind = "case_sensitive"
		default:
			return fail("search kind must be default, case_insensitive or case_sensitive", tokens[2].Span)
		}
		tokens = tokens[3:]
	}
	if len(tokens) >= 2 && lower(tokens[0]) == "in" && tokens[1].Text == "(" {
		close := matchingClose(tokens, 1)
		if close < 0 {
			return fail("search in requires a closing parenthesis", tokens[1].Span)
		}
		for _, part := range splitTopLevel(tokens[2:close], ",") {
			var name strings.Builder
			for _, token := range part {
				name.WriteString(token.Text)
			}
			if name.Len() == 0 {
				return fail("search in requires table names", tokensSpan(part))
			}
			spec.Tables = append(spec.Tables, name.String())
		}
		if len(spec.Tables) == 0 {
			return fail("search in requires table names", tokens[1].Span)
		}
		spec.HasTables = true
		tokens = tokens[close+1:]
	}
	if len(tokens) == 0 {
		return fail("search requires a predicate", span)
	}
	if len(tokens) == 1 && tokens[0].Text == "*" {
		spec.Term = &StarExpression{Span: tokens[0].Span}
		return spec, nil
	}
	term, errs := parseExpressionTokens(tokens)
	spec.Term = term
	return spec, errs
}
