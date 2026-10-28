package ksql

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// maxSearchTables bounds the union a search without a tabular input builds.
const maxSearchTables = 256

// TableLister is an optional Catalog extension listing the tables a search
// without a tabular input (or with table patterns) spans.
type TableLister interface {
	TableNames() []string
}

// search filters its input with a search predicate rewritten to an ordinary
// where predicate (see searchPredicate).
func (s *compileState) search(from *sqlast.Subquery, input Schema, operator kql.Operator) Relation {
	if input.Unknown {
		s.bindError("KQLB0350", operator.Span, "operator.search", "search requires a bound input schema")
		return Relation{}
	}
	spec, ok := operator.Body.(kql.SearchSpec)
	if !ok {
		s.error("KQLL0350", operator.Span, "operator.search", "unsupported search form")
		return Relation{}
	}
	predicate, ok := s.searchCondition(spec, input, operator.Span)
	if !ok {
		return Relation{}
	}
	return Relation{Query: &sqlast.Select{From: from, Where: predicate}, Schema: input.Clone()}
}

func (s *compileState) searchCondition(spec kql.SearchSpec, input Schema, span kql.Span) (sqlast.Expr, bool) {
	if maximum := s.compiler.limits.MaxProjectionItems; maximum > 0 && len(input.Columns) > maximum {
		s.bindError("KQLB0355", span, "operator.search", fmt.Sprintf("search input exceeds MaxProjectionItems (%d)", maximum))
		return nil, false
	}
	rewritten, ok := s.searchPredicate(spec.Term, input, spec.Kind == "case_sensitive")
	if !ok {
		return nil, false
	}
	previous := s.input
	s.input = input
	defer func() { s.input = previous }()
	bound := s.expression(rewritten)
	if bound.SQL == nil {
		return nil, false
	}
	if bound.Type != TypeBool && bound.Type != TypeUnknown {
		s.bindError("KQLB0351", spec.Term.NodeSpan(), "operator.search", "search predicate must be Boolean")
		return nil, false
	}
	return bound.SQL, true
}

// searchPredicate rewrites Kusto's search syntax into a where predicate:
// a bare string term matches any column (has, or hasprefix/hassuffix/
// contains/regex by the placement of *), Column:"term" matches one column,
// * alone is true and "* op value" applies a string operator to every
// column. kind=case_sensitive switches string operators to their _cs forms.
func (s *compileState) searchPredicate(expression kql.Expression, input Schema, caseSensitive bool) (kql.Expression, bool) {
	switch value := expression.(type) {
	case *kql.StarExpression:
		return &kql.LiteralExpression{Text: "true", Kind: kql.IdentifierToken, Span: value.Span}, true
	case *kql.LiteralExpression:
		if value.Kind == kql.StringToken {
			return s.searchTerm(nil, stringLiteralValue(value), input, caseSensitive, value.Span)
		}
	case *kql.BinaryExpression:
		operator := strings.ToLower(value.Operator)
		switch operator {
		case "and", "or":
			left, ok := s.searchPredicate(value.Left, input, caseSensitive)
			if !ok {
				return nil, false
			}
			right, ok := s.searchPredicate(value.Right, input, caseSensitive)
			if !ok {
				return nil, false
			}
			return &kql.BinaryExpression{Left: left, Operator: value.Operator, Right: right, Span: value.Span}, true
		case ":":
			column, okColumn := value.Left.(*kql.NameExpression)
			literal, okLiteral := value.Right.(*kql.LiteralExpression)
			if !okColumn || !okLiteral || literal.Kind != kql.StringToken {
				s.bindError("KQLB0351", value.Span, "operator.search", "Column:term requires a column name and a string literal")
				return nil, false
			}
			return s.searchTerm(column, stringLiteralValue(literal), input, caseSensitive, value.Span)
		}
		if _, ok := value.Left.(*kql.StarExpression); ok {
			var terms kql.Expression
			for _, column := range input.Columns {
				match := &kql.BinaryExpression{Left: searchColumnText(column), Operator: searchOperator(value.Operator, caseSensitive), Right: value.Right, Span: value.Span}
				terms = searchOr(terms, match)
			}
			if terms == nil {
				s.bindError("KQLB0354", value.Span, "operator.search", "search input has no visible columns")
				return nil, false
			}
			return terms, true
		}
		return &kql.BinaryExpression{Left: value.Left, Operator: searchOperator(value.Operator, caseSensitive), Right: value.Right, Span: value.Span}, true
	case *kql.CallExpression:
		if name, ok := value.Function.(*kql.NameExpression); ok && strings.EqualFold(name.Name, "not") && len(value.Args) == 1 {
			inner, ok := s.searchPredicate(value.Args[0], input, caseSensitive)
			if !ok {
				return nil, false
			}
			return &kql.CallExpression{Function: value.Function, Args: []kql.Expression{inner}, Span: value.Span}, true
		}
	}
	return expression, true
}

