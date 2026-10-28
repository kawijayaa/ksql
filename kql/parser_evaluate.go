package kql

// EvaluateSpec is the typed body of evaluate Plugin(Args) [: (Schema)].
// Wildcard records a leading * in the output schema.
type EvaluateSpec struct {
	Plugin    string
	Args      []Expression
	Schema    []ColumnDefinition
	Wildcard  bool
	HasSchema bool
	Span      Span
}

func (EvaluateSpec) operatorBody() {}

func parseEvaluate(op Operator, tokens []Token) (Operator, []ParseError) {
	spec := EvaluateSpec{Span: op.Span}
	op.Body = spec
	fail := func(message string, span Span) (Operator, []ParseError) {
		return op, []ParseError{{Code: "KQLP0353", Message: message, Span: span}}
	}
	if len(tokens) < 3 || tokens[0].Kind != IdentifierToken || tokens[1].Text != "(" {
		return fail("evaluate requires a plugin call", op.Span)
	}
	close := matchingClose(tokens, 1)
	if close < 0 {
		return fail("evaluate plugin call is not closed", tokens[1].Span)
	}
	spec.Plugin = lower(tokens[0])
	if close > 2 {
		args, errs := parseExpressionList(tokens[2:close])
		if len(errs) > 0 {
			return op, errs
		}
		spec.Args = args
	}
	rest := tokens[close+1:]
	if len(rest) > 0 {
		if rest[0].Text != ":" || len(rest) < 3 || rest[1].Text != "(" || matchingClose(rest, 1) != len(rest)-1 {
			return fail("evaluate output schema requires : (Name:type, ...)", tokensSpan(rest))
		}
		spec.HasSchema = true
		for i, part := range splitTopLevel(rest[2:len(rest)-1], ",") {
			if len(part) == 1 && part[0].Text == "*" && i == 0 {
				spec.Wildcard = true
				continue
			}
			colon := topLevelToken(part, ":")
			if colon < 1 || colon+2 != len(part) || part[colon+1].Kind != IdentifierToken {
				return fail("evaluate output schema column requires Name:type", tokensSpan(part))
			}
			name, valid := declarationName(part[:colon])
			if !valid {
				return fail("evaluate output schema column requires an identifier", tokensSpan(part))
			}
			spec.Schema = append(spec.Schema, ColumnDefinition{Name: name, Type: part[colon+1].Text})
		}
	}
	op.Body = spec
	return op, nil
}
