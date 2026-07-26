package ksql

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/kawijayaa/ksql/dialect"
	featureledger "github.com/kawijayaa/ksql/features"
	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// Compiler translates KQL into one configured SQL dialect. A Compiler is safe
// for concurrent use after construction.
type Compiler struct {
	dialect       dialect.Dialect
	functionRules map[string]FunctionRule
	operatorRules map[string]OperatorRule
	sourceRules   map[string]SourceRule
}

// New constructs a compiler for target. A nil target selects ANSI SQL.
func New(target dialect.Dialect, options ...Option) *Compiler {
	if target == nil {
		target = dialect.ANSI()
	}
	compiler := &Compiler{
		dialect: target, functionRules: make(map[string]FunctionRule),
		operatorRules: make(map[string]OperatorRule), sourceRules: make(map[string]SourceRule),
	}
	for _, option := range options {
		if option != nil {
			option(compiler)
		}
	}
	return compiler
}

// Result contains either SQL or error diagnostics. SQL is empty whenever an
// error diagnostic is present.
type Result struct {
	SQL         string
	Args        []any
	Tree        *kql.Script
	Diagnostics []Diagnostic
	Features    []FeatureUse
}

// OK reports whether compilation completed without an error.
func (r Result) OK() bool {
	for _, diagnostic := range r.Diagnostics {
		if diagnostic.Severity == SeverityError {
			return false
		}
	}
	return true
}

// Compile parses and translates source. The method never emits partial SQL.
func (c *Compiler) Compile(source string) Result {
	tree, parseErrors := kql.Parse(source)
	result := Result{Tree: tree}
	for _, parseError := range parseErrors {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{
			Code: parseError.Code, Severity: SeverityError, Phase: PhaseParse,
			Message: parseError.Message, Span: parseError.Span,
		})
	}
	if !result.OK() {
		return result
	}

	state := compileState{compiler: c, scalarLets: make(map[string]sqlast.Expr), tabularLets: make(map[string]sqlast.Query)}
	var query sqlast.Query
	for _, statement := range tree.Statements {
		switch node := statement.(type) {
		case *kql.LetStatement:
			if node.Pipeline != nil {
				value := state.pipeline(node.Pipeline)
				if value != nil {
					state.tabularLets[strings.ToLower(node.Name)] = value
				}
			} else if node.Value != nil {
				value := state.expression(node.Value)
				if value != nil {
					state.scalarLets[strings.ToLower(node.Name)] = value
				}
			} else {
				state.error("KQLL0101", node.Span, "statement.let", "function declarations require semantic parameter binding")
			}
			state.use("statement.let", "equivalent", node.Span)
		case *kql.ControlStatement:
			state.error("KQLL0102", node.Span, "statement."+node.Kind, node.Kind+" is a KQL session/control statement with no SQL query equivalent")
		case *kql.ExpressionStatement:
			if query != nil {
				state.error("KQLL0103", node.Span, "statement.expression", "multiple result-producing statements cannot be represented by one SQL query")
				continue
			}
			query = state.pipeline(node.Pipeline)
		}
	}
	result.Diagnostics = state.diagnostics
	result.Features = state.features
	if !result.OK() || query == nil {
		return result
	}

	sql, err := dialect.Render(c.dialect, query)
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{
			Code: "SQLD0001", Severity: SeverityError, Phase: PhaseSQL,
			Message: err.Error(),
		})
		return result
	}
	result.SQL = sql
	return result
}

type compileState struct {
	compiler    *Compiler
	scalarLets  map[string]sqlast.Expr
	tabularLets map[string]sqlast.Query
	diagnostics []Diagnostic
	features    []FeatureUse
	alias       int
	qualifiers  map[string]string
	qualifier   string
}

func (s *compileState) pipeline(pipeline *kql.Pipeline) sqlast.Query {
	if pipeline == nil {
		return nil
	}
	query := s.source(pipeline.Source)
	for _, operator := range pipeline.Operators {
		if query == nil {
			return nil
		}
		query = s.operator(query, operator)
	}
	return query
}

func (s *compileState) source(source kql.Source) sqlast.Query {
	s.use("source."+source.Kind, "equivalent", source.Span)
	if rule, ok := s.compiler.sourceRules[strings.ToLower(source.Kind)]; ok {
		query, err := rule(source)
		if err != nil {
			s.error("KQLL0203", source.Span, "source."+source.Kind, err.Error())
			return nil
		}
		if query == nil {
			s.error("KQLL0204", source.Span, "source."+source.Kind, "custom source rule returned a nil query")
		}
		return query
	}
	switch source.Kind {
	case "table":
		if bound, ok := s.tabularLets[strings.ToLower(source.Name)]; ok {
			return bound
		}
		return &sqlast.Select{From: &sqlast.Table{Parts: splitName(source.Name)}}
	case "print":
		selectItems := s.selectItems(source.Items, true)
		return &sqlast.Select{Projections: selectItems}
	case "datatable":
		columns := make([]string, len(source.Columns))
		for i, column := range source.Columns {
			columns[i] = column.Name
		}
		rows := make([][]sqlast.Expr, len(source.Rows))
		for i, row := range source.Rows {
			rows[i] = s.expressions(row)
		}
		return &sqlast.Select{From: &sqlast.Values{Columns: columns, Rows: rows, Alias: s.nextAlias()}}
	case "range":
		return &sqlast.Select{From: &sqlast.Series{Column: source.Name, From: s.expression(source.From), To: s.expression(source.To), Step: s.expression(source.Step), Alias: s.nextAlias()}}
	case "expression":
		if call, ok := source.Expr.(*kql.CallExpression); ok {
			if table := s.entityCall(call); table != nil {
				return &sqlast.Select{From: table}
			}
		}
		s.error("KQLL0201", source.Span, "source.expression", "expression is not a supported tabular source")
	default:
		s.error("KQLL0202", source.Span, "source."+source.Kind, source.Kind+" has no generic SQL source equivalent")
	}
	return nil
}

