package ksql

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// scan lowers Kusto's scan operator to a recursive query that consumes the
// serialized input one record per recursion step. Each step's state (an
// active flag, its match id, its own record and the referenced values of
// earlier steps) is carried in columns, and every record is evaluated
// against the steps in reverse order exactly as Kusto's matching logic
// describes: Check 1 promotes the previous step's state, Check 2 extends the
// step's own active sequence (or starts one at the first step). Outputs are
// read from consecutive recursion rows, which also supplies the deferred
// records of output=last steps.

const (
	maxScanSteps        = 16
	maxScanStateColumns = 1024
	scanRowColumn       = "__scan_rn"
)

type scanRef struct{ step, column int }

func (s *compileState) scan(input Relation, operator kql.Operator) Relation {
	spec := operator.Body.(kql.ScanSpec)
	feature := "operator.scan"
	fail := func(span kql.Span, message string) Relation {
		s.bindError("KQLB0360", span, feature, message)
		return Relation{}
	}
	if input.Schema.Unknown {
		return fail(operator.Span, "scan requires a bound input schema")
	}
	n := len(spec.Steps)
	if n > maxScanSteps {
		return fail(operator.Span, fmt.Sprintf("scan supports at most %d steps", maxScanSteps))
	}
	// The record columns: the input's, then the declared ones.
	record := input.Schema.Clone().Columns
	defaults := make([]kql.Expression, len(record))
	inputColumns := len(record)
	for _, declare := range spec.Declares {
		if containsColumn(record, declare.Name) || (spec.MatchID != "" && strings.EqualFold(declare.Name, spec.MatchID)) {
			return fail(declare.Span, "scan declares an existing column "+declare.Name)
		}
		kind := parseScalarType(declare.Type)
		if kind == TypeUnknown {
			return fail(declare.Span, "unknown scan column type "+declare.Type)
		}
		record = append(record, Column{Name: declare.Name, Type: kind})
		defaults = append(defaults, declare.Default)
	}
	if spec.MatchID != "" && containsColumn(input.Schema.Columns, spec.MatchID) {
		return fail(operator.Span, "with_match_id names an existing column "+spec.MatchID)
	}
	columnIndex := func(name string) int {
		for i, column := range record {
			if column.Name == name {
				return i
			}
		}
		return -1
	}
	stepIndex := map[string]int{}
	for k, step := range spec.Steps {
		if _, exists := stepIndex[step.Name]; exists {
			return fail(step.Span, "scan step names must be unique")
		}
		stepIndex[step.Name] = k + 1
		for _, assignment := range step.Assignments {
			if i := columnIndex(assignment.Name); i < inputColumns {
				return fail(assignment.Span, "scan steps assign only declared columns")
			}
		}
	}
	// Rewrite step.column references and declared names to placeholders.
	refs := map[scanRef]bool{}
	var rewriteErr string
	rewrite := func(expression kql.Expression) {
		kqlNames(expression, func(name *kql.NameExpression) {
			text := unquoteKQL(name.Name)
			if dot := strings.Index(text, "."); dot > 0 {
				if k, ok := stepIndex[text[:dot]]; ok {
					column := columnIndex(text[dot+1:])
					if column < 0 {
						rewriteErr = "unknown scan column " + text
						return
					}
					refs[scanRef{k, column}] = true
					name.Name = fmt.Sprintf("__scanref_%d_%d", k, column)
					return
				}
			}
			if column := columnIndex(text); column >= inputColumns {
				name.Name = fmt.Sprintf("__scandecl_%d", column)
			}
		})
	}
	for _, step := range spec.Steps {
		rewrite(step.Condition)
		for _, assignment := range step.Assignments {
			rewrite(assignment.Value)
		}
	}
	if rewriteErr != "" {
		return fail(operator.Span, rewriteErr)
	}
	// State column names: v(k, j, c) holds step j's value of column c in
	// step k's state; step k keeps its whole record and the referenced
	// columns of earlier steps.
	hasValue := func(k, j, c int) bool { return j == k || (j < k && refs[scanRef{j, c}]) }
	vname := func(k, j, c int) string { return fmt.Sprintf("__v%d_%d_%d", k, j, c) }
	var columns []string
	columns = append(columns, "rn", "nid")
	for k := 1; k <= n; k++ {
		columns = append(columns, fmt.Sprintf("a%d", k), fmt.Sprintf("m%d", k), fmt.Sprintf("c1_%d", k), fmt.Sprintf("c2_%d", k))
		for j := 1; j <= k; j++ {
			for c := range record {
				if hasValue(k, j, c) {
					columns = append(columns, vname(k, j, c))
				}
			}
		}
	}
	if len(columns) > maxScanStateColumns {
		return fail(operator.Span, "scan state is too large; reference fewer step columns")
	}
	// Number the serialized input.
	ordinal := &sqlast.Window{Expr: &sqlast.Call{Name: "row_number"}, OrderBy: input.order}
	numbered := &sqlast.Select{From: &sqlast.Subquery{Query: input.Query, Alias: s.nextAlias()}, Projections: []sqlast.SelectItem{
		{Expr: &sqlast.Star{}}, {Expr: ordinal, Alias: scanRowColumn},
	}}
	number := func(value int) sqlast.Expr {
		return &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: fmt.Sprint(value)}
	}
	null := &sqlast.Literal{Kind: sqlast.NullLiteral}
	col := func(alias, name string) sqlast.Expr { return &sqlast.Identifier{Parts: []string{alias, name}} }
	isTrue := func(alias, name string) sqlast.Expr {
		return &sqlast.Binary{Left: col(alias, name), Operator: "=", Right: number(1)}
	}
	and := func(left, right sqlast.Expr) sqlast.Expr {
		return &sqlast.Binary{Left: left, Operator: "AND", Right: right}
	}
	not := func(value sqlast.Expr) sqlast.Expr { return &sqlast.Unary{Operator: "NOT ", Operand: value} }
	flag := func(value sqlast.Expr) sqlast.Expr {
		return &sqlast.Case{Branches: []sqlast.When{{Condition: value, Result: number(1)}}, Else: number(0)}
	}
	cases := func(branches []sqlast.When, otherwise sqlast.Expr) sqlast.Expr {
		return &sqlast.Case{Branches: branches, Else: otherwise}
	}
	defaultOf := func(c int) sqlast.Expr {
		if defaults[c] == nil {
			return null
		}
		previous := s.input
		s.input = Schema{}
		bound := s.expression(defaults[c])
		s.input = previous
		if bound.SQL == nil {
			return null
		}
		return bound.SQL
	}
	// Bind expressions against a synthetic schema of placeholders.
	synthetic := Schema{}
	for _, column := range input.Schema.Columns {
		synthetic.Columns = append(synthetic.Columns, column)
	}
	for ref := range refs {
		synthetic.Columns = append(synthetic.Columns, Column{Name: fmt.Sprintf("__scanref_%d_%d", ref.step, ref.column), Type: record[ref.column].Type})
	}
	for c := inputColumns; c < len(record); c++ {
		synthetic.Columns = append(synthetic.Columns, Column{Name: fmt.Sprintf("__scandecl_%d", c), Type: record[c].Type})
	}
	bindings := map[kql.Expression]boundExpr{}
	bind := func(expression kql.Expression) (boundExpr, bool) {
		if bound, ok := bindings[expression]; ok {
			return bound, bound.SQL != nil
		}
		previousInput, previousQualifier := s.input, s.qualifier
		s.input, s.qualifier = synthetic, ""
		bound := s.expression(expression)
		s.input, s.qualifier = previousInput, previousQualifier
		bindings[expression] = bound
		return bound, bound.SQL != nil
	}
	// resolve substitutes placeholders: input columns read the current row,
	// declared names their defaults, and step references the given state.
	resolve := func(expr sqlast.Expr, stepValue func(j, c int) sqlast.Expr) sqlast.Expr {
		return substituteIdentifiers(expr, func(name string) (sqlast.Expr, bool) {
			var j, c int
			if _, err := fmt.Sscanf(name, "__scanref_%d_%d", &j, &c); err == nil && strings.HasPrefix(name, "__scanref_") {
				return stepValue(j, c), true
			}
			if _, err := fmt.Sscanf(name, "__scandecl_%d", &c); err == nil && strings.HasPrefix(name, "__scandecl_") {
				return defaultOf(c), true
			}
			if containsColumn(input.Schema.Columns, name) {
				return col("r", name), true
			}
			return nil, false
		})
	}
	present := &sqlast.Binary{Left: col("r", scanRowColumn), Operator: "IS NOT", Right: null}
	// Check expressions per step, computed from the last step down.
	check1 := make([]sqlast.Expr, n+2)
	check2 := make([]sqlast.Expr, n+2)
	alive := make([]sqlast.Expr, n+2)
	cleared := make([]sqlast.Expr, n+2)
	falseExpr := &sqlast.Binary{Left: number(1), Operator: "=", Right: number(0)}
	check1[n+1] = falseExpr
	for k := n; k >= 1; k-- {
		step := spec.Steps[k-1]
		condition, ok := bind(step.Condition)
		if !ok {
			return Relation{}
		}
		if condition.Type != TypeBool && condition.Type != TypeUnknown {
			return fail(step.Span, "scan step conditions must be Boolean")
		}
		cleared[k] = check1[k+1]
		alive[k] = and(isTrue("p", fmt.Sprintf("a%d", k)), not(cleared[k]))
		if k > 1 {
			fromPrevious := func(j, c int) sqlast.Expr {
				if j < k && hasValue(k-1, j, c) {
					return col("p", vname(k-1, j, c))
				}
				return defaultOf(c)
			}
			check1[k] = and(and(present, isTrue("p", fmt.Sprintf("a%d", k-1))), resolve(condition.SQL, fromPrevious))
		} else {
			check1[k] = falseExpr
		}
		fromOwn := func(j, c int) sqlast.Expr {
			if j <= k && hasValue(k, j, c) {
				return cases([]sqlast.When{{Condition: alive[k], Result: col("p", vname(k, j, c))}}, defaultOf(c))
			}
			return defaultOf(c)
		}
		active := alive[k]
		if k == 1 {
			active = &sqlast.Binary{Left: number(1), Operator: "=", Right: number(1)}
		}
		check2[k] = and(and(and(present, not(check1[k])), active), resolve(condition.SQL, fromOwn))
	}
	// The recursive step's projections.
	step := &sqlast.Select{
		From: &sqlast.Join{
			Left:  &sqlast.CTE{Name: "__scan", Alias: "p"},
			Right: &sqlast.Subquery{Query: numbered, Alias: "r"},
			Kind:  "LEFT",
			On:    &sqlast.Binary{Left: col("r", scanRowColumn), Operator: "=", Right: &sqlast.Binary{Left: col("p", "rn"), Operator: "+", Right: number(1)}},
		},
		Where: &sqlast.Binary{Left: col("p", "rn"), Operator: "<=", Right: &sqlast.ScalarSubquery{Query: &sqlast.Select{
			From: &sqlast.Subquery{Query: numbered, Alias: s.nextAlias()}, Projections: []sqlast.SelectItem{{Expr: &sqlast.Call{Name: "count", Args: []sqlast.Expr{&sqlast.Star{}}}}},
		}}},
	}
	// The first step starts a sequence when it has none, including one this
	// record just promoted to the second step.
	startsSequence := and(check2[1], not(alive[1]))
	add := func(name string, value sqlast.Expr) {
		step.Projections = append(step.Projections, sqlast.SelectItem{Expr: value, Alias: name})
	}
	add("rn", &sqlast.Binary{Left: col("p", "rn"), Operator: "+", Right: number(1)})
	add("nid", &sqlast.Binary{Left: col("p", "nid"), Operator: "+", Right: flag(startsSequence)})
	for k := 1; k <= n; k++ {
		stepSpec := spec.Steps[k-1]
		a, m := fmt.Sprintf("a%d", k), fmt.Sprintf("m%d", k)
		add(a, cases([]sqlast.When{
			{Condition: &sqlast.Binary{Left: check1[k], Operator: "OR", Right: check2[k]}, Result: number(1)},
			{Condition: cleared[k], Result: number(0)},
		}, col("p", a)))
		matchID := col("p", m)
		if k == 1 {
			matchID = cases([]sqlast.When{{Condition: startsSequence, Result: col("p", "nid")}}, col("p", m))
		}
		var previousID sqlast.Expr = null
		if k > 1 {
			previousID = col("p", fmt.Sprintf("m%d", k-1))
		}
		branches := []sqlast.When{{Condition: check1[k], Result: previousID}}
		if k == 1 {
			branches = append(branches, sqlast.When{Condition: startsSequence, Result: col("p", "nid")})
		}
		branches = append(branches, sqlast.When{Condition: cleared[k], Result: null})
		add(m, cases(branches, matchID))
		add(fmt.Sprintf("c1_%d", k), flag(check1[k]))
		add(fmt.Sprintf("c2_%d", k), flag(check2[k]))
		fromPrevious := func(j, c int) sqlast.Expr {
			if j < k && hasValue(k-1, j, c) {
				return col("p", vname(k-1, j, c))
			}
			return defaultOf(c)
		}
		fromOwn := func(j, c int) sqlast.Expr {
			if j <= k && hasValue(k, j, c) {
				return cases([]sqlast.When{{Condition: alive[k], Result: col("p", vname(k, j, c))}}, defaultOf(c))
			}
			return defaultOf(c)
		}
		assigned := map[int]kql.Expression{}
		for _, assignment := range stepSpec.Assignments {
			assigned[columnIndex(assignment.Name)] = assignment.Value
		}
		for j := 1; j <= k; j++ {
			for c := range record {
				if !hasValue(k, j, c) {
					continue
				}
				name := vname(k, j, c)
				if j < k {
					add(name, cases([]sqlast.When{{Condition: check1[k], Result: fromPrevious(j, c)}, {Condition: cleared[k], Result: defaultOf(c)}}, col("p", name)))
					continue
				}
				// The step's own record: the current row extended with its
				// assignments, evaluated in the state of the matching check.
				newValue := func(state func(j, c int) sqlast.Expr) sqlast.Expr {
					if c < inputColumns {
						return col("r", record[c].Name)
					}
					value, ok := assigned[c]
					if !ok {
						return defaultOf(c)
					}
					bound, ok := bind(value)
					if !ok {
						return null
					}
					return resolve(bound.SQL, state)
				}
				add(name, cases([]sqlast.When{
					{Condition: check1[k], Result: newValue(fromPrevious)},
					{Condition: check2[k], Result: newValue(fromOwn)},
					{Condition: cleared[k], Result: defaultOf(c)},
				}, col("p", name)))
			}
		}
	}
	if len(s.diagnostics) > 0 && hasErrors(s.diagnostics) {
		return Relation{}
	}
	anchor := &sqlast.Select{}
	for _, name := range columns {
		value := sqlast.Expr(null)
		switch {
		case name == "rn" || name == "nid":
			value = number(0)
		case strings.HasPrefix(name, "a") || strings.HasPrefix(name, "c1_") || strings.HasPrefix(name, "c2_"):
			value = number(0)
		case strings.HasPrefix(name, "__v"):
			var k, j, c int
			fmt.Sscanf(name, "__v%d_%d_%d", &k, &j, &c)
			value = defaultOf(c)
		}
		anchor.Projections = append(anchor.Projections, sqlast.SelectItem{Expr: value, Alias: name})
	}
	// Outputs, in Kusto's order: per record and step (last to first), the
	// deferred record of a replaced or promoted output=last sequence, then
	// the matched record of an output=all step; then, after the input,
	// the pending output=last records.
	output := Schema{Columns: append([]Column{}, record...)}
	if spec.MatchID != "" {
		output.Columns = append(output.Columns, Column{Name: spec.MatchID, Type: TypeLong})
	}
	var queries []sqlast.Query
	slot := 0
	emit := func(condition sqlast.Expr, alias string, k int) {
		projections := make([]sqlast.SelectItem, 0, len(record)+3)
		for c, column := range record {
			projections = append(projections, sqlast.SelectItem{Expr: col(alias, vname(k, k, c)), Alias: column.Name})
		}
		if spec.MatchID != "" {
			projections = append(projections, sqlast.SelectItem{Expr: col(alias, fmt.Sprintf("m%d", k)), Alias: spec.MatchID})
		}
		projections = append(projections, sqlast.SelectItem{Expr: col("cur", "rn"), Alias: "__scan_order_rn"}, sqlast.SelectItem{Expr: number(slot), Alias: "__scan_order_slot"})
		slot++
		queries = append(queries, &sqlast.Select{
			From: &sqlast.Join{
				Left:  &sqlast.CTE{Name: "__scan", Alias: "cur"},
				Right: &sqlast.CTE{Name: "__scan", Alias: "prev"},
				Kind:  "INNER",
				On:    &sqlast.Binary{Left: col("cur", "rn"), Operator: "=", Right: &sqlast.Binary{Left: col("prev", "rn"), Operator: "+", Right: number(1)}},
			},
			Where:       condition,
			Projections: projections,
		})
	}
	total := &sqlast.ScalarSubquery{Query: &sqlast.Select{
		From: &sqlast.Subquery{Query: numbered, Alias: s.nextAlias()}, Projections: []sqlast.SelectItem{{Expr: &sqlast.Call{Name: "count", Args: []sqlast.Expr{&sqlast.Star{}}}}},
	}}
	inInput := &sqlast.Binary{Left: col("cur", "rn"), Operator: "<=", Right: total}
	for k := n; k >= 1; k-- {
		mode := spec.Steps[k-1].Output
		if mode == "last" {
			// A new sequence replaces the pending record of this step.
			replaced := and(isTrue("cur", fmt.Sprintf("c1_%d", k)), isTrue("prev", fmt.Sprintf("a%d", k)))
			if k < n {
				replaced = and(replaced, not(isTrue("cur", fmt.Sprintf("c1_%d", k+1))))
			}
			emit(and(inInput, replaced), "prev", k)
		}
		if k > 1 && spec.Steps[k-2].Output == "last" {
			// Promotion ends the previous step's series.
			emit(and(inInput, isTrue("cur", fmt.Sprintf("c1_%d", k))), "prev", k-1)
		}
		if mode == "all" {
			emit(and(inInput, &sqlast.Binary{Left: isTrue("cur", fmt.Sprintf("c1_%d", k)), Operator: "OR", Right: isTrue("cur", fmt.Sprintf("c2_%d", k))}), "cur", k)
		}
	}
	for k := n; k >= 1; k-- {
		if spec.Steps[k-1].Output == "last" {
			emit(and(not(inInput), isTrue("cur", fmt.Sprintf("a%d", k))), "cur", k)
		}
	}
	finalColumns := make([]sqlast.SelectItem, len(output.Columns))
	for i, column := range output.Columns {
		finalColumns[i] = sqlast.SelectItem{Expr: &sqlast.Identifier{Parts: []string{column.Name}}, Alias: column.Name}
	}
	var body sqlast.Query
	if len(queries) == 0 {
		// Every step is output=none: no records.
		empty := &sqlast.Select{From: &sqlast.CTE{Name: "__scan", Alias: "cur"}, Where: falseExpr}
		for _, column := range output.Columns {
			empty.Projections = append(empty.Projections, sqlast.SelectItem{Expr: null, Alias: column.Name})
		}
		body = empty
	} else {
		var outputs sqlast.Query = queries[0]
		if len(queries) > 1 {
			outputs = &sqlast.Union{All: true, Queries: queries}
		}
		order := []sqlast.Order{{Expr: &sqlast.Identifier{Parts: []string{"__scan_order_rn"}}}, {Expr: &sqlast.Identifier{Parts: []string{"__scan_order_slot"}}}}
		body = &sqlast.Select{From: &sqlast.Subquery{Query: outputs, Alias: s.nextAlias()}, Projections: finalColumns, OrderBy: order}
	}
	query := &sqlast.RecursiveQuery{Name: "__scan", Columns: columns, Anchor: anchor, Step: step, Query: body}
	return Relation{Query: &sqlast.Select{From: &sqlast.Subquery{Query: query, Alias: s.nextAlias()}}, Schema: output, serialized: true, rowDependencies: input.rowDependencies}
}

