package kql

// ParseKVOption is one name=value entry of a parse-kv with clause. Value is a
// string literal token or a bool identifier.
type ParseKVOption struct {
	Name  string
	Value Token
	Span  Span
}

// ParseKVSpec is the typed body of parse-kv. Keys use ParseColumn items.
type ParseKVSpec struct {
	Source  Expression
	Keys    []ParseItem
	Options []ParseKVOption
}

func (ParseKVSpec) operatorBody() {}

func parseParseKV(op Operator, tokens []Token) (Operator, []ParseError) {
	spec := ParseKVSpec{}
	op.Body = spec
	fail := func(message string, span Span) (Operator, []ParseError) {
		return op, []ParseError{{Code: "KQLP0351", Message: message, Span: span}}
	}
	as := topLevelKeyword(tokens, "as")
	if as <= 0 || as+1 >= len(tokens) || tokens[as+1].Text != "(" {
		return fail("parse-kv requires an expression followed by as (keys)", op.Span)
	}
	source, errs := parseExpressionTokens(tokens[:as])
	if len(errs) > 0 {
		return op, errs
	}
	spec.Source = source
	close := matchingClose(tokens, as+1)
	if close < 0 {
		return fail("parse-kv key list is not closed", tokens[as+1].Span)
	}
	keyTokens := tokens[as+2 : close]
	if len(keyTokens) == 0 {
		return fail("parse-kv requires at least one key", tokens[as+1].Span)
	}
	for _, part := range splitTopLevel(keyTokens, ",") {
		if len(spec.Keys) >= maxParsePatternItems {
			return fail("parse-kv key list exceeds 256 keys", tokensSpan(part))
		}
		item := ParseItem{Kind: ParseColumn, Span: tokensSpan(part)}
		nameTokens := part
		if colon := topLevelToken(part, ":"); colon >= 0 {
			if colon+2 != len(part) || part[colon+1].Kind != IdentifierToken {
				return fail("parse-kv key type requires a scalar type name", item.Span)
			}
			item.Type, nameTokens = part[colon+1].Text, part[:colon]
		}
		name, valid := declarationName(nameTokens)
		if !valid {
			return fail("parse-kv key requires an identifier", item.Span)
		}
		item.Name = name
		spec.Keys = append(spec.Keys, item)
	}
	rest := tokens[close+1:]
	if len(rest) == 0 {
		op.Body = spec
		return op, nil
	}
	if lower(rest[0]) != "with" || len(rest) < 2 || rest[1].Text != "(" || matchingClose(rest, 1) != len(rest)-1 {
		return fail("parse-kv options require with (name=value, ...)", tokensSpan(rest))
	}
	if inner := rest[2 : len(rest)-1]; len(inner) > 0 {
		for _, part := range splitTopLevel(inner, ",") {
			if len(part) != 3 || part[0].Kind != IdentifierToken || part[1].Text != "=" || (part[2].Kind != StringToken && part[2].Kind != IdentifierToken) {
				return fail("parse-kv option requires name=value", tokensSpan(part))
			}
			spec.Options = append(spec.Options, ParseKVOption{Name: lower(part[0]), Value: part[2], Span: tokensSpan(part)})
		}
	}
	op.Body = spec
	return op, nil
}