func (s *compileState) entityCall(call *kql.CallExpression) *sqlast.Table {
	name, ok := call.Function.(*kql.NameExpression)
	if !ok || len(call.Args) != 1 {
		return nil
	}
	function := strings.ToLower(name.Name)
	if function != "table" && function != "external_table" && function != "materialized_view" {
		return nil
	}
	literal, ok := call.Args[0].(*kql.LiteralExpression)
	if !ok || literal.Kind != kql.StringToken {
		return nil
	}
	return &sqlast.Table{Parts: []string{unquoteKQL(literal.Text)}}
}

func (s *compileState) operator(input sqlast.Query, operator kql.Operator) sqlast.Query {
	feature := "operator." + operator.Kind
	s.use(feature, "equivalent", operator.Span)
	if rule, ok := s.compiler.operatorRules[strings.ToLower(operator.Kind)]; ok {
		query, err := rule(input, operator)
		if err != nil {
			s.error("KQLL0303", operator.Span, feature, err.Error())
			return nil
		}
		if query == nil {
			s.error("KQLL0304", operator.Span, feature, "custom operator rule returned a nil query")
		}
		return query
	}
	if operator.Kind == "join" || operator.Kind == "lookup" {
		return s.join(input, operator)
	}
	if operator.Kind == "union" {
		return s.union(input, operator)
	}
	if operator.Kind == "mv-expand" {
		return s.mvExpand(input, operator.Body.(kql.MvExpandSpec), operator)
	}
	if operator.Kind == "mv-apply" {
		return s.mvApply(input, operator.Body.(kql.MvApplySpec), operator)
	}
	if operator.Kind == "as" {
		spec := operator.Body.(kql.AsSpec)
		if spec.Materialized != nil && *spec.Materialized {
			s.error("KQLL0306", operator.Span, feature, "as hint.materialized=true requires guaranteed single evaluation")
			return nil
		}
		s.tabularLets[strings.ToLower(spec.Name)] = input
		return input
	}
	from := &sqlast.Subquery{Query: input, Alias: s.nextAlias()}
	switch operator.Kind {
	case "where":
		spec := operator.Body.(kql.ExpressionSpec)
		return &sqlast.Select{From: from, Where: s.expression(spec.Expressions[0])}
	case "project":
		spec := operator.Body.(kql.ExpressionSpec)
		return &sqlast.Select{From: from, Projections: s.selectItems(spec.Expressions, false)}
	case "extend":
		spec := operator.Body.(kql.ExpressionSpec)
		items := []sqlast.SelectItem{{Expr: &sqlast.Star{}}}
		items = append(items, s.selectItems(spec.Expressions, true)...)
		return &sqlast.Select{From: from, Projections: items}
	case "summarize":
		spec := operator.Body.(kql.SummarizeSpec)
		groups := s.expressions(spec.By)
		items := s.selectItems(spec.By, true)
		items = append(items, s.selectItems(spec.Aggregates, true)...)
		return &sqlast.Select{From: from, Projections: items, GroupBy: groups}
	case "distinct":
		spec := operator.Body.(kql.ExpressionSpec)
		return &sqlast.Select{Distinct: true, From: from, Projections: s.selectItems(spec.Expressions, false)}
	case "count":
		alias := "Count"
		if raw, ok := operator.Body.(kql.RawSpec); ok && len(raw.Tokens) == 2 && strings.EqualFold(raw.Tokens[0].Text, "as") {
			alias = raw.Tokens[1].Text
		}
		return &sqlast.Select{From: from, Projections: []sqlast.SelectItem{{Expr: &sqlast.Call{Name: "count", Args: []sqlast.Expr{&sqlast.Star{}}}, Alias: alias}}}
	case "sort":
		spec := operator.Body.(kql.SortSpec)
		orders := make([]sqlast.Order, len(spec.Expressions))
		for i, order := range spec.Expressions {
			orders[i] = sqlast.Order{Expr: s.expression(order.Expression), Direction: order.Direction, Nulls: order.Nulls}
		}
		return &sqlast.Select{From: from, OrderBy: orders}
	case "take":
		spec := operator.Body.(kql.ExpressionSpec)
		return &sqlast.Select{From: from, Limit: s.expression(spec.Expressions[0])}
	case "top":
		spec := operator.Body.(kql.TopSpec)
		return &sqlast.Select{From: from, OrderBy: []sqlast.Order{{Expr: s.expression(spec.By.Expression), Direction: spec.By.Direction, Nulls: spec.By.Nulls}}, Limit: s.expression(spec.Count)}
	case "sample":
		if !supportsRandom(s.compiler.dialect) {
			s.error("KQLL0305", operator.Span, feature, "dialect "+s.compiler.dialect.Name()+" has no random sampling expression")
			return nil
		}
		spec := operator.Body.(kql.ExpressionSpec)
		return &sqlast.Select{From: from, OrderBy: []sqlast.Order{{Expr: &sqlast.Random{}}}, Limit: s.expression(spec.Expressions[0])}
	case "sample-distinct":
		if !supportsRandom(s.compiler.dialect) {
			s.error("KQLL0305", operator.Span, feature, "dialect "+s.compiler.dialect.Name()+" has no random sampling expression")
			return nil
		}
		spec := operator.Body.(kql.SampleDistinctSpec)
		inner := &sqlast.Select{Distinct: true, From: from, Projections: []sqlast.SelectItem{{Expr: s.expression(spec.Column)}}}
		return &sqlast.Select{From: &sqlast.Subquery{Query: inner, Alias: s.nextAlias()}, OrderBy: []sqlast.Order{{Expr: &sqlast.Random{}}}, Limit: s.expression(spec.Count)}
	case "serialize":
		s.warn("KQLL0301", operator.Span, feature, "serialize metadata is unnecessary in SQL; row order still requires an explicit sort")
		return &sqlast.Select{From: from}
	default:
		s.error("KQLL0302", operator.Span, feature, operator.Kind+" is recognized but has no enabled lowering for "+s.compiler.dialect.Name())
		return nil
	}
}

