package kql

// RenderProperty is one with-clause property. Values holds one token group per
// comma-separated value; list properties such as series take several.
type RenderProperty struct {
	Name   string
	Values [][]Token
	Span   Span
}

// RenderSpec is the typed body of render.
type RenderSpec struct {
	Visualization string
	Properties    []RenderProperty
	Span          Span
}

func (RenderSpec) operatorBody() {}

func parseRender(op Operator, tokens []Token) (Operator, []ParseError) {
	spec := RenderSpec{Span: op.Span}
	op.Body = spec
	fail := func(message string, span Span) (Operator, []ParseError) {
		return op, []ParseError{{Code: "KQLP0352", Message: message, Span: span}}
	}
	if len(tokens) == 0 || tokens[0].Kind != IdentifierToken {
		return fail("render requires a visualization", op.Span)
	}
	spec.Visualization, spec.Span = lower(tokens[0]), tokens[0].Span
	rest := tokens[1:]
	if len(rest) > 0 {
		if lower(rest[0]) != "with" || len(rest) < 3 || rest[1].Text != "(" || matchingClose(rest, 1) != len(rest)-1 {
			return fail("render properties require with (name=value, ...)", tokensSpan(rest))
		}
		for _, part := range splitTopLevel(rest[2:len(rest)-1], ",") {
			if len(part) == 0 {
				return fail("render property list has an empty entry", tokensSpan(rest))
			}
			if len(part) >= 3 && part[0].Kind == IdentifierToken && part[1].Text == "=" {
				spec.Properties = append(spec.Properties, RenderProperty{Name: lower(part[0]), Values: [][]Token{part[2:]}, Span: tokensSpan(part)})
				continue
			}
			// A bare entry continues the previous list-valued property.
			if len(spec.Properties) == 0 {
				return fail("render property requires name=value", tokensSpan(part))
			}
			last := &spec.Properties[len(spec.Properties)-1]
			last.Values = append(last.Values, part)
		}
	}
	op.Body = spec
	return op, nil
}
