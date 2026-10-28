package kql

func parseQueryParameters(tokens []Token) (Statement, []ParseError) {
	statement := &QueryParametersStatement{Span: tokensSpan(tokens)}
	fail := func(message string, span Span) (Statement, []ParseError) {
		return statement, []ParseError{{Code: "KQLP0120", Message: message, Span: span}}
	}
	if len(tokens) < 4 || tokens[2].Text != "(" || matchingClose(tokens, 2) != len(tokens)-1 {
		return fail("declare query_parameters requires one parenthesized declaration list", statement.Span)
	}
	parts := splitTopLevel(tokens[3:len(tokens)-1], ",")
	if len(parts) > 64 {
		return fail("query parameter declaration exceeds 64 parameters", statement.Span)
	}
	for _, part := range parts {
		colon := topLevelToken(part, ":")
		if colon < 1 || colon+1 >= len(part) {
			return fail("query parameter requires name:type", statement.Span)
		}
		name, valid := declarationName(part[:colon])
		if !valid {
			return fail("query parameter requires an identifier declaration", tokensSpan(part))
		}
		parameter := FunctionParameter{Name: name, Span: tokensSpan(part)}
		typeTokens := part[colon+1:]
		if equal := topLevelToken(typeTokens, "="); equal >= 0 {
			if equal+1 == len(typeTokens) {
				return fail("query parameter default requires a literal", parameter.Span)
			}
			var errs []ParseError
			parameter.Default, errs = parseExpressionTokens(typeTokens[equal+1:])
			if len(errs) > 0 {
				return statement, errs
			}
			typeTokens = typeTokens[:equal]
		}
		if len(typeTokens) != 1 || typeTokens[0].Kind != IdentifierToken {
			return fail("query parameter requires a scalar type name", parameter.Span)
		}
		parameter.Type = typeTokens[0].Text
		statement.Parameters = append(statement.Parameters, parameter)
	}
	return statement, nil
}
