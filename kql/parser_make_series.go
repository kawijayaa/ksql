package kql

// MakeSeriesItem is one [Name =] Aggregation [default = Value] entry.
type MakeSeriesItem struct {
	Name        string
	Aggregation Expression
	Default     Expression
	Span        Span
}

// MakeSeriesSpec is the typed body of make-series. Range marks the alternate
// "on Axis in range(start, stop, step)" form, whose stop is inclusive.
type MakeSeriesSpec struct {
	Kind  string
	Items []MakeSeriesItem
	Axis  Expression
	From  Expression
	To    Expression
	Step  Expression
	Range bool
	By    []Expression
}

func (MakeSeriesSpec) operatorBody() {}

func parseMakeSeries(op Operator, tokens []Token) (Operator, []ParseError) {
	spec := MakeSeriesSpec{}
	op.Body = spec
	fail := func(message string, span Span) (Operator, []ParseError) {
		return op, []ParseError{{Code: "KQLP0354", Message: message, Span: span}}
	}
	for len(tokens) >= 3 && lower(tokens[0]) == "kind" && tokens[1].Text == "=" {
		if lower(tokens[2]) != "nonempty" {
			return fail("make-series kind must be nonempty", tokens[2].Span)
		}
		spec.Kind, tokens = "nonempty", tokens[3:]
	}
	on := topLevelKeyword(tokens, "on")
	if on <= 0 || on+1 >= len(tokens) {
		return fail("make-series requires aggregations followed by on Axis", op.Span)
	}
	for _, part := range splitTopLevel(tokens[:on], ",") {
		item := MakeSeriesItem{Span: tokensSpan(part)}
		if index := topLevelKeyword(part, "default"); index > 0 {
			if index+2 >= len(part) || part[index+1].Text != "=" {
				return fail("make-series default requires default = value", item.Span)
			}
			value, errs := parseExpressionTokens(part[index+2:])
			if len(errs) > 0 {
				return op, errs
			}
			item.Default, part = value, part[:index]
		}
		if len(part) == 0 {
			return fail("make-series requires an aggregation", item.Span)
		}
		expressions, errs := parseExpressionList(part)
		if len(errs) > 0 {
			return op, errs
		}
		if len(expressions) != 1 {
			return fail("make-series requires one aggregation per item", item.Span)
		}
		expression := expressions[0]
		if named, ok := expression.(*NamedExpression); ok {
			item.Name, expression = named.Name, named.Value
		}
		item.Aggregation = expression
		spec.Items = append(spec.Items, item)
	}
	rest := tokens[on+1:]
	by := topLevelKeyword(rest, "by")
	axisTokens := rest
	if by >= 0 {
		if by+1 >= len(rest) {
			return fail("make-series by requires group expressions", rest[by].Span)
		}
		groups, errs := parseExpressionList(rest[by+1:])
		if len(errs) > 0 {
			return op, errs
		}
		spec.By, axisTokens = groups, rest[:by]
	}
	// on Axis in range(start, stop, step)
	if in := topLevelKeyword(axisTokens, "in"); in > 0 {
		axis, errs := parseExpressionTokens(axisTokens[:in])
		if len(errs) > 0 {
			return op, errs
		}
		spec.Axis, spec.Range = axis, true
		value, errs := parseExpressionTokens(axisTokens[in+1:])
		if len(errs) > 0 {
			return op, errs
		}
		call, ok := value.(*CallExpression)
		if ok {
			name, isName := call.Function.(*NameExpression)
			ok = isName && lower(Token{Text: name.Name}) == "range" && len(call.Args) == 3
		}
		if !ok {
			return fail("make-series in requires range(start, stop, step)", tokensSpan(axisTokens[in+1:]))
		}
		spec.From, spec.To, spec.Step = call.Args[0], call.Args[1], call.Args[2]
		op.Body = spec
		return op, nil
	}
	step := topLevelKeyword(axisTokens, "step")
	if step <= 0 || step+1 >= len(axisTokens) {
		return fail("make-series requires step", tokensSpan(axisTokens))
	}
	stepExpression, errs := parseExpressionTokens(axisTokens[step+1:])
	if len(errs) > 0 {
		return op, errs
	}
	spec.Step = stepExpression
	bounds := axisTokens[:step]
	if to := topLevelKeyword(bounds, "to"); to >= 0 {
		value, errs := parseExpressionTokens(bounds[to+1:])
		if len(errs) > 0 {
			return op, errs
		}
		spec.To, bounds = value, bounds[:to]
	}
	if from := topLevelKeyword(bounds, "from"); from >= 0 {
		value, errs := parseExpressionTokens(bounds[from+1:])
		if len(errs) > 0 {
			return op, errs
		}
		spec.From, bounds = value, bounds[:from]
	}
	axis, errs := parseExpressionTokens(bounds)
	if len(errs) > 0 {
		return op, errs
	}
	spec.Axis = axis
	op.Body = spec
	return op, nil
}