func (s *compileState) selectItems(expressions []kql.Expression, inferAlias bool) []sqlast.SelectItem {
	items := make([]sqlast.SelectItem, 0, len(expressions))
	for _, expression := range expressions {
		item := sqlast.SelectItem{}
		if named, ok := expression.(*kql.NamedExpression); ok {
			item.Expr = s.expression(named.Value)
			item.Alias = named.Name
		} else {
			item.Expr = s.expression(expression)
			if inferAlias {
				switch value := expression.(type) {
				case *kql.NameExpression:
					item.Alias = lastName(value.Name)
				case *kql.CallExpression:
					if function, ok := value.Function.(*kql.NameExpression); ok {
						item.Alias = lastName(function.Name)
					}
				}
			}
		}
		if item.Expr != nil {
			items = append(items, item)
		}
	}
	return items
}

func (s *compileState) expressions(expressions []kql.Expression) []sqlast.Expr {
	result := make([]sqlast.Expr, 0, len(expressions))
	for _, expression := range expressions {
		if value := s.expression(expression); value != nil {
			result = append(result, value)
		}
	}
	return result
}

func (s *compileState) expression(expression kql.Expression) sqlast.Expr {
	if expression == nil {
		return nil
	}
	switch value := expression.(type) {
	case *kql.NameExpression:
		if bound, ok := s.scalarLets[strings.ToLower(value.Name)]; ok {
			return bound
		}
		parts := splitName(unquoteKQL(value.Name))
		if len(parts) > 1 && s.qualifiers != nil {
			if qualifier, ok := s.qualifiers[strings.ToLower(parts[0])]; ok {
				parts[0] = qualifier
			}
		} else if len(parts) == 1 && s.qualifier != "" {
			parts = append([]string{s.qualifier}, parts...)
		}
		return &sqlast.Identifier{Parts: parts}
	case *kql.LiteralExpression:
		return lowerLiteral(value)
	case *kql.StarExpression:
		return &sqlast.Star{}
	case *kql.UnaryExpression:
		return &sqlast.Unary{Operator: value.Operator, Operand: s.expression(value.Operand)}
	case *kql.BinaryExpression:
		return s.binary(value)
	case *kql.CallExpression:
		return s.call(value)
	case *kql.IndexExpression:
		return &sqlast.Index{Value: s.expression(value.Value), Index: s.expression(value.Index)}
	case *kql.NamedExpression:
		return s.expression(value.Value)
	case *kql.ListExpression:
		return &sqlast.List{Items: s.expressions(value.Items)}
	case *kql.RawExpression:
		s.error("KQLL0401", value.Span, "expression.raw", "expression syntax is preserved but cannot be lowered")
		return nil
	default:
		s.error("KQLL0402", expression.NodeSpan(), "expression.unknown", fmt.Sprintf("unsupported expression node %T", expression))
		return nil
	}
}

