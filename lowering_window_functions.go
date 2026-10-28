package ksql

import (
	"strconv"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

const maxWindowCalls = 64

var windowFunctions = map[string]bool{
	"prev": true, "next": true, "row_number": true, "row_cumsum": true,
	"row_rank_dense": true, "row_rank_min": true, "row_window_session": true,
}

// isWindowCall reports a call to a built-in window function that no query
// function or application rule overrides.
func (s *compileState) isWindowCall(expression kql.Expression) (*kql.CallExpression, string, bool) {
	call, ok := expression.(*kql.CallExpression)
	if !ok {
		return nil, "", false
	}
	name, ok := call.Function.(*kql.NameExpression)
	if !ok {
		return nil, "", false
	}
	function := strings.ToLower(unquoteKQL(name.Name))
	if !windowFunctions[function] {
		return nil, "", false
	}
	if _, declared := s.functions[function]; declared || s.compiler.functionRules[function] != nil || s.compiler.typedFunctionRules[function] != nil {
		return nil, "", false
	}
	return call, function, true
}

// rewriteExpression copies expression nodes on the path to replaced nodes so
// shared let and function bodies are never mutated. Subqueries are opaque.
func rewriteExpression(expression kql.Expression, replace func(kql.Expression) (kql.Expression, bool)) kql.Expression {
	if expression == nil {
		return nil
	}
	if replacement, ok := replace(expression); ok {
		return replacement
	}
	switch node := expression.(type) {
	case *kql.UnaryExpression:
		copy := *node
		copy.Operand = rewriteExpression(node.Operand, replace)
		return &copy
	case *kql.BinaryExpression:
		copy := *node
		copy.Left, copy.Right = rewriteExpression(node.Left, replace), rewriteExpression(node.Right, replace)
		return &copy
	case *kql.CallExpression:
		copy := *node
		copy.Args = make([]kql.Expression, len(node.Args))
		for i, argument := range node.Args {
			copy.Args[i] = rewriteExpression(argument, replace)
		}
		return &copy
	case *kql.IndexExpression:
		copy := *node
		copy.Value, copy.Index = rewriteExpression(node.Value, replace), rewriteExpression(node.Index, replace)
		return &copy
	case *kql.NamedExpression:
		copy := *node
		copy.Value = rewriteExpression(node.Value, replace)
		return &copy
	case *kql.CompoundNamedExpression:
		copy := *node
		copy.Value = rewriteExpression(node.Value, replace)
		return &copy
	case *kql.ListExpression:
		copy := *node
		copy.Items = make([]kql.Expression, len(node.Items))
		for i, item := range node.Items {
			copy.Items[i] = rewriteExpression(item, replace)
		}
		return &copy
	}
	return expression
}

// innermostWindowCall finds a window call whose arguments contain no window
// calls, so it can be computed from the current stage's columns.
func (s *compileState) innermostWindowCall(expression kql.Expression) *kql.CallExpression {
	var found *kql.CallExpression
	rewriteExpression(expression, func(node kql.Expression) (kql.Expression, bool) {
		if found != nil {
			return node, true
		}
		if call, _, ok := s.isWindowCall(node); ok {
			for _, argument := range call.Args {
				if inner := s.innermostWindowCall(argument); inner != nil {
					found = inner
					return node, true
				}
			}
			found = call
			return node, true
		}
		return nil, false
	})
	return found
}

func (s *compileState) hasWindowCall(expressions []kql.Expression) bool {
	for _, expression := range expressions {
		if s.innermostWindowCall(expression) != nil {
			return true
		}
	}
	return false
}

// windowStages accumulates hidden columns computed by streaming fenced selects
// over a serialized relation, all ordered by one hidden ordinal.
type windowStages struct {
	state    *compileState
	relation Relation
	reserved map[string]bool
	ordinal  string
	hidden   []string
}

func (w *windowStages) name(base string) string {
	for i := 0; ; i++ {
		name := "__ksql_window_" + base + strconv.Itoa(i)
		key := strings.ToLower(name)
		if _, let := w.state.scalarLets[key]; !w.reserved[key] && !let {
			w.reserved[key] = true
			w.hidden = append(w.hidden, name)
			return name
		}
	}
}

// stage appends named expressions to the relation in one select.
func (w *windowStages) stage(names []string, values []boundExpr) {
	s := w.state
	items := make([]sqlast.SelectItem, 0, len(w.relation.Schema.Columns)+len(names))
	for _, column := range w.relation.Schema.Columns {
		items = append(items, sqlast.SelectItem{Expr: s.columnIdentifier(column.Name), Alias: column.Name})
	}
	schema := w.relation.Schema.Clone()
	for i, name := range names {
		items = append(items, sqlast.SelectItem{Expr: values[i].SQL, Alias: name})
		schema.Columns = append(schema.Columns, Column{Name: name, Type: values[i].Type})
	}
	w.relation = Relation{Query: &sqlast.Select{Fence: true, From: &sqlast.Subquery{Query: w.relation.Query, Alias: s.nextAlias()}, Projections: items}, Schema: schema, rowDependencies: w.relation.rowDependencies}
}

func (w *windowStages) order() []sqlast.Order {
	return []sqlast.Order{{Expr: w.state.columnIdentifier(w.ordinal), Direction: "asc"}}
}

func (w *windowStages) window(call sqlast.Expr, partition []sqlast.Expr, cumulative bool) sqlast.Expr {
	return &sqlast.Window{Expr: call, PartitionBy: partition, OrderBy: w.order(), Cumulative: cumulative}
}

func (w *windowStages) bind(expression kql.Expression) boundExpr {
	w.state.input = w.relation.Schema
	return w.state.expression(expression)
}

func zeroOneCase(condition sqlast.Expr) sqlast.Expr {
	one := &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "1"}
	zero := &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "0"}
	return &sqlast.Case{Branches: []sqlast.When{{Condition: condition, Result: one}}, Else: zero}
}

