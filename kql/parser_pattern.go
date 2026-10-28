package kql

// patternValue reads a string literal pattern argument or path value.
func patternValue(tokens []Token) (string, bool) {
	if len(tokens) != 1 {
		return "", false
	}
	if tokens[0].Kind == IdentifierToken {
		return tokens[0].Text, true
	}
	if tokens[0].Kind != StringToken {
		return "", false
	}
	value, err := DecodeString(tokens[0].Text)
	return value, err == nil
}

// parsePattern parses declare pattern Name [= (Arg:string, ...)[Path:string]
// { (values).[path] = { query }; ... }].
func parsePattern(tokens []Token) (Statement, []ParseError) {
	statement := &PatternStatement{Span: tokensSpan(tokens)}
	fail := func(message string, span Span) (Statement, []ParseError) {
		return statement, []ParseError{{Code: "KQLP0104", Message: message, Span: span}}
	}
	if len(tokens) < 3 || tokens[2].Kind != IdentifierToken {
		return fail("declare pattern requires a name", statement.Span)
	}
	statement.Name = tokens[2].Text
	rest := tokens[3:]
	if len(rest) == 0 {
		return statement, nil
	}
	if rest[0].Text != "=" || len(rest) < 3 || rest[1].Text != "(" {
		return fail("declare pattern requires = (Arg:string, ...) { ... }", rest[0].Span)
	}
	rest = rest[1:]
	close := matchingClose(rest, 0)
	if close < 0 {
		return fail("unclosed pattern parameters", rest[0].Span)
	}
	for _, part := range splitTopLevel(rest[1:close], ",") {
		if len(part) != 3 || part[0].Kind != IdentifierToken || part[1].Text != ":" || lower(part[2]) != "string" {
			return fail("pattern parameters are Name:string", tokensSpan(part))
		}
		statement.Params = append(statement.Params, part[0].Text)
	}
	if len(statement.Params) == 0 {
		return fail("a pattern requires at least one parameter", rest[0].Span)
	}
	rest = rest[close+1:]
	if len(rest) > 0 && rest[0].Text == "[" {
		end := matchingClose(rest, 0)
		if end != 4 || rest[1].Kind != IdentifierToken || rest[2].Text != ":" || lower(rest[3]) != "string" {
			return fail("a pattern path is [Name:string]", rest[0].Span)
		}
		statement.Path, statement.HasPath, rest = rest[1].Text, true, rest[5:]
	}
	if len(rest) < 2 || rest[0].Text != "{" || matchingClose(rest, 0) != len(rest)-1 {
		return fail("pattern entries must be in braces", statement.Span)
	}
	var errs []ParseError
	for _, entry := range splitTopLevel(rest[1:len(rest)-1], ";") {
		if len(entry) == 0 {
			continue
		}
		parsed := PatternEntry{Span: tokensSpan(entry)}
		if entry[0].Text != "(" {
			return fail("a pattern entry starts with (values)", parsed.Span)
		}
		end := matchingClose(entry, 0)
		if end < 0 {
			return fail("unclosed pattern entry values", parsed.Span)
		}
		for _, part := range splitTopLevel(entry[1:end], ",") {
			value, ok := patternValue(part)
			if !ok {
				return fail("pattern entry values are string literals", tokensSpan(part))
			}
			parsed.Args = append(parsed.Args, value)
		}
		if len(parsed.Args) != len(statement.Params) {
			return fail("a pattern entry needs one value per parameter", parsed.Span)
		}
		body := entry[end+1:]
		if len(body) >= 4 && body[0].Text == "." && body[1].Text == "[" {
			pathEnd := matchingClose(body, 1)
			value, ok := "", pathEnd == 3
			if ok {
				value, ok = patternValue(body[2:3])
			}
			if !ok || !statement.HasPath {
				return fail("a pattern entry path is .[\"value\"] and requires a declared path", parsed.Span)
			}
			parsed.Path, body = value, body[pathEnd+1:]
		} else if statement.HasPath {
			return fail("pattern entries need a path value", parsed.Span)
		}
		if len(body) < 3 || body[0].Text != "=" || body[1].Text != "{" || matchingClose(body, 1) != len(body)-1 {
			return fail("a pattern entry is (values) = { query }", parsed.Span)
		}
		pipeline, pipelineErrs := parsePipeline(body[2 : len(body)-1])
		errs = append(errs, pipelineErrs...)
		parsed.Body = pipeline
		statement.Entries = append(statement.Entries, parsed)
	}
	return statement, errs
}