func hasErrors(diagnostics []Diagnostic) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == SeverityError {
			return true
		}
	}
	return false
}

// kqlNames visits the name expressions of a KQL expression; call targets
// are function names and are not visited.
func kqlNames(expression kql.Expression, visit func(*kql.NameExpression)) {
	switch value := expression.(type) {
	case *kql.NameExpression:
		visit(value)
	case *kql.BinaryExpression:
		kqlNames(value.Left, visit)
		kqlNames(value.Right, visit)
	case *kql.UnaryExpression:
		kqlNames(value.Operand, visit)
	case *kql.CallExpression:
		for _, arg := range value.Args {
			kqlNames(arg, visit)
		}
	case *kql.IndexExpression:
		kqlNames(value.Value, visit)
		kqlNames(value.Index, visit)
	case *kql.ListExpression:
		for _, item := range value.Items {
			kqlNames(item, visit)
		}
	case *kql.NamedExpression:
		kqlNames(value.Value, visit)
	}
}

var sqlExprType = reflect.TypeOf((*sqlast.Expr)(nil)).Elem()

// substituteIdentifiers returns a copy of expr whose single-part
// identifiers are replaced where replace reports a substitute.
func substituteIdentifiers(expr sqlast.Expr, replace func(string) (sqlast.Expr, bool)) sqlast.Expr {
	if expr == nil {
		return nil
	}
	var copyValue func(reflect.Value) reflect.Value
	copyValue = func(value reflect.Value) reflect.Value {
		switch value.Kind() {
		case reflect.Interface:
			if value.IsNil() {
				return value
			}
			if identifier, ok := value.Interface().(*sqlast.Identifier); ok && len(identifier.Parts) == 1 && value.Type() == sqlExprType {
				if substitute, ok := replace(identifier.Parts[0]); ok {
					return reflect.ValueOf(&substitute).Elem()
				}
			}
			inner := copyValue(value.Elem())
			result := reflect.New(value.Type()).Elem()
			result.Set(inner)
			return result
		case reflect.Pointer:
			if value.IsNil() {
				return value
			}
			if value.Elem().Kind() != reflect.Struct {
				return value
			}
			result := reflect.New(value.Elem().Type())
			result.Elem().Set(copyValue(value.Elem()))
			return result
		case reflect.Struct:
			result := reflect.New(value.Type()).Elem()
			for i := 0; i < value.NumField(); i++ {
				if result.Field(i).CanSet() {
					result.Field(i).Set(copyValue(value.Field(i)))
				}
			}
			return result
		case reflect.Slice:
			if value.IsNil() {
				return value
			}
			result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
			for i := 0; i < value.Len(); i++ {
				result.Index(i).Set(copyValue(value.Index(i)))
			}
			return result
		}
		return value
	}
	if identifier, ok := expr.(*sqlast.Identifier); ok && len(identifier.Parts) == 1 {
		if substitute, ok := replace(identifier.Parts[0]); ok {
			return substitute
		}
	}
	return copyValue(reflect.ValueOf(&expr).Elem()).Interface().(sqlast.Expr)
}

