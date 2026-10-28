package ksql

import (
	"encoding/json"
	"fmt"
	"regexp/syntax"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/runtime"
	"github.com/kawijayaa/ksql/sqlast"
)

func supportsKeyValues(target any) bool {
	capability, ok := target.(interface {
		KeyValues(value, options string) (string, bool)
	})
	if !ok {
		return false
	}
	_, ok = capability.KeyValues("", "")
	return ok
}

// parseKVOptions validates a with clause and selects the delimited, free or
// regex extraction mode documented for parse-kv.
func (s *compileState) parseKVOptions(spec kql.ParseKVSpec, feature string) (runtime.KeyValueOptions, bool) {
	options := runtime.KeyValueOptions{Mode: "free"}
	fail := func(span kql.Span, message string) (runtime.KeyValueOptions, bool) {
		s.bindError("KQLB0380", span, feature, message)
		return options, false
	}
	seen := map[string]bool{}
	for _, option := range spec.Options {
		if seen[option.Name] && option.Name != "quote" && option.Name != "escape" {
			return fail(option.Span, "duplicate parse-kv option "+option.Name)
		}
		seen[option.Name] = true
		if option.Name == "greedy" {
			if option.Value.Kind != kql.IdentifierToken || (!strings.EqualFold(option.Value.Text, "true") && !strings.EqualFold(option.Value.Text, "false")) {
				return fail(option.Span, "parse-kv greedy requires true or false")
			}
			options.Greedy = strings.EqualFold(option.Value.Text, "true")
			continue
		}
		if option.Value.Kind != kql.StringToken {
			return fail(option.Span, "parse-kv option "+option.Name+" requires a string literal")
		}
		text, err := kql.DecodeString(option.Value.Text)
		if err != nil {
			return fail(option.Span, "invalid parse-kv string literal")
		}
		switch option.Name {
		case "pair_delimiter":
			options.Pair = text
		case "kv_delimiter":
			options.KV = text
		case "quote":
			if count := utf8.RuneCountInString(text); count < 1 || count > 2 {
				return fail(option.Span, "parse-kv quote requires one or two characters")
			}
			options.Quotes = append(options.Quotes, text)
		case "escape":
			if utf8.RuneCountInString(text) != 1 {
				return fail(option.Span, "parse-kv escape requires one character")
			}
			options.Escapes = append(options.Escapes, text)
		case "regex":
			parsed, err := syntax.Parse(text, syntax.Perl)
			if err != nil || parsed.MaxCap() != 2 {
				return fail(option.Span, "parse-kv regex requires exactly two capturing groups")
			}
			if limit := s.compiler.limits.MaxRegexBytes; limit > 0 && len(text) > limit {
				return fail(option.Span, fmt.Sprintf("parse-kv regex exceeds MaxRegexBytes (%d)", limit))
			}
			options.Regex = text
		default:
			return fail(option.Span, "unknown parse-kv option "+option.Name)
		}
	}
	span := spec.Source.NodeSpan()
	switch {
	case seen["regex"]:
		if len(seen) != 1 {
			return fail(span, "parse-kv regex cannot be combined with other options")
		}
		options.Mode = "regex"
	case seen["pair_delimiter"] || seen["kv_delimiter"]:
		if options.Pair == "" || options.KV == "" {
			return fail(span, "parse-kv requires both non-empty pair_delimiter and kv_delimiter")
		}
		options.Mode = "delimited"
	case seen["greedy"]:
		return fail(span, "parse-kv greedy requires pair_delimiter and kv_delimiter")
	}
	if len(options.Escapes) > 0 && len(options.Quotes) == 0 {
		return fail(span, "parse-kv escape requires quote")
	}
	return options, true
}