func (s *compileState) join(input sqlast.Query, operator kql.Operator) sqlast.Query {
	spec := operator.Body.(kql.JoinSpec)
	leftAlias := s.nextAlias()
	rightQuery := s.pipeline(spec.Right)
	if rightQuery == nil {
		return nil
	}
	rightAlias := s.nextAlias()
	join := &sqlast.Join{
		Left:  &sqlast.Subquery{Query: input, Alias: leftAlias},
		Right: &sqlast.Subquery{Query: rightQuery, Alias: rightAlias},
	}
	switch spec.Kind {
	case "inner":
		join.Kind = "INNER"
	case "leftouter":
		join.Kind = "LEFT"
	case "rightouter":
		join.Kind = "RIGHT"
	case "fullouter":
		join.Kind = "FULL"
	case "innerunique":
		s.error("KQLL0310", operator.Span, "operator.join.innerunique", "innerunique deduplicates the left input and requires schema-aware window lowering")
		return nil
	default:
		s.error("KQLL0311", operator.Span, "operator."+operator.Kind+"."+spec.Kind, "join kind "+spec.Kind+" requires semi/anti relational lowering")
		return nil
	}
	if len(spec.Conditions) > 0 {
		allNames := true
		for _, condition := range spec.Conditions {
			name, ok := condition.(*kql.NameExpression)
			if !ok || strings.Contains(name.Name, ".") {
				allNames = false
				break
			}
			join.Using = append(join.Using, unquoteKQL(name.Name))
		}
		if !allNames {
			join.Using = nil
			previous := s.qualifiers
			s.qualifiers = map[string]string{"$left": leftAlias, "$right": rightAlias}
			for _, condition := range spec.Conditions {
				translated := s.expression(condition)
				if join.On == nil {
					join.On = translated
				} else {
					join.On = &sqlast.Binary{Left: join.On, Operator: "AND", Right: translated}
				}
			}
			s.qualifiers = previous
		}
	} else if spec.Condition != nil {
		previous := s.qualifiers
		s.qualifiers = map[string]string{"$left": leftAlias, "$right": rightAlias}
		join.On = s.expression(spec.Condition)
		s.qualifiers = previous
	}
	return &sqlast.Select{From: join}
}

func (s *compileState) union(input sqlast.Query, operator kql.Operator) sqlast.Query {
	spec := operator.Body.(kql.UnionSpec)
	queries := []sqlast.Query{input}
	for _, pipeline := range spec.Inputs {
		query := s.pipeline(pipeline)
		if query == nil {
			return nil
		}
		queries = append(queries, query)
	}
	if spec.Kind == "outer" {
		s.warn("KQLL0320", operator.Span, "operator.union.outer", "SQL UNION aligns columns by position; KQL outer union aligns and qualifies them by name and type")
	} else if spec.Kind != "inner" {
		s.error("KQLL0321", operator.Span, "operator.union."+spec.Kind, "unknown union kind "+spec.Kind)
		return nil
	} else {
		s.warn("KQLL0322", operator.Span, "operator.union.inner", "SQL UNION requires inputs to already have the same projected schema")
	}
	return &sqlast.Union{All: true, Queries: queries}
}

func (s *compileState) mvExpand(input sqlast.Query, spec kql.MvExpandSpec, operator kql.Operator) sqlast.Query {
	return s.expandJSON(input, spec.Items, spec.ItemIndex, spec.Limit, operator)
}

func (s *compileState) mvApply(input sqlast.Query, spec kql.MvApplySpec, operator kql.Operator) sqlast.Query {
	query := s.expandJSON(input, spec.Items, spec.ItemIndex, spec.Limit, operator)
	if query == nil {
		return nil
	}
	for _, inner := range spec.Operators {
		switch inner.Kind {
		case "where", "extend", "serialize":
			query = s.operator(query, inner)
		default:
			s.error("KQLL0334", inner.Span, "operator.mv-apply."+inner.Kind, "SQLite mv-apply currently supports where, extend, and serialize subqueries")
			return nil
		}
		if query == nil {
			return nil
		}
	}
	return query
}