var searchCaseSensitive = map[string]string{
	"has": "has_cs", "!has": "!has_cs", "hasprefix": "hasprefix_cs", "!hasprefix": "!hasprefix_cs",
	"hassuffix": "hassuffix_cs", "!hassuffix": "!hassuffix_cs", "contains": "contains_cs", "!contains": "!contains_cs",
	"startswith": "startswith_cs", "!startswith": "!startswith_cs", "endswith": "endswith_cs", "!endswith": "!endswith_cs",
}

func searchOperator(operator string, caseSensitive bool) string {
	if replacement, ok := searchCaseSensitive[strings.ToLower(operator)]; ok && caseSensitive {
		return replacement
	}
	return operator
}

func searchOr(left, right kql.Expression) kql.Expression {
	if left == nil {
		return right
	}
	return &kql.BinaryExpression{Left: left, Operator: "or", Right: right}
}

// searchColumnText refers to a column as text; non-string columns are
// converted with tostring.
func searchColumnText(column Column) kql.Expression {
	name := kql.Expression(&kql.NameExpression{Name: searchName(column.Name)})
	if column.Type == TypeString {
		return name
	}
	return &kql.CallExpression{Function: &kql.NameExpression{Name: "tostring"}, Args: []kql.Expression{name}}
}

var plainIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func searchName(name string) string {
	if plainIdentifier.MatchString(name) {
		return name
	}
	return "['" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(name) + "']"
}

