package kql

// parseScan parses the scan operator body.
func parseScan(op Operator, tokens []Token) (Operator, []ParseError) {
	spec := ScanSpec{}
	op.Body = spec
	fail := func(message string, span Span) (Operator, []ParseError) {
		return op, []ParseError{{Code: "KQLP0359", Message: message, Span: span}}
	}
	if len(tokens) >= 3 && lower(tokens[0]) == "with_match_id" && tokens[1].Text == "=" {
		if tokens[2].Kind != IdentifierToken {
			return fail("with_match_id requires a column name", tokens[2].Span)
		}
		spec.MatchID, tokens = tokens[2].Text, tokens[3:]
	}
	if len(tokens) >= 2 && lower(tokens[0]) == "declare" && tokens[1].Text == "(" {
		close := matchingClose(tokens, 1)
		if close < 0 {
			return fail("unclosed scan declare", tokens[1].Span)
		}
		for _, part := range splitTopLevel(tokens[2:close], ",") {
			if len(part) < 3 || part[0].Kind != IdentifierToken || part[1].Text != ":" || part[2].Kind != IdentifierToken {
				return fail("scan declarations are Name:type [= default]", tokensSpan(part))
			}
			declare := ScanDeclare{Name: part[0].Text, Type: lower(part[2]), Span: tokensSpan(part)}
			if len(part) > 3 {
				if part[3].Text != "=" || len(part) < 5 {
					return fail("scan declarations are Name:type [= default]", tokensSpan(part))
				}
				value, errs := parseExpressionTokens(part[4:])
				if len(errs) > 0 {
					return op, errs
				}
				declare.Default = value
			}
			spec.Declares = append(spec.Declares, declare)
		}
		tokens = tokens[close+1:]
	}
	if len(tokens) < 3 || lower(tokens[0]) != "with" || tokens[1].Text != "(" || matchingClose(tokens, 1) != len(tokens)-1 {
		return fail("scan requires with (step ...; ...)", op.Span)
	}
	var errs []ParseError
	for _, part := range splitTopLevel(tokens[2:len(tokens)-1], ";") {
		if len(part) == 0 {
			continue
		}
		step := ScanStep{Output: "all", Span: tokensSpan(part)}
		if len(part) < 4 || lower(part[0]) != "step" || part[1].Kind != IdentifierToken {
			return fail("a scan step is step Name: Condition [=> assignments]", step.Span)
		}
		step.Name, part = part[1].Text, part[2:]
		if len(part) >= 3 && lower(part[0]) == "output" && part[1].Text == "=" {
			switch lower(part[2]) {
			case "all", "last", "none":
				step.Output = lower(part[2])
			default:
				return fail("scan step output must be all, last or none", part[2].Span)
			}
			part = part[3:]
		}
		if len(part) < 2 || part[0].Text != ":" {
			return fail("a scan step needs : Condition", step.Span)
		}
		part = part[1:]
		arrow, width := -1, 1
		depth := 0
		for i, token := range part {
			switch token.Text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			}
			if depth == 0 && token.Text == "=>" {
				arrow = i
				break
			}
			if depth == 0 && token.Text == "=" && i+1 < len(part) && part[i+1].Text == ">" {
				arrow, width = i, 2
				break
			}
		}
		condition := part
		if arrow >= 0 {
			condition = part[:arrow]
			for _, assignment := range splitTopLevel(part[arrow+width:], ",") {
				if len(assignment) < 3 || assignment[0].Kind != IdentifierToken || assignment[1].Text != "=" {
					return fail("scan assignments are Column = Value", tokensSpan(assignment))
				}
				value, valueErrs := parseExpressionTokens(assignment[2:])
				errs = append(errs, valueErrs...)
				step.Assignments = append(step.Assignments, &NamedExpression{Name: assignment[0].Text, Value: value, Span: tokensSpan(assignment)})
			}
		}
		if len(condition) == 0 {
			return fail("a scan step needs a condition", step.Span)
		}
		value, conditionErrs := parseExpressionTokens(condition)
		errs = append(errs, conditionErrs...)
		step.Condition = value
		spec.Steps = append(spec.Steps, step)
	}
	if len(spec.Steps) == 0 {
		return fail("scan requires at least one step", op.Span)
	}
	op.Body = spec
	return op, errs
}