func (s *compileState) expandJSON(input sqlast.Query, items []kql.MvExpansion, itemIndex string, limit kql.Expression, operator kql.Operator) sqlast.Query {
	if !supportsJSONEach(s.compiler.dialect) {
		s.error("KQLL0330", operator.Span, "operator."+operator.Kind, "dialect "+s.compiler.dialect.Name()+" has no JSON array expansion")
		return nil
	}
	if len(items) != 1 {
		s.error("KQLL0331", operator.Span, "operator."+operator.Kind, operator.Kind+" currently requires exactly one array expression")
		return nil
	}
	item := items[0]
	sourceName := ""
	if name, ok := item.Expression.(*kql.NameExpression); ok {
		sourceName = name.Name
	}
	if item.Name == "" {
		s.error("KQLL0332", item.Span, "operator."+operator.Kind, operator.Kind+" requires an explicit output name for non-column expressions")
		return nil
	}
	if strings.EqualFold(item.Name, sourceName) {
		s.error("KQLL0333", item.Span, "operator."+operator.Kind, "SQLite cannot replace a column during JSON expansion without the input schema; use a distinct output alias")
		return nil
	}

	inputAlias, expansionAlias := s.nextAlias(), s.nextAlias()
	previousQualifier := s.qualifier
	s.qualifier = inputAlias
	value := s.expression(item.Expression)
	s.qualifier = previousQualifier
	expansion := &sqlast.JSONEach{Value: value, Alias: expansionAlias}
	from := &sqlast.Join{
		Left:  &sqlast.Subquery{Query: input, Alias: inputAlias},
		Right: expansion,
		Kind:  "CROSS",
	}
	element := sqlast.Expr(&sqlast.Identifier{Parts: []string{expansionAlias, "value"}})
	if item.Type != "" {
		types := map[string]string{
			"long": "BIGINT", "int": "INTEGER", "real": "DOUBLE PRECISION", "double": "DOUBLE PRECISION",
			"decimal": "DECIMAL", "string": "VARCHAR", "bool": "BOOLEAN", "boolean": "BOOLEAN",
			"datetime": "TIMESTAMP", "guid": "UUID",
		}
		target, ok := types[item.Type]
		if !ok {
			s.error("KQLL0335", item.Span, "operator."+operator.Kind, "unsupported expansion type "+item.Type)
			return nil
		}
		s.warn("KQLL0336", item.Span, "operator."+operator.Kind, "SQLite casts invalid expanded values instead of returning null")
		element = &sqlast.Cast{Expr: element, Type: target}
	}
	projections := []sqlast.SelectItem{
		{Expr: &sqlast.QualifiedStar{Parts: []string{inputAlias}}},
		{Expr: element, Alias: item.Name},
	}
	if itemIndex != "" {
		projections = append(projections, sqlast.SelectItem{
			Expr:  &sqlast.Cast{Expr: &sqlast.Identifier{Parts: []string{expansionAlias, "key"}}, Type: "INTEGER"},
			Alias: itemIndex,
		})
	}
	query := &sqlast.Select{From: from, Projections: projections}
	if limit != nil {
		query.Where = &sqlast.Binary{
			Left:     &sqlast.Cast{Expr: &sqlast.Identifier{Parts: []string{expansionAlias, "key"}}, Type: "INTEGER"},
			Operator: "<",
			Right:    s.expression(limit),
		}
	}
	return query
}

func supportsRandom(target dialect.Dialect) bool {
	capability, ok := target.(interface{ Random() (string, bool) })
	if !ok {
		return false
	}
	_, ok = capability.Random()
	return ok
}

func supportsJSONEach(target dialect.Dialect) bool {
	capability, ok := target.(interface {
		JSONEach(string, string) (string, bool)
	})
	if !ok {
		return false
	}
	_, ok = capability.JSONEach("", "")
	return ok
}

func supportsRegex(target dialect.Dialect) bool {
	_, ok := target.Regex("", "", true)
	return ok
}

