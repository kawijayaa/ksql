package ksql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

const maxBagUnpackKeys = 1024

// BagKey is one top-level property found by bag_unpack discovery, with the
// SQLite json_each type of one of its values (integer, real, text, true,
// false, object or array).
type BagKey struct {
	Name     string
	JSONType string
}

// BagKeysResolver runs a bag_unpack discovery query and returns its rows. The
// query selects distinct (key, type) pairs of the unpacked column; it is a SQL
// AST so the application renders it exactly as it renders compiled queries.
// Without a resolver, bag_unpack requires an output schema.
type BagKeysResolver func(query sqlast.Query, args []any) ([]BagKey, error)

// WithBagKeysResolver enables bag_unpack without an output schema.
func WithBagKeysResolver(resolver BagKeysResolver) Option {
	return func(c *Compiler) { c.bagKeys = resolver }
}

func (s *compileState) evaluate(input Relation, operator kql.Operator) Relation {
	spec := operator.Body.(kql.EvaluateSpec)
	feature := "plugin." + spec.Plugin
	s.use(feature, "equivalent", operator.Span)
	switch spec.Plugin {
	case "bag_unpack":
		return s.bagUnpack(input, spec, feature)
	case "narrow":
		return s.narrow(input, spec, feature)
	case "rows_near":
		return s.rowsNear(input, spec, feature)
	case "pivot":
		return s.pivot(input, spec, operator, feature)
	case "ipv4_lookup":
		return s.ipLookup(input, spec, feature, "ipv4_is_in_range")
	case "ipv6_lookup":
		return s.ipLookup(input, spec, feature, "ipv6_is_in_range")
	case "sequence_detect":
		return s.sequenceDetect(input, spec, feature)

	}
	s.error("KQLL0391", operator.Span, feature, "evaluate plugin "+spec.Plugin+" is not supported by SQL lowering")
	return Relation{}
}

type bagSlot struct {
	key, name string
	kind      ScalarType
}