// restartGroup numbers the segments started by a restart predicate.
func (w *windowStages) restartGroup(restart kql.Expression, feature string) ([]sqlast.Expr, bool) {
	if restart == nil {
		return nil, true
	}
	bound := w.bind(restart)
	if bound.SQL == nil {
		return nil, false
	}
	if bound.Type != TypeBool && bound.Type != TypeUnknown {
		w.state.bindError("KQLB0392", restart.NodeSpan(), feature, "window restart predicate must be bool")
		return nil, false
	}
	group := w.name("group")
	sum := &sqlast.Call{Name: "sum", Args: []sqlast.Expr{zeroOneCase(bound.SQL)}}
	w.stage([]string{group}, []boundExpr{{SQL: w.window(sum, nil, true), Type: TypeLong}})
	return []sqlast.Expr{w.state.columnIdentifier(group)}, true
}

func integerLiteral(expression kql.Expression) (int64, bool) {
	sign := int64(1)
	if unary, ok := expression.(*kql.UnaryExpression); ok && unary.Operator == "-" {
		sign, expression = -1, unary.Operand
	}
	literal, ok := expression.(*kql.LiteralExpression)
	if !ok || literal.Kind != kql.NumberToken {
		return 0, false
	}
	value, err := strconv.ParseInt(literal.Text, 10, 64)
	return sign * value, err == nil
}