func (s *compileState) parseKVOperator(input Relation, operator kql.Operator) Relation {
	feature := "operator.parse-kv"
	spec := operator.Body.(kql.ParseKVSpec)
	if spec.Source == nil {
		return Relation{}
	}
	if input.Schema.Unknown {
		s.bindError("KQLB0377", operator.Span, feature, "parse-kv requires a known input schema")
		return Relation{}
	}
	options, ok := s.parseKVOptions(spec, feature)
	if !ok {
		return Relation{}
	}
	type key struct {
		name       string
		conversion string
		span       kql.Span
	}
	keys := make([]key, 0, len(spec.Keys))
	seen := map[string]bool{}
	for _, item := range spec.Keys {
		name := unquoteKQL(item.Name)
		typeName := strings.ToLower(item.Type)
		conversion, typed := parseConversions[typeName]
		if item.Type != "" && !typed && typeName != "string" {
			s.bindError("KQLB0373", item.Span, feature, "unsupported parse-kv key type "+item.Type)
			return Relation{}
		}
		if seen[strings.ToLower(name)] {
			s.bindError("KQLB0374", item.Span, feature, "duplicate parse-kv key "+name)
			return Relation{}
		}
		seen[strings.ToLower(name)] = true
		keys = append(keys, key{name: name, conversion: conversion, span: item.Span})
		options.Keys = append(options.Keys, name)
	}
	if len(keys) > maxParseColumns {
		s.bindError("KQLB0375", operator.Span, feature, fmt.Sprintf("parse-kv exceeds %d keys", maxParseColumns))
		return Relation{}
	}
	if !supportsKeyValues(s.compiler.dialect) {
		s.error("KQLL0371", operator.Span, feature, "parse-kv requires a key-value extraction capable SQL dialect")
		return Relation{}
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		s.bindError("KQLB0380", operator.Span, feature, "parse-kv options cannot be encoded")
		return Relation{}
	}
	previousInput := s.input
	defer func() { s.input = previousInput }()
	reserved := map[string]bool{}
	for _, column := range input.Schema.Columns {
		reserved[strings.ToLower(column.Name)] = true
	}
	for _, key := range keys {
		reserved[strings.ToLower(key.name)] = true
	}
	bagName := ""
	for i := 0; bagName == ""; i++ {
		candidate := "__ksql_parse_kv" + strconv.Itoa(i)
		if _, let := s.scalarLets[candidate]; !reserved[candidate] && !let {
			bagName = candidate
		}
	}
	name := func(text string) kql.Expression { return &kql.NameExpression{Name: text, Span: operator.Span} }
	call := func(function string, args ...kql.Expression) kql.Expression {
		return &kql.CallExpression{Function: name(function), Args: args, Span: operator.Span}
	}

	// Stage 1: one extraction per row into a dynamic bag of requested keys.
	s.input = input.Schema
	source := s.expression(spec.Source)
	if source.SQL == nil {
		return Relation{}
	}
	if source.Type != TypeString {
		source = s.expression(call("tostring", spec.Source))
	}
	items := make([]sqlast.SelectItem, 0, len(input.Schema.Columns)+1)
	for _, column := range input.Schema.Columns {
		items = append(items, sqlast.SelectItem{Expr: s.columnIdentifier(column.Name), Alias: column.Name})
	}
	items = append(items, sqlast.SelectItem{Expr: &sqlast.KeyValues{Value: source.SQL, Options: s.bind(string(encoded))}, Alias: bagName})
	stage := &sqlast.Select{Fence: true, From: &sqlast.Subquery{Query: input.Query, Alias: s.nextAlias()}, Projections: items}
	bagSchema := input.Schema.Clone()
	bagSchema.Columns = append(bagSchema.Columns, Column{Name: bagName, Type: TypeDynamic})

	// Stage 2: typed values replace same-named inputs in place, as extend does.
	s.input = bagSchema
	output := input.Schema.Clone()
	values := map[string]boundExpr{}
	for _, key := range keys {
		literal := &kql.LiteralExpression{Text: `@"` + strings.ReplaceAll(key.name, `"`, `""`) + `"`, Kind: kql.StringToken, Span: key.span}
		value := call("tostring", &kql.IndexExpression{Value: name(bagName), Index: literal, Span: key.span})
		if key.conversion != "" {
			value = call(key.conversion, value)
		}
		bound := s.expression(value)
		if bound.SQL == nil {
			return Relation{}
		}
		values[strings.ToLower(key.name)] = bound
		if _, index, found, _ := output.Lookup(key.name); found {
			output.Columns[index] = Column{Name: output.Columns[index].Name, Type: bound.Type}
		} else {
			output.Columns = append(output.Columns, Column{Name: key.name, Type: bound.Type})
		}
	}
	if !s.checkProjectionWidth(len(output.Columns), operator.Span, feature) {
		return Relation{}
	}
	final := &sqlast.Select{From: &sqlast.Subquery{Query: stage, Alias: s.nextAlias()}}
	for _, column := range output.Columns {
		expr := s.columnIdentifier(column.Name)
		if value, ok := values[strings.ToLower(column.Name)]; ok {
			expr = value.SQL
		}
		final.Projections = append(final.Projections, sqlast.SelectItem{Expr: expr, Alias: column.Name})
	}
	return Relation{Query: final, Schema: output}
}