func (s *compileState) binary(value *kql.BinaryExpression) sqlast.Expr {
	left, right := s.expression(value.Left), s.expression(value.Right)
	op := strings.ToLower(value.Operator)
	s.use(scalarOperatorID(op), "equivalent", value.Span)
	switch op {
	case "==":
		op = "="
	case "!=", "<>":
		op = "<>"
	case "and", "or":
		op = strings.ToUpper(op)
	case "=~", "!~":
		comparison := "="
		if op == "!~" {
			comparison = "<>"
		}
		return &sqlast.Binary{Left: &sqlast.Call{Name: "lower", Args: []sqlast.Expr{left}}, Operator: comparison, Right: &sqlast.Call{Name: "lower", Args: []sqlast.Expr{right}}}
	case "in", "!in":
		if op == "!in" {
			op = "NOT IN"
		} else {
			op = "IN"
		}
	case "in~", "!in~":
		list, ok := right.(*sqlast.List)
		if !ok || len(list.Items) == 0 {
			s.error("KQLL0403", value.Span, "operator.scalar."+op, op+" requires a non-empty scalar value list")
			return nil
		}
		items := make([]sqlast.Expr, len(list.Items))
		for i, item := range list.Items {
			items[i] = &sqlast.Call{Name: "lower", Args: []sqlast.Expr{item}}
		}
		operator := "IN"
		if op == "!in~" {
			operator = "NOT IN"
		}
		return &sqlast.Binary{Left: &sqlast.Call{Name: "lower", Args: []sqlast.Expr{left}}, Operator: operator, Right: &sqlast.List{Items: items}}
	case "between", "!between":
		list, ok := right.(*sqlast.List)
		if !ok || len(list.Items) != 2 {
			return nil
		}
		between := &sqlast.Binary{Left: left, Operator: "BETWEEN", Right: &sqlast.Binary{Left: list.Items[0], Operator: "AND", Right: list.Items[1]}}
		if op == "!between" {
			return &sqlast.Unary{Operator: "NOT ", Operand: between}
		}
		return between
	case "contains", "contains_cs", "containscs", "!contains", "!contains_cs", "notcontains", "notcontainscs":
		caseSensitive := strings.Contains(op, "_cs") || strings.HasSuffix(op, "cs")
		negative := strings.HasPrefix(op, "!") || strings.HasPrefix(op, "not")
		return stringMatch(left, right, "both", caseSensitive, negative)
	case "startswith", "startswith_cs", "!startswith", "!startswith_cs":
		return stringMatch(left, right, "suffix", strings.Contains(op, "_cs"), strings.HasPrefix(op, "!"))
	case "endswith", "endswith_cs", "!endswith", "!endswith_cs":
		return stringMatch(left, right, "prefix", strings.Contains(op, "_cs"), strings.HasPrefix(op, "!"))
	case "matches regex":
		return &sqlast.Regex{Value: left, Pattern: right, CaseSensitive: true}
	case "has", "!has", "has_cs", "!has_cs", "hasprefix", "!hasprefix", "hasprefix_cs", "!hasprefix_cs", "hassuffix", "!hassuffix", "hassuffix_cs", "!hassuffix_cs":
		if !supportsRegex(s.compiler.dialect) {
			s.error("KQLL0404", value.Span, "operator.scalar."+op, op+" requires a regex-capable SQL dialect")
			return nil
		}
		mode := "term"
		if strings.Contains(op, "prefix") {
			mode = "prefix"
		} else if strings.Contains(op, "suffix") {
			mode = "suffix"
		}
		result, ok := termMatch(left, right, mode, strings.Contains(op, "_cs"), strings.HasPrefix(op, "!"))
		if !ok {
			s.error("KQLL0404", value.Span, "operator.scalar."+op, op+" currently requires one non-empty alphanumeric literal term")
			return nil
		}
		return result
	case "has_any", "has_all":
		if !supportsRegex(s.compiler.dialect) {
			s.error("KQLL0404", value.Span, "operator.scalar."+op, op+" requires a regex-capable SQL dialect")
			return nil
		}
		list, ok := right.(*sqlast.List)
		if !ok || len(list.Items) == 0 {
			s.error("KQLL0404", value.Span, "operator.scalar."+op, op+" requires a non-empty literal term list")
			return nil
		}
		operator := "OR"
		if op == "has_all" {
			operator = "AND"
		}
		var result sqlast.Expr
		for _, item := range list.Items {
			match, matched := termMatch(left, item, "term", false, false)
			if !matched {
				s.error("KQLL0404", value.Span, "operator.scalar."+op, op+" currently requires non-empty alphanumeric literal terms")
				return nil
			}
			if result == nil {
				result = match
			} else {
				result = &sqlast.Binary{Left: result, Operator: operator, Right: match}
			}
		}
		return result
	case ":":
		s.error("KQLL0404", value.Span, "operator.scalar.colon", ": search semantics require input schema information")
		return nil
	}
	return &sqlast.Binary{Left: left, Operator: op, Right: right}
}

func stringMatch(left, right sqlast.Expr, wildcard string, caseSensitive, negative bool) sqlast.Expr {
	patternArgs := []sqlast.Expr{}
	if wildcard == "both" || wildcard == "prefix" {
		patternArgs = append(patternArgs, &sqlast.Literal{Kind: sqlast.StringLiteral, Value: "%"})
	}
	patternArgs = append(patternArgs, right)
	if wildcard == "both" || wildcard == "suffix" {
		patternArgs = append(patternArgs, &sqlast.Literal{Kind: sqlast.StringLiteral, Value: "%"})
	}
	pattern := sqlast.Expr(&sqlast.Call{Name: "concat", Args: patternArgs})
	if !caseSensitive {
		left = &sqlast.Call{Name: "lower", Args: []sqlast.Expr{left}}
		pattern = &sqlast.Call{Name: "lower", Args: []sqlast.Expr{pattern}}
	}
	op := "LIKE"
	if negative {
		op = "NOT LIKE"
	}
	return &sqlast.Binary{Left: left, Operator: op, Right: pattern}
}

func termMatch(left, right sqlast.Expr, mode string, caseSensitive, negative bool) (sqlast.Expr, bool) {
	literal, ok := right.(*sqlast.Literal)
	if !ok || literal.Kind != sqlast.StringLiteral || literal.Value == "" {
		return nil, false
	}
	for _, r := range literal.Value {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			return nil, false
		}
	}
	term := literal.Value
	leftBoundary, rightBoundary := `(^|[^[:alnum:]])`, `([^[:alnum:]]|$)`
	pattern := leftBoundary + term + rightBoundary
	switch mode {
	case "prefix":
		pattern = leftBoundary + term + `[[:alnum:]]*`
	case "suffix":
		pattern = leftBoundary + `[[:alnum:]]*` + term + rightBoundary
	}
	result := sqlast.Expr(&sqlast.Regex{
		Value:         left,
		Pattern:       &sqlast.Literal{Kind: sqlast.StringLiteral, Value: pattern},
		CaseSensitive: caseSensitive,
	})
	if negative {
		result = &sqlast.Unary{Operator: "NOT ", Operand: result}
	}
	return result, true
}