func searchString(value string) *kql.LiteralExpression {
	return &kql.LiteralExpression{Text: `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(value) + `"`, Kind: kql.StringToken}
}

func (s *compileState) searchTerm(column *kql.NameExpression, term string, input Schema, caseSensitive bool, span kql.Span) (kql.Expression, bool) {
	if term == "" {
		s.bindError("KQLB0352", span, "operator.search", "search term must be non-empty")
		return nil, false
	}
	if term == "*" {
		return &kql.LiteralExpression{Text: "true", Kind: kql.IdentifierToken, Span: span}, true
	}
	operator, value := "has", term
	inner := strings.Trim(term, "*")
	switch {
	case strings.Contains(inner, "*"):
		// An inner * matches any characters between whole terms.
		parts := strings.Split(term, "*")
		for i, part := range parts {
			parts[i] = regexp.QuoteMeta(part)
		}
		pattern := `\b` + strings.Join(parts, ".*") + `\b`
		if !caseSensitive {
			pattern = "(?i)" + pattern
		}
		if s.compiler.limits.MaxRegexBytes > 0 && len(pattern) > s.compiler.limits.MaxRegexBytes {
			s.bindError("KQLB0353", span, "operator.search", fmt.Sprintf("search pattern exceeds MaxRegexBytes (%d)", s.compiler.limits.MaxRegexBytes))
			return nil, false
		}
		operator, value = "matches regex", pattern
	case strings.HasPrefix(term, "*") && strings.HasSuffix(term, "*"):
		operator, value = "contains", inner
	case strings.HasPrefix(term, "*"):
		operator, value = "hassuffix", inner
	case strings.HasSuffix(term, "*"):
		operator, value = "hasprefix", inner
	}
	if value == "" {
		return &kql.LiteralExpression{Text: "true", Kind: kql.IdentifierToken, Span: span}, true
	}
	if operator != "matches regex" {
		operator = searchOperator(operator, caseSensitive)
	}
	if column != nil {
		return &kql.BinaryExpression{Left: column, Operator: operator, Right: searchString(value), Span: span}, true
	}
	var terms kql.Expression
	for _, candidate := range input.Columns {
		terms = searchOr(terms, &kql.BinaryExpression{Left: searchColumnText(candidate), Operator: operator, Right: searchString(value), Span: span})
	}
	if terms == nil {
		s.bindError("KQLB0354", span, "operator.search", "search input has no visible columns")
		return nil, false
	}
	return terms, true
}

// searchSource lowers a search that starts a query: a union of the listed
// tables (or of every table the catalog lists) with a $table column.
func (s *compileState) searchSource(source kql.Source) Relation {
	spec := *source.Search
	names, ok := s.resolveTablePatterns(spec.Tables, spec.HasTables, source.Span, "source.search")
	if !ok {
		return Relation{}
	}
	union := kql.UnionSpec{Kind: "outer", WithSource: "$table"}
	for _, name := range names {
		union.Inputs = append(union.Inputs, &kql.Pipeline{Source: kql.Source{Kind: "table", Name: searchName(name), Span: source.Span}, Span: source.Span})
	}
	relation := s.union(Relation{}, kql.Operator{Kind: "union", Body: union, Span: source.Span})
	if relation.Query == nil {
		return Relation{}
	}
	// $table identifies the source; search terms match the data columns.
	data := Schema{}
	for _, column := range relation.Schema.Columns {
		if column.Name != "$table" {
			data.Columns = append(data.Columns, column)
		}
	}
	predicate, ok := s.searchCondition(spec, data, source.Span)
	if !ok {
		return Relation{}
	}
	from := &sqlast.Subquery{Query: relation.Query, Alias: s.nextAlias()}
	return Relation{Query: &sqlast.Select{From: from, Where: predicate}, Schema: relation.Schema.Clone()}
}

// resolveTablePatterns expands table names and * patterns (or every table
// when none are given) through the catalog's TableLister.
func (s *compileState) resolveTablePatterns(patterns []string, explicit bool, span kql.Span, feature string) ([]string, bool) {
	lister, _ := s.compiler.catalog.(TableLister)
	var all []string
	if lister != nil {
		all = lister.TableNames()
	}
	// Views declared with let V = view () { ... } are searched like tables.
	views := 0
	for _, function := range s.functions {
		if function.definition != nil && function.definition.View && function.tabular && len(function.parameters) == 0 {
			all = append(all, function.name)
			views++
		}
	}
	sort.Strings(all)
	var names []string
	seen := map[string]bool{}
	add := func(name string) {
		if key := strings.ToLower(name); !seen[key] {
			seen[key] = true
			names = append(names, name)
		}
	}
	if !explicit {
		patterns = []string{"*"}
	}
	for _, pattern := range patterns {
		if !strings.Contains(pattern, "*") {
			add(unquoteKQL(pattern))
			continue
		}
		if lister == nil && views == 0 {
			s.bindError("KQLB0356", span, feature, "searching table patterns or every table requires a catalog that lists its tables")
			return nil, false
		}
		for _, name := range all {
			if matchColumnPattern(name, pattern) {
				add(name)
			}
		}
	}
	if len(names) == 0 {
		s.bindError("KQLB0356", span, feature, "no tables matched")
		return nil, false
	}
	if len(names) > maxSearchTables {
		s.bindError("KQLB0356", span, feature, fmt.Sprintf("more than %d tables matched", maxSearchTables))
		return nil, false
	}
	return names, true
}