// lower computes one window call into a hidden column and returns its name.
func (w *windowStages) lower(call *kql.CallExpression, function string) (string, bool) {
	s := w.state
	feature := "function." + function
	arity := func(minimum, maximum int) bool {
		if len(call.Args) < minimum || len(call.Args) > maximum {
			s.bindError("KQLB0393", call.Span, feature, function+" has an invalid number of arguments")
			return false
		}
		return true
	}
	argument := func(i int) kql.Expression {
		if i < len(call.Args) {
			return call.Args[i]
		}
		return nil
	}
	result := w.name("value")
	switch function {
	case "prev", "next":
		if !arity(1, 3) {
			return "", false
		}
		value := w.bind(call.Args[0])
		if value.SQL == nil {
			return "", false
		}
		offset := int64(1)
		if len(call.Args) > 1 {
			var ok bool
			if offset, ok = integerLiteral(call.Args[1]); !ok || offset < 0 {
				s.bindError("KQLB0394", call.Args[1].NodeSpan(), feature, function+" offset must be a non-negative integer literal")
				return "", false
			}
		}
		args := []sqlast.Expr{value.SQL, &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: strconv.FormatInt(offset, 10)}}
		if len(call.Args) > 2 {
			fallback := w.bind(call.Args[2])
			if fallback.SQL == nil {
				return "", false
			}
			args = append(args, fallback.SQL)
		}
		name := map[string]string{"prev": "lag", "next": "lead"}[function]
		w.stage([]string{result}, []boundExpr{{SQL: w.window(&sqlast.Call{Name: name, Args: args}, nil, false), Type: value.Type}})
	case "row_number":
		if !arity(0, 2) {
			return "", false
		}
		start := int64(1)
		if len(call.Args) > 0 {
			var ok bool
			if start, ok = integerLiteral(call.Args[0]); !ok {
				s.bindError("KQLB0394", call.Args[0].NodeSpan(), feature, "row_number starting index must be an integer literal")
				return "", false
			}
		}
		partition, ok := w.restartGroup(argument(1), feature)
		if !ok {
			return "", false
		}
		number := w.window(&sqlast.Call{Name: "row_number"}, partition, false)
		if start != 1 {
			number = &sqlast.Binary{Left: number, Operator: "+", Right: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: strconv.FormatInt(start-1, 10)}}
		}
		w.stage([]string{result}, []boundExpr{{SQL: number, Type: TypeLong}})
	case "row_cumsum":
		if !arity(1, 2) {
			return "", false
		}
		term := w.bind(call.Args[0])
		if term.SQL == nil {
			return "", false
		}
		resultType := TypeLong
		switch term.Type {
		case TypeReal:
			resultType = TypeReal
		case TypeInt, TypeLong, TypeUnknown:
		default:
			s.bindError("KQLB0395", call.Args[0].NodeSpan(), feature, "row_cumsum term must be int, long or real")
			return "", false
		}
		termName := w.name("term")
		w.stage([]string{termName}, []boundExpr{term})
		partition, ok := w.restartGroup(argument(1), feature)
		if !ok {
			return "", false
		}
		sum := &sqlast.Call{Name: "sum", Args: []sqlast.Expr{s.columnIdentifier(termName)}}
		w.stage([]string{result}, []boundExpr{{SQL: w.window(sum, partition, true), Type: resultType}})
	case "row_rank_dense", "row_rank_min":
		if !arity(1, 2) {
			return "", false
		}
		term := w.bind(call.Args[0])
		if term.SQL == nil {
			return "", false
		}
		if term.Type == TypeDynamic {
			s.bindError("KQLB0395", call.Args[0].NodeSpan(), feature, function+" term must be a scalar value")
			return "", false
		}
		termName := w.name("term")
		w.stage([]string{termName}, []boundExpr{term})
		partition, ok := w.restartGroup(argument(1), feature)
		if !ok {
			return "", false
		}
		termColumn := s.columnIdentifier(termName)
		if function == "row_rank_min" {
			// The rank is the minimal row number at which the term appears.
			number := w.name("number")
			w.stage([]string{number}, []boundExpr{{SQL: w.window(&sqlast.Call{Name: "row_number"}, partition, false), Type: TypeLong}})
			minimum := &sqlast.Window{Expr: &sqlast.Call{Name: "min", Args: []sqlast.Expr{s.columnIdentifier(number)}}, PartitionBy: append(append([]sqlast.Expr{}, partition...), termColumn)}
			w.stage([]string{result}, []boundExpr{{SQL: minimum, Type: TypeLong}})
			break
		}
		// Dense rank increments when the term differs from the previous row.
		previous := w.window(&sqlast.Call{Name: "lag", Args: []sqlast.Expr{termColumn}}, partition, false)
		first := &sqlast.Binary{Left: w.window(&sqlast.Call{Name: "row_number"}, partition, false), Operator: "=", Right: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "1"}}
		same := &sqlast.Binary{
			Left:     &sqlast.Binary{Left: termColumn, Operator: "=", Right: previous},
			Operator: "OR",
			Right: &sqlast.Binary{
				Left:     &sqlast.Binary{Left: termColumn, Operator: "IS", Right: &sqlast.Literal{Kind: sqlast.NullLiteral}},
				Operator: "AND",
				Right:    &sqlast.Binary{Left: previous, Operator: "IS", Right: &sqlast.Literal{Kind: sqlast.NullLiteral}},
			},
		}
		changed := w.name("changed")
		change := &sqlast.Case{Branches: []sqlast.When{
			{Condition: first, Result: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "1"}},
			{Condition: same, Result: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "0"}},
		}, Else: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "1"}}
		w.stage([]string{changed}, []boundExpr{{SQL: change, Type: TypeLong}})
		sum := &sqlast.Call{Name: "sum", Args: []sqlast.Expr{s.columnIdentifier(changed)}}
		w.stage([]string{result}, []boundExpr{{SQL: w.window(sum, partition, true), Type: TypeLong}})
	default:
		s.error("KQLL0390", call.Span, feature, function+" is not supported by SQL lowering")
		return "", false
	}
	return result, true
}