func lowerLiteral(value *kql.LiteralExpression) sqlast.Expr {
	text := value.Text
	if value.Kind == kql.StringToken {
		return &sqlast.Literal{Kind: sqlast.StringLiteral, Value: unquoteKQL(text)}
	}
	if strings.EqualFold(text, "true") || strings.EqualFold(text, "false") {
		return &sqlast.Literal{Kind: sqlast.BooleanLiteral, Value: strings.ToLower(text)}
	}
	if strings.EqualFold(text, "null") {
		return &sqlast.Literal{Kind: sqlast.NullLiteral}
	}
	if isTimespan(text) {
		return &sqlast.Literal{Kind: sqlast.IntervalLiteral, Value: text}
	}
	return &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: text}
}

func (s *compileState) call(value *kql.CallExpression) sqlast.Expr {
	name, ok := value.Function.(*kql.NameExpression)
	if !ok {
		s.error("KQLL0501", value.Span, "function.dynamic", "dynamic function references are not supported")
		return nil
	}
	function := strings.ToLower(name.Name)
	args := s.expressions(value.Args)
	s.use("function."+function, "equivalent", value.Span)
	if rule, ok := s.compiler.functionRules[function]; ok {
		result, err := rule(args)
		if err != nil {
			s.error("KQLL0504", value.Span, "function."+function, err.Error())
			return nil
		}
		if result == nil {
			s.error("KQLL0505", value.Span, "function."+function, "custom function rule returned a nil expression")
		}
		return result
	}
	switch function {
	case "iif", "iff":
		if len(args) != 3 {
			return s.arity(value, function, 3)
		}
		return &sqlast.Case{Branches: []sqlast.When{{Condition: args[0], Result: args[1]}}, Else: args[2]}
	case "case":
		if len(args) < 3 || len(args)%2 == 0 {
			s.error("KQLB0502", value.Span, "function.case", "case requires condition/result pairs and an else expression")
			return nil
		}
		result := &sqlast.Case{Else: args[len(args)-1]}
		for i := 0; i < len(args)-1; i += 2 {
			result.Branches = append(result.Branches, sqlast.When{Condition: args[i], Result: args[i+1]})
		}
		return result
	case "datetime", "date":
		if len(value.Args) != 1 {
			return s.arity(value, function, 1)
		}
		return &sqlast.Literal{Kind: sqlast.DateTimeLiteral, Value: expressionText(value.Args[0])}
	case "timespan", "time":
		if len(value.Args) != 1 {
			return s.arity(value, function, 1)
		}
		return &sqlast.Literal{Kind: sqlast.IntervalLiteral, Value: expressionText(value.Args[0])}
	case "int", "int32", "long", "int64", "real", "double", "decimal":
		if len(args) != 1 {
			return s.arity(value, function, 1)
		}
		types := map[string]string{"int": "INTEGER", "int32": "INTEGER", "long": "BIGINT", "int64": "BIGINT", "real": "DOUBLE PRECISION", "double": "DOUBLE PRECISION", "decimal": "DECIMAL"}
		return &sqlast.Cast{Expr: args[0], Type: types[function]}
	case "tostring":
		if len(args) != 1 {
			return s.arity(value, function, 1)
		}
		return &sqlast.Cast{Expr: args[0], Type: "VARCHAR"}
	case "toint", "tolong", "toreal", "todouble", "todatetime", "todecimal", "tobool", "toboolean", "toguid":
		if len(args) != 1 {
			return s.arity(value, function, 1)
		}
		types := map[string]string{"toint": "INTEGER", "tolong": "BIGINT", "toreal": "DOUBLE PRECISION", "todouble": "DOUBLE PRECISION", "todatetime": "TIMESTAMP", "todecimal": "DECIMAL", "tobool": "BOOLEAN", "toboolean": "BOOLEAN", "toguid": "UUID"}
		return &sqlast.Cast{Expr: args[0], Type: types[function], Safe: true}
	case "strcat":
		return &sqlast.Call{Name: "concat", Args: args}
	case "strlen":
		return &sqlast.Call{Name: "length", Args: args}
	case "tolower":
		return &sqlast.Call{Name: "lower", Args: args}
	case "toupper":
		return &sqlast.Call{Name: "upper", Args: args}
	case "substring":
		return &sqlast.Call{Name: "substr", Args: args}
	case "countif":
		if len(args) != 1 {
			return s.arity(value, function, 1)
		}
		return &sqlast.Call{Name: "sum", Args: []sqlast.Expr{&sqlast.Case{Branches: []sqlast.When{{Condition: args[0], Result: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "1"}}}, Else: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "0"}}}}
	case "sumif":
		if len(args) != 2 {
			return s.arity(value, function, 2)
		}
		return &sqlast.Call{Name: "sum", Args: []sqlast.Expr{&sqlast.Case{Branches: []sqlast.When{{Condition: args[1], Result: args[0]}}, Else: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "0"}}}}
	case "count":
		if len(args) == 0 {
			args = []sqlast.Expr{&sqlast.Star{}}
		}
		return &sqlast.Call{Name: function, Args: args}
	case "sum", "avg", "min", "max", "coalesce", "abs", "ceiling", "ceil", "floor", "round", "pow", "sqrt", "exp", "ln", "log", "log10", "sin", "cos", "tan", "asin", "acos", "atan", "atan2", "sign":
		return &sqlast.Call{Name: function, Args: args}
	default:
		s.error("KQLL0503", value.Span, "function."+function, "function "+function+" is recognized syntactically but has no safe SQL mapping")
		return nil
	}
}

