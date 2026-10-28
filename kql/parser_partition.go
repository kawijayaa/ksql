package kql

// parsePartition parses partition [hints] by Column ( SubQuery ) and the
// legacy by Column { SubQueryWithSource } form.
func parsePartition(op Operator, tokens []Token) (Operator, []ParseError) {
	spec := PartitionSpec{Strategy: "legacy"}
	op.Body = spec
	fail := func(message string, span Span) (Operator, []ParseError) {
		return op, []ParseError{{Code: "KQLP0358", Message: message, Span: span}}
	}
	for len(tokens) >= 5 && lower(tokens[0]) == "hint" && tokens[1].Text == "." && tokens[3].Text == "=" {
		if lower(tokens[2]) == "strategy" {
			switch lower(tokens[4]) {
			case "legacy", "native", "shuffle":
				spec.Strategy = lower(tokens[4])
			default:
				return fail("partition hint.strategy must be legacy, native or shuffle", tokens[4].Span)
			}
		}
		tokens = tokens[5:]
	}
	if len(tokens) < 4 || lower(tokens[0]) != "by" || tokens[1].Kind != IdentifierToken {
		return fail("partition requires by Column followed by a subquery", op.Span)
	}
	spec.Key, tokens = tokens[1].Text, tokens[2:]
	if tokens[0].Text != "(" && tokens[0].Text != "{" {
		return fail("partition subquery must be in parentheses or braces", tokens[0].Span)
	}
	close := matchingClose(tokens, 0)
	if close != len(tokens)-1 || close < 2 {
		return fail("partition subquery must end the operator", tokens[0].Span)
	}
	inner := tokens[1:close]
	spec.WithSource = tokens[0].Text == "{"
	if spec.WithSource && spec.Strategy != "legacy" {
		return fail("a partition subquery with its own source requires the legacy strategy", tokens[0].Span)
	}
	var errs []ParseError
	if spec.WithSource {
		spec.Body, errs = parsePipeline(inner)
	} else {
		// The implicit source is the partition: parse the operators alone.
		spec.Body = &Pipeline{Span: tokensSpan(inner)}
		for _, part := range splitTopLevel(inner, "|") {
			if len(part) == 0 {
				return fail("missing query operator in partition subquery", tokensSpan(inner))
			}
			operator, operatorErrs := parseOperator(part)
			errs = append(errs, operatorErrs...)
			spec.Body.Operators = append(spec.Body.Operators, operator)
		}
	}
	op.Body = spec
	return op, errs
}