// windowOperator lifts window calls out of an extend, project, serialize or
// where operator into hidden columns, applies the rewritten operator, and
// restores the serialized order while removing the hidden columns.
func (s *compileState) windowOperator(input Relation, operator kql.Operator, expressions []kql.Expression) Relation {
	feature := "operator." + operator.Kind
	if input.Schema.Unknown {
		s.bindError("KQLB0390", operator.Span, feature, "window functions require a known input schema")
		return Relation{}
	}
	if !input.serialized {
		s.bindError("KQLB0390", operator.Span, feature, "window functions require a serialized row set; add serialize or sort first")
		return Relation{}
	}
	previousInput := s.input
	defer func() { s.input = previousInput }()
	stages := &windowStages{state: s, relation: input, reserved: map[string]bool{}}
	for _, column := range input.Schema.Columns {
		stages.reserved[strings.ToLower(column.Name)] = true
	}
	stages.ordinal = stages.name("ordinal")
	ordinal := &sqlast.Window{Expr: &sqlast.Call{Name: "row_number"}, OrderBy: input.order}
	stages.stage([]string{stages.ordinal}, []boundExpr{{SQL: ordinal, Type: TypeLong}})

	rewritten := append([]kql.Expression(nil), expressions...)
	for calls := 0; ; calls++ {
		var call *kql.CallExpression
		for _, expression := range rewritten {
			if call = s.innermostWindowCall(expression); call != nil {
				break
			}
		}
		if call == nil {
			break
		}
		if calls >= maxWindowCalls {
			s.bindError("KQLB0396", operator.Span, feature, "operator exceeds 64 window function calls")
			return Relation{}
		}
		_, function, _ := s.isWindowCall(call)
		name, ok := stages.lower(call, function)
		if !ok {
			return Relation{}
		}
		for i, expression := range rewritten {
			rewritten[i] = rewriteExpression(expression, func(node kql.Expression) (kql.Expression, bool) {
				if node == kql.Expression(call) {
					return &kql.NameExpression{Name: name, Span: call.Span}, true
				}
				return nil, false
			})
		}
	}

	staged := stages.relation
	staged.serialized = true
	staged.order = stages.order()
	kind := operator.Kind
	if kind == "serialize" {
		kind = "extend"
	}
	if kind == "project" {
		rewritten = append(rewritten, &kql.NameExpression{Name: stages.ordinal, Span: operator.Span})
	}
	inner := kql.Operator{Kind: kind, Body: kql.ExpressionSpec{Expressions: rewritten}, Span: operator.Span, Raw: operator.Raw}
	result := s.operator(staged, inner)
	if result.Query == nil {
		return Relation{}
	}
	hidden := map[string]bool{}
	for _, name := range stages.hidden {
		hidden[strings.ToLower(name)] = true
	}
	visible := Schema{}
	items := []sqlast.SelectItem{}
	for _, column := range result.Schema.Columns {
		if hidden[strings.ToLower(column.Name)] {
			continue
		}
		visible.Columns = append(visible.Columns, column)
		items = append(items, sqlast.SelectItem{Expr: s.columnIdentifier(column.Name), Alias: column.Name})
	}
	final := &sqlast.Select{From: &sqlast.Subquery{Query: result.Query, Alias: s.nextAlias()}, Projections: items, OrderBy: stages.order()}
	return Relation{Query: final, Schema: visible, rowDependencies: result.rowDependencies}
}