// rowWindowSession lowers extend items Name = row_window_session(Expr,
// MaxDistanceFromFirst, MaxDistanceBetweenNeighbors [, Restart]) through
// scan: the inputs are extended first (so Restart may use prev()), then a
// one-step scan carries the session start and the previous value.
func (s *compileState) rowWindowSession(input Relation, operator kql.Operator) (Relation, bool) {
	spec, ok := operator.Body.(kql.ExpressionSpec)
	if !ok {
		return Relation{}, false
	}
	found := false
	for _, expression := range spec.Expressions {
		if call, ok := rowWindowSessionCall(expression); ok && call != nil {
			found = true
		}
	}
	if !found {
		return Relation{}, false
	}
	relation := input
	var pending []kql.Expression
	flush := func() {
		if len(pending) > 0 && relation.Query != nil {
			relation = s.serializedAfter(relation, s.operator(relation, kql.Operator{Kind: "extend", Body: kql.ExpressionSpec{Expressions: pending}, Span: operator.Span}), operator)
			pending = nil
		}
	}
	for index, expression := range spec.Expressions {
		named, isNamed := expression.(*kql.NamedExpression)
		call, ok := rowWindowSessionCall(expression)
		if !ok || call == nil {
			pending = append(pending, expression)
			continue
		}
		if !isNamed {
			s.bindError("KQLB0361", call.Span, "function.row_window_session", "row_window_session requires a column name: Name = row_window_session(...)")
			return Relation{}, true
		}
		if len(call.Args) < 3 || len(call.Args) > 4 {
			s.bindError("KQLB0361", call.Span, "function.row_window_session", "row_window_session requires Expr, MaxDistanceFromFirst, MaxDistanceBetweenNeighbors and an optional Restart")
			return Relation{}, true
		}
		flush()
		if relation.Query == nil {
			return Relation{}, true
		}
		value := fmt.Sprintf("__rws_value_%d", index)
		restart := fmt.Sprintf("__rws_restart_%d", index)
		previous := fmt.Sprintf("__rws_previous_%d", index)
		var restartValue kql.Expression = &kql.LiteralExpression{Text: "false", Kind: kql.IdentifierToken}
		if len(call.Args) == 4 {
			restartValue = call.Args[3]
		}
		relation = s.serializedAfter(relation, s.operator(relation, kql.Operator{Kind: "extend", Body: kql.ExpressionSpec{Expressions: []kql.Expression{
			&kql.NamedExpression{Name: value, Value: call.Args[0]},
			&kql.NamedExpression{Name: restart, Value: restartValue},
		}}, Span: operator.Span}), operator)
		if relation.Query == nil {
			return Relation{}, true
		}
		if column, _, found, _ := relation.Schema.Lookup(value); found && column.Type != TypeDateTime && column.Type != TypeUnknown {
			s.bindError("KQLB0361", call.Span, "function.row_window_session", "row_window_session requires a datetime expression")
			return Relation{}, true
		}
		name := func(text string) *kql.NameExpression { return &kql.NameExpression{Name: text} }
		ref := func(text string) *kql.NameExpression { return &kql.NameExpression{Name: "s." + text} }
		isnull := func(expression kql.Expression) kql.Expression {
			return &kql.CallExpression{Function: name("isnull"), Args: []kql.Expression{expression}}
		}
		or := func(left, right kql.Expression) kql.Expression {
			return &kql.BinaryExpression{Left: left, Operator: "or", Right: right}
		}
		// A new session starts without a previous session or value, at or
		// beyond either distance, or on restart.
		target := unquoteKQL(named.Name)
		fromFirst := &kql.BinaryExpression{Left: name(value), Operator: ">=", Right: &kql.BinaryExpression{Left: ref(searchName(target)), Operator: "+", Right: call.Args[1]}}
		fromNeighbor := &kql.BinaryExpression{Left: name(value), Operator: ">=", Right: &kql.BinaryExpression{Left: ref(previous), Operator: "+", Right: call.Args[2]}}
		starts := or(or(or(or(isnull(ref(searchName(target))), isnull(ref(previous))), fromFirst), fromNeighbor), name(restart))
		session := &kql.CallExpression{Function: name("iff"), Args: []kql.Expression{starts, name(value), ref(searchName(target))}}
		scan := kql.ScanSpec{
			Declares: []kql.ScanDeclare{{Name: target, Type: "datetime"}, {Name: previous, Type: "datetime"}},
			Steps: []kql.ScanStep{{Name: "s", Output: "all", Condition: &kql.LiteralExpression{Text: "true", Kind: kql.IdentifierToken},
				Assignments: []*kql.NamedExpression{{Name: target, Value: session}, {Name: previous, Value: name(value)}}}},
		}
		if _, _, exists, _ := relation.Schema.Lookup(target); exists {
			// Replacing a column: drop it so scan can declare it.
			relation = s.operator(relation, kql.Operator{Kind: "project-away", Body: kql.ExpressionSpec{Expressions: []kql.Expression{name(searchName(target))}}, Span: operator.Span})
			if relation.Query == nil {
				return Relation{}, true
			}
		}
		relation = s.scan(relation, kql.Operator{Kind: "scan", Body: scan, Span: operator.Span})
		if relation.Query == nil {
			return Relation{}, true
		}
		relation = s.operator(relation, kql.Operator{Kind: "project-away", Body: kql.ExpressionSpec{Expressions: []kql.Expression{name(value), name(restart), name(previous)}}, Span: operator.Span})
		relation.serialized = true
		if relation.Query == nil {
			return Relation{}, true
		}
	}
	flush()
	return relation, true
}

func rowWindowSessionCall(expression kql.Expression) (*kql.CallExpression, bool) {
	if named, ok := expression.(*kql.NamedExpression); ok {
		expression = named.Value
	}
	call, ok := expression.(*kql.CallExpression)
	if !ok {
		return nil, true
	}
	name, ok := call.Function.(*kql.NameExpression)
	if !ok || !strings.EqualFold(unquoteKQL(name.Name), "row_window_session") {
		return nil, true
	}
	return call, true
}
