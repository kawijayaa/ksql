package kql

// parseReduce parses reduce [kind = source] by Expr
// [with [threshold = Constant] [, characters = Constant]].
func parseReduce(op Operator, tokens []Token) (Operator, []ParseError) {
	spec := ReduceSpec{}
	op.Body = spec
	fail := func(message string) (Operator, []ParseError) {
		return op, []ParseError{{Code: "KQLP0364", Message: message, Span: op.Span}}
	}
	if len(tokens) >= 3 && lower(tokens[0]) == "kind" && tokens[1].Text == "=" {
		switch lower(tokens[2]) {
		case "source":
			spec.Source = true
		case "default":
		default:
			return fail("reduce kind must be source")
		}
		tokens = tokens[3:]
	}
	if len(tokens) == 0 || lower(tokens[0]) != "by" {
		return fail("reduce requires by Expr")
	}
	tokens = tokens[1:]
	if with := topLevelKeyword(tokens, "with"); with >= 0 {
		for _, part := range splitTopLevel(tokens[with+1:], ",") {
			if len(part) < 3 || part[1].Text != "=" {
				return fail("reduce with expects threshold = Constant or characters = Constant")
			}
			value, errs := parseExpressionTokens(part[2:])
			if len(errs) > 0 {
				return op, errs
			}
			switch lower(part[0]) {
			case "threshold":
				spec.Threshold = value
			case "characters":
				spec.Characters = value
			default:
				return fail("unknown reduce parameter " + part[0].Text)
			}
		}
		tokens = tokens[:with]
	}
	if len(tokens) == 0 {
		return fail("reduce requires by Expr")
	}
	value, errs := parseExpressionTokens(tokens)
	if len(errs) > 0 {
		return op, errs
	}
	spec.By = value
	op.Body = spec
	return op, nil
}