func (s *compileState) arity(value *kql.CallExpression, function string, expected int) sqlast.Expr {
	s.error("KQLB0501", value.Span, "function."+function, fmt.Sprintf("%s requires %d arguments", function, expected))
	return nil
}

func (s *compileState) nextAlias() string {
	alias := fmt.Sprintf("_k%d", s.alias)
	s.alias++
	return alias
}

func (s *compileState) use(id, support string, span kql.Span) {
	if entry, ok := featureledger.Lookup(id); ok {
		support = string(entry.Support)
	} else if strings.HasPrefix(id, "function.") {
		if entry, found := featureledger.Lookup("aggregate." + strings.TrimPrefix(id, "function.")); found {
			id = entry.ID
			support = string(entry.Support)
		}
	}
	s.features = append(s.features, FeatureUse{ID: id, Support: support, Span: span})
}

func (s *compileState) error(code string, span kql.Span, feature, message string) {
	s.diagnostics = append(s.diagnostics, Diagnostic{Code: code, Severity: SeverityError, Phase: PhaseLower, Message: message, Span: span, Feature: feature})
}

func (s *compileState) warn(code string, span kql.Span, feature, message string) {
	s.diagnostics = append(s.diagnostics, Diagnostic{Code: code, Severity: SeverityWarning, Phase: PhaseLower, Message: message, Span: span, Feature: feature})
}

func splitName(name string) []string {
	parts := strings.Split(name, ".")
	for i := range parts {
		parts[i] = unquoteKQL(parts[i])
	}
	return parts
}

func lastName(name string) string {
	parts := splitName(name)
	return parts[len(parts)-1]
}

func unquoteKQL(value string) string {
	hidden := false
	if len(value) > 1 && (value[0] == 'h' || value[0] == 'H') && (value[1] == '\'' || value[1] == '"' || value[1] == '@') {
		hidden = true
		value = value[1:]
	}
	_ = hidden
	verbatim := strings.HasPrefix(value, "@")
	if verbatim {
		value = value[1:]
	}
	if len(value) >= 6 && ((strings.HasPrefix(value, "```") && strings.HasSuffix(value, "```")) || (strings.HasPrefix(value, "~~~") && strings.HasSuffix(value, "~~~"))) {
		return value[3 : len(value)-3]
	}
	if len(value) < 2 {
		return value
	}
	quote := value[0]
	if (quote != '\'' && quote != '"') || value[len(value)-1] != quote {
		return value
	}
	value = value[1 : len(value)-1]
	if verbatim {
		return strings.ReplaceAll(value, string([]byte{quote, quote}), string(quote))
	}
	if unquoted, err := strconv.Unquote(string(quote) + value + string(quote)); err == nil {
		return unquoted
	}
	return strings.ReplaceAll(value, "\\"+string(quote), string(quote))
}

func isTimespan(value string) bool {
	if value == "" {
		return false
	}
	last := value[len(value)-1]
	return last >= 'a' && last <= 'z' || last >= 'A' && last <= 'Z'
}

func expressionText(expression kql.Expression) string {
	switch value := expression.(type) {
	case *kql.LiteralExpression:
		return unquoteKQL(value.Text)
	case *kql.NameExpression:
		return value.Name
	default:
		return ""
	}
}

func scalarOperatorID(operator string) string {
	names := map[string]string{
		"+": "add", "-": "subtract", "*": "multiply", "/": "divide", "%": "modulo",
		"<": "less_than", "<=": "less_than_or_equal", ">": "greater_than", ">=": "greater_than_or_equal",
		"==": "equal", "!=": "not_equal", "<>": "not_equal", "=~": "equal_tilde", "!~": "bang_tilde",
		"!has": "not_has", "!has_cs": "not_has_cs", "!hasprefix": "not_hasprefix",
		"!hasprefix_cs": "not_hasprefix_cs", "!hassuffix": "not_hassuffix",
		"!hassuffix_cs": "not_hassuffix_cs", "!contains": "not_contains",
		"!contains_cs": "not_contains_cs", "!startswith": "not_startswith",
		"!startswith_cs": "not_startswith_cs", "!endswith": "not_endswith",
		"!endswith_cs": "not_endswith_cs", "matches regex": "matches_regex",
		"!in": "not_in", "in~": "in_cs", "!in~": "not_in_cs", "!between": "not_between",
	}
	if name, ok := names[operator]; ok {
		return "operator.scalar." + name
	}
	return "operator.scalar." + strings.ReplaceAll(operator, " ", "_")
}
