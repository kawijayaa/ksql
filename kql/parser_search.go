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

// parseTableList parses "(" Name {"," Name} ")" starting at tokens[0],
// returning the names (with * patterns) and the tokens after ")".
func parseTableList(tokens []Token) ([]string, []Token, bool) {
	if len(tokens) < 2 || tokens[0].Text != "(" {
		return nil, tokens, false
	}
	close := matchingClose(tokens, 0)
	if close < 0 {
		return nil, tokens, false
	}
	var names []string
	for _, part := range splitTopLevel(tokens[1:close], ",") {
		var name strings.Builder
		for _, token := range part {
			name.WriteString(token.Text)
		}
		if name.Len() == 0 {
			return nil, tokens, false
		}
		names = append(names, name.String())
	}
	return names, tokens[close+1:], len(names) > 0
}

func parseFindSpec(tokens []Token, span Span) (FindSpec, []ParseError) {
	spec := FindSpec{WithSource: "source_"}
	fail := func(message string, at Span) (FindSpec, []ParseError) {
		return spec, []ParseError{{Code: "KQLP0357", Message: message, Span: at}}
	}
	if len(tokens) >= 3 && lower(tokens[0]) == "withsource" && tokens[1].Text == "=" {
		spec.WithSource, tokens = tokens[2].Text, tokens[3:]
	}
	if len(tokens) >= 1 && lower(tokens[0]) == "in" {
		names, rest, ok := parseTableList(tokens[1:])
		if !ok {
			return fail("find in requires a parenthesized list of table names", tokens[0].Span)
		}
		spec.Tables, spec.HasTables, tokens = names, true, rest
		if len(tokens) == 0 || lower(tokens[0]) != "where" {
			return fail("find in (...) requires where Predicate", span)
		}
	}
	if len(tokens) > 0 && lower(tokens[0]) == "where" {
		tokens = tokens[1:]
	}
	predicate := tokens
	if index := topLevelKeyword(tokens, "project-smart"); index >= 0 {
		if index+1 != len(tokens) {
			return fail("project-smart takes no columns", tokens[index].Span)
		}
		predicate = tokens[:index]
	} else if index := topLevelKeyword(tokens, "project"); index >= 0 {
		predicate = tokens[:index]
		spec.HasProject = true
		items := tokens[index+1:]
		if len(items) == 0 {
			return fail("find project requires columns", tokens[index].Span)
		}
		for _, item := range splitTopLevel(items, ",") {
			switch {
			case len(item) == 3 && lower(item[0]) == "pack_all" && item[1].Text == "(" && item[2].Text == ")":
				spec.PackAll = true
			case len(item) == 1:
				spec.Project = append(spec.Project, FindColumn{Name: item[0].Text, Span: item[0].Span})
			case len(item) == 3 && item[1].Text == ":":
				spec.Project = append(spec.Project, FindColumn{Name: item[0].Text, Type: lower(item[2]), Span: tokensSpan(item)})
			default:
				return fail("find project items are Column, Column:Type or pack_all()", tokensSpan(item))
			}
		}
	}
	if len(predicate) == 0 {
		return fail("find requires a predicate", span)
	}
	term, errs := parseExpressionTokens(predicate)
	spec.Term = term
	return spec, errs
}