// bagUnpack follows Microsoft's bag_unpack plugin: one column per top-level
// slot (sorted by name), mixed slot types become dynamic, nulls are ignored,
// and columnsConflict chooses error, replace_source or keep_source.
func (s *compileState) bagUnpack(input Relation, spec kql.EvaluateSpec, feature string) Relation {
	fail := func(message string) Relation {
		s.bindError("KQLB0416", spec.Span, feature, message)
		return Relation{}
	}
	if input.Schema.Unknown {
		return fail("bag_unpack requires a known input schema")
	}
	if len(spec.Args) == 0 || len(spec.Args) > 4 {
		return fail("bag_unpack requires a column and up to three options")
	}
	column, ok := spec.Args[0].(*kql.NameExpression)
	if !ok {
		return fail("bag_unpack requires a column reference")
	}
	source, _, found, _ := input.Schema.Lookup(unquoteKQL(column.Name))
	if !found {
		s.bindError("KQLB0401", column.Span, feature, "unknown column "+unquoteKQL(column.Name))
		return Relation{}
	}
	if source.Type != TypeDynamic && source.Type != TypeUnknown {
		return fail("bag_unpack requires a dynamic column")
	}
	prefix, conflict := "", "error"
	ignored := map[string]bool{}
	positional := []string{"outputcolumnprefix", "columnsconflict", "ignoredproperties"}
	for i, argument := range spec.Args[1:] {
		name := positional[i]
		if named, ok := argument.(*kql.NamedExpression); ok {
			name, argument = strings.ToLower(unquoteKQL(named.Name)), named.Value
		}
		switch name {
		case "outputcolumnprefix", "columnsconflict":
			literal, ok := argument.(*kql.LiteralExpression)
			if !ok || literal.Kind != kql.StringToken {
				return fail("bag_unpack " + name + " must be a string literal")
			}
			if name == "outputcolumnprefix" {
				prefix = stringLiteralValue(literal)
			} else {
				conflict = strings.ToLower(stringLiteralValue(literal))
				if conflict != "error" && conflict != "replace_source" && conflict != "keep_source" {
					return fail("bag_unpack columnsConflict must be error, replace_source or keep_source")
				}
			}
		case "ignoredproperties":
			names, ok := literalStringArray(argument)
			if !ok {
				return fail("bag_unpack ignoredProperties must be a dynamic array of strings")
			}
			for _, key := range names {
				ignored[key] = true
			}
		default:
			return fail("unknown bag_unpack option " + name)
		}
	}

	var slots []bagSlot
	if spec.HasSchema {
		for _, definition := range spec.Schema {
			name := unquoteKQL(definition.Name)
			kind := parseScalarType(definition.Type)
			if kind == TypeUnknown {
				return fail("unsupported bag_unpack schema type " + definition.Type)
			}
			if _, _, exists, _ := input.Schema.Lookup(name); exists && !strings.EqualFold(name, source.Name) {
				if spec.Wildcard {
					continue // * already carries this input column.
				}
			}
			slots = append(slots, bagSlot{key: strings.TrimPrefix(name, prefix), name: name, kind: kind})
		}
	} else {
		discovered, ok := s.discoverBagKeys(input, source.Name, spec.Span, feature)
		if !ok {
			return Relation{}
		}
		for _, slot := range discovered {
			if !ignored[slot.key] {
				slot.name = prefix + slot.key
				slots = append(slots, slot)
			}
		}
	}

	previous := s.input
	s.input = input.Schema
	defer func() { s.input = previous }()
	values := map[string]boundExpr{}
	for _, slot := range slots {
		bound := s.expression(bagSlotExpression(column, slot))
		if bound.SQL == nil {
			return Relation{}
		}
		values[strings.ToLower(slot.name)] = boundExpr{SQL: bound.SQL, Type: bound.Type}
	}

	output := Schema{}
	items := []sqlast.SelectItem{}
	include := !spec.HasSchema || spec.Wildcard
	for _, inputColumn := range input.Schema.Columns {
		if inputColumn.Name == source.Name || !include {
			continue
		}
		expr := s.columnIdentifier(inputColumn.Name)
		kind := inputColumn.Type
		if value, collides := values[strings.ToLower(inputColumn.Name)]; collides && !spec.HasSchema {
			switch conflict {
			case "error":
				return fail("bag_unpack slot " + inputColumn.Name + " conflicts with an input column; use columnsConflict")
			case "replace_source":
				expr, kind = value.SQL, value.Type
			}
			delete(values, strings.ToLower(inputColumn.Name))
		}
		items = append(items, sqlast.SelectItem{Expr: expr, Alias: inputColumn.Name})
		output.Columns = append(output.Columns, Column{Name: inputColumn.Name, Type: kind})
	}
	for _, slot := range slots {
		value, pending := values[strings.ToLower(slot.name)]
		if !pending {
			continue
		}
		items = append(items, sqlast.SelectItem{Expr: value.SQL, Alias: slot.name})
		output.Columns = append(output.Columns, Column{Name: slot.name, Type: value.Type})
	}
	if len(items) == 0 {
		return fail("bag_unpack produced no columns")
	}
	if !s.checkProjectionWidth(len(items), spec.Span, feature) {
		return Relation{}
	}
	s.validateUnique(output.Columns, spec.Span, feature)
	return Relation{Query: &sqlast.Select{From: &sqlast.Subquery{Query: input.Query, Alias: s.nextAlias()}, Projections: items}, Schema: output}
}

// bagSlotExpression reads one slot with the conversion matching its type, so
// application conversion overrides apply.
func bagSlotExpression(column *kql.NameExpression, slot bagSlot) kql.Expression {
	span := column.Span
	key := &kql.LiteralExpression{Text: `@"` + strings.ReplaceAll(slot.key, `"`, `""`) + `"`, Kind: kql.StringToken, Span: span}
	value := kql.Expression(&kql.IndexExpression{Value: column, Index: key, Span: span})
	call := func(function string, argument kql.Expression) kql.Expression {
		return &kql.CallExpression{Function: &kql.NameExpression{Name: function, Span: span}, Args: []kql.Expression{argument}, Span: span}
	}
	switch slot.kind {
	case TypeLong:
		return call("tolong", value)
	case TypeInt:
		return call("toint", value)
	case TypeReal:
		return call("toreal", value)
	case TypeBool:
		return call("tobool", value)
	case TypeString:
		return call("tostring", value)
	case TypeDateTime:
		return call("todatetime", call("tostring", value))
	case TypeTimespan:
		return call("totimespan", call("tostring", value))
	}
	return value
}

