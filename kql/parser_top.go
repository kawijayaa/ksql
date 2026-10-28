package kql

// parseTopHitters parses top-hitters Count of Value [by Sum].
func parseTopHitters(op Operator, tokens []Token) (Operator, []ParseError) {
	spec := TopHittersSpec{}
	op.Body = spec
	fail := func(message string) (Operator, []ParseError) {
		return op, []ParseError{{Code: "KQLP0360", Message: message, Span: op.Span}}
	}
	of := topLevelKeyword(tokens, "of")
	if of <= 0 || of+1 >= len(tokens) {
		return fail("top-hitters requires Count of Value")
	}
	count, errs := parseExpressionTokens(tokens[:of])
	if len(errs) > 0 {
		return op, errs
	}
	rest := tokens[of+1:]
	value := rest
	if by := topLevelKeyword(rest, "by"); by >= 0 {
		if by+1 >= len(rest) {
			return fail("top-hitters by requires an expression")
		}
		sum, errs := parseExpressionTokens(rest[by+1:])
		if len(errs) > 0 {
			return op, errs
		}
		spec.By, value = sum, rest[:by]
	}
	expressions, errs := parseExpressionList(value)
	if len(errs) > 0 {
		return op, errs
	}
	if len(expressions) != 1 {
		return fail("top-hitters requires one value expression")
	}
	spec.Count, spec.Value = count, expressions[0]
	op.Body = spec
	return op, nil
}

// parseTopNested parses [N] of Expr [with others = Const] by Aggregation
// [asc|desc] clauses separated by ", top-nested".
func parseTopNested(op Operator, tokens []Token) (Operator, []ParseError) {
	spec := TopNestedSpec{}
	op.Body = spec
	fail := func(message string, span Span) (Operator, []ParseError) {
		return op, []ParseError{{Code: "KQLP0361", Message: message, Span: span}}
	}
	var clauses [][]Token
	for i, part := range splitTopLevel(tokens, ",") {
		if i > 0 {
			if len(part) == 0 || lower(part[0]) != "top-nested" {
				// A comma inside an aggregation belongs to the previous clause.
				if len(clauses) == 0 {
					return fail("top-nested clauses are separated by , top-nested", op.Span)
				}
				previous := clauses[len(clauses)-1]
				clauses[len(clauses)-1] = append(append(previous, Token{Kind: PunctuationToken, Text: ","}), part...)
				continue
			}
			part = part[1:]
		}
		clauses = append(clauses, part)
	}
	var errs []ParseError
	for _, clause := range clauses {
		level := TopNestedLevel{Span: tokensSpan(clause)}
		of := topLevelKeyword(clause, "of")
		if of < 0 {
			return fail("a top-nested clause requires of Expr", level.Span)
		}
		if of > 0 {
			count, countErrs := parseExpressionTokens(clause[:of])
			errs = append(errs, countErrs...)
			level.Count = count
		}
		rest := clause[of+1:]
		by := topLevelKeyword(rest, "by")
		if by <= 0 || by+1 >= len(rest) {
			return fail("a top-nested clause requires by Aggregation", level.Span)
		}
		valueTokens, aggregation := rest[:by], rest[by+1:]
		if with := topLevelKeyword(valueTokens, "with"); with >= 0 {
			others := valueTokens[with+1:]
			if len(others) < 3 || lower(others[0]) != "others" || others[1].Text != "=" {
				return fail("top-nested requires with others = Value", level.Span)
			}
			value, otherErrs := parseExpressionTokens(others[2:])
			errs = append(errs, otherErrs...)
			level.Others, valueTokens = value, valueTokens[:with]
		}
		if n := len(aggregation); n > 1 && (lower(aggregation[n-1]) == "asc" || lower(aggregation[n-1]) == "desc") {
			level.Ascending, aggregation = lower(aggregation[n-1]) == "asc", aggregation[:n-1]
		}
		values, valueErrs := parseExpressionList(valueTokens)
		aggregates, aggregateErrs := parseExpressionList(aggregation)
		errs = append(append(errs, valueErrs...), aggregateErrs...)
		if len(values) != 1 || len(aggregates) != 1 {
			return fail("a top-nested clause has one value and one aggregation", level.Span)
		}
		level.Value, level.Aggregation = values[0], aggregates[0]
		spec.Levels = append(spec.Levels, level)
	}
	op.Body = spec
	return op, errs
}