// orderColumns lists the column names an ordering depends on.
func orderColumns(orders []sqlast.Order) map[string]bool {
	columns := map[string]bool{}
	for _, order := range orders {
		sqlast.Walk(order.Expr, func(node any) bool {
			if identifier, ok := node.(*sqlast.Identifier); ok && len(identifier.Parts) > 0 {
				columns[strings.ToLower(identifier.Parts[len(identifier.Parts)-1])] = true
			}
			return true
		})
	}
	return columns
}

// serializedAfter applies Kusto's serialization rules: sort and top emit
// serialized rows; where, take, serialize, as, extend, parse and the project
// family preserve them. An operator that redefines or removes a column the
// order depends on ends serialization, because SQL has no implicit row order.
func (s *compileState) serializedAfter(input, output Relation, operator kql.Operator) Relation {
	if output.serialized || output.Query == nil {
		return output
	}
	if !input.serialized {
		return output
	}
	columns := orderColumns(input.order)
	keep := func(assigned []string) Relation {
		for _, name := range assigned {
			if columns[strings.ToLower(name)] {
				return output
			}
		}
		for name := range columns {
			if _, _, found, _ := output.Schema.Lookup(name); !found {
				return output
			}
		}
		output.serialized, output.order = true, input.order
		return output
	}
	assignedNames := func(expressions []kql.Expression) []string {
		names := []string{}
		for _, expression := range expressions {
			switch node := expression.(type) {
			case *kql.NamedExpression:
				if value, ok := node.Value.(*kql.NameExpression); !ok || !strings.EqualFold(unquoteKQL(value.Name), unquoteKQL(node.Name)) {
					names = append(names, unquoteKQL(node.Name))
				}
			case *kql.CompoundNamedExpression:
				for _, name := range node.Names {
					names = append(names, unquoteKQL(name))
				}
			}
		}
		return names
	}
	switch operator.Kind {
	case "where", "filter", "take", "limit", "as", "serialize":
		return keep(nil)
	case "extend", "project", "project-away", "project-keep", "project-reorder":
		if spec, ok := operator.Body.(kql.ExpressionSpec); ok {
			return keep(assignedNames(spec.Expressions))
		}
		return keep(nil)
	case "project-rename":
		if spec, ok := operator.Body.(kql.ExpressionSpec); ok {
			names := assignedNames(spec.Expressions)
			for _, expression := range spec.Expressions {
				if named, ok := expression.(*kql.NamedExpression); ok {
					if source, ok := named.Value.(*kql.NameExpression); ok {
						names = append(names, unquoteKQL(source.Name))
					}
				}
			}
			return keep(names)
		}
	case "parse", "parse-where":
		names := []string{}
		for _, item := range operator.Body.(kql.ParseSpec).Pattern {
			if item.Kind == kql.ParseColumn {
				names = append(names, unquoteKQL(item.Name))
			}
		}
		return keep(names)
	case "parse-kv":
		names := []string{}
		for _, item := range operator.Body.(kql.ParseKVSpec).Keys {
			names = append(names, unquoteKQL(item.Name))
		}
		return keep(names)
	}
	return output
}