func literalStringArray(expression kql.Expression) ([]string, bool) {
	dynamic, ok := expression.(*kql.DynamicExpression)
	if !ok {
		return nil, false
	}
	var values []string
	for _, token := range dynamic.Tokens {
		switch {
		case token.Kind == kql.StringToken:
			values = append(values, unquoteKQL(token.Text))
		case token.Text == "[" || token.Text == "]" || token.Text == ",":
		default:
			return nil, false
		}
	}
	return values, true
}

// discoverBagKeys asks the resolver for distinct object keys and types.
func (s *compileState) discoverBagKeys(input Relation, column string, span kql.Span, feature string) ([]bagSlot, bool) {
	if s.compiler.bagKeys == nil || s.compiler.dialect.Name() != "sqlite" {
		s.error("KQLL0392", span, feature, "bag_unpack without an output schema requires a key resolver; add : (Name:type, ...)")
		return nil, false
	}
	materialized := false
	sqlast.Walk(input.Query, func(node any) bool {
		if _, ok := node.(*sqlast.MaterializedReference); ok {
			materialized = true
		}
		return !materialized
	})
	if materialized || len(input.rowDependencies) != 0 || len(s.capturedRows) != 0 {
		s.error("KQLL0392", span, feature, "bag_unpack over cached or correlated input requires an output schema")
		return nil, false
	}
	bag := &sqlast.Identifier{Parts: []string{"ksql_bag", column}}
	object := &sqlast.Case{Branches: []sqlast.When{{
		Condition: &sqlast.Call{Name: "json_valid", Args: []sqlast.Expr{bag}},
		Result: &sqlast.Case{Branches: []sqlast.When{{
			Condition: &sqlast.Binary{Left: &sqlast.Call{Name: "json_type", Args: []sqlast.Expr{bag}}, Operator: "=", Right: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: "object"}},
			Result:    bag,
		}}},
	}}}
	discovery := &sqlast.Select{
		Distinct: true,
		Projections: []sqlast.SelectItem{
			{Expr: &sqlast.Identifier{Parts: []string{"ksql_keys", "key"}}, Alias: "key"},
			{Expr: &sqlast.Identifier{Parts: []string{"ksql_keys", "type"}}, Alias: "type"},
		},
		From:    &sqlast.Join{Left: &sqlast.Subquery{Query: input.Query, Alias: "ksql_bag"}, Right: &sqlast.JSONEach{Value: object, Alias: "ksql_keys"}, Kind: "cross"},
		Where:   &sqlast.Binary{Left: &sqlast.Identifier{Parts: []string{"ksql_keys", "type"}}, Operator: "<>", Right: &sqlast.Literal{Kind: sqlast.StringLiteral, Value: "null"}},
		OrderBy: []sqlast.Order{{Expr: &sqlast.Identifier{Parts: []string{"key"}}}},
		Limit:   &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: fmt.Sprint(maxBagUnpackKeys*8 + 1)},
	}
	args := sqlast.NormalizeParameters(discovery, append([]any(nil), s.args...))
	rows, err := s.compiler.bagKeys(discovery, args)
	if err != nil {
		s.error("KQLL0393", span, feature, "bag_unpack key discovery failed: "+err.Error())
		return nil, false
	}
	types := map[string]map[string]bool{}
	for _, row := range rows {
		if types[row.Name] == nil {
			types[row.Name] = map[string]bool{}
		}
		kind := row.JSONType
		if kind == "true" || kind == "false" {
			kind = "bool"
		}
		types[row.Name][kind] = true
	}
	if len(types) > maxBagUnpackKeys {
		s.bindError("KQLB0416", span, feature, fmt.Sprintf("bag_unpack found more than %d keys; add an output schema", maxBagUnpackKeys))
		return nil, false
	}
	slots := make([]bagSlot, 0, len(types))
	for key, kinds := range types {
		kind := TypeDynamic
		if len(kinds) == 1 {
			for jsonType := range kinds {
				kind = map[string]ScalarType{"integer": TypeLong, "real": TypeReal, "text": TypeString, "bool": TypeBool}[jsonType]
				if kind == "" {
					kind = TypeDynamic
				}
			}
		}
		slots = append(slots, bagSlot{key: key, kind: kind})
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].key < slots[j].key })
	return slots, true
}
