package ksql

import (
	"fmt"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// Kusto limits the legacy strategy to 64 partitions; native and shuffle are
// unlimited, but each partition here is a separate SQL branch, so they are
// bounded too.
const (
	maxLegacyPartitions = 64
	maxPartitions       = 1024
)

// ValuesResolver runs a discovery query and returns the values of its first
// column. The query is a SQL AST so the application renders it exactly as it
// renders compiled queries.
type ValuesResolver func(query sqlast.Query, args []any) ([]any, error)

// WithValuesResolver enables operators that discover data values at compile
// time, such as partition.
func WithValuesResolver(resolver ValuesResolver) Option {
	return func(c *Compiler) { c.values = resolver }
}

// partition applies its subquery to the rows of each distinct key value and
// concatenates the results. The keys are discovered with the application's
// ValuesResolver.
func (s *compileState) partition(input Relation, operator kql.Operator) Relation {
	spec := operator.Body.(kql.PartitionSpec)
	feature := "operator.partition"
	if input.Schema.Unknown {
		s.bindError("KQLB0358", operator.Span, feature, "partition requires a bound input schema")
		return Relation{}
	}
	key, _, found, ambiguous := input.Schema.Lookup(unquoteKQL(spec.Key))
	if !found || ambiguous {
		s.bindError("KQLB0401", operator.Span, feature, "unknown partition column "+spec.Key)
		return Relation{}
	}
	if key.Type == TypeDynamic {
		s.bindError("KQLB0358", operator.Span, feature, "partition keys cannot be dynamic")
		return Relation{}
	}
	limit := maxPartitions
	if spec.Strategy == "legacy" {
		limit = maxLegacyPartitions
	}
	values, ok := s.discoverValues(input, key.Name, limit, operator.Span, feature)
	if !ok {
		return Relation{}
	}
	column := &sqlast.Identifier{Parts: []string{key.Name}}
	var legs []Relation
	branch := func(condition sqlast.Expr, value sqlast.Expr) bool {
		rows := Relation{Query: &sqlast.Select{From: &sqlast.Subquery{Query: input.Query, Alias: s.nextAlias()}, Where: condition}, Schema: input.Schema.Clone(), rowDependencies: input.rowDependencies}
		var result Relation
		if spec.WithSource {
			name := strings.ToLower(key.Name)
			previous, had := s.scalarLets[name]
			s.scalarLets[name] = boundExpr{SQL: value, Type: key.Type}
			result = s.pipeline(spec.Body)
			if had {
				s.scalarLets[name] = previous
			} else {
				delete(s.scalarLets, name)
			}
		} else {
			result = rows
			for _, op := range spec.Body.Operators {
				if result.Query == nil {
					break
				}
				result = s.serializedAfter(result, s.operator(result, op), op)
			}
		}
		if result.Query == nil {
			return false
		}
		legs = append(legs, result)
		return true
	}
	for _, value := range values {
		var condition, keyValue sqlast.Expr
		if value == nil {
			condition, keyValue = &sqlast.Binary{Left: column, Operator: "IS", Right: &sqlast.Literal{Kind: sqlast.NullLiteral}}, &sqlast.Literal{Kind: sqlast.NullLiteral}
		} else {
			keyValue = s.bind(value)
			condition = &sqlast.Binary{Left: column, Operator: "=", Right: keyValue}
			if spec.WithSource {
				keyValue = s.bind(value)
			}
		}
		if !branch(condition, keyValue) {
			return Relation{}
		}
	}
	if len(legs) == 0 {
		// No partitions: keep the subquery's schema over no rows.
		empty := &sqlast.Binary{Left: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "1"}, Operator: "=", Right: &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: "0"}}
		if !branch(empty, &sqlast.Literal{Kind: sqlast.NullLiteral}) {
			return Relation{}
		}
		legs[0].Query = &sqlast.Select{From: &sqlast.Subquery{Query: legs[0].Query, Alias: s.nextAlias()}, Where: empty}
	}
	schema := legs[0].Schema
	queries := make([]sqlast.Query, len(legs))
	for i, leg := range legs {
		if leg.Schema.Unknown || !sameColumns(leg.Schema, schema) {
			s.bindError("KQLB0358", operator.Span, feature, "partition subquery results must have the same columns for every partition")
			return Relation{}
		}
		queries[i] = &sqlast.Select{From: &sqlast.Subquery{Query: leg.Query, Alias: s.nextAlias()}}
	}
	if len(queries) == 1 {
		return Relation{Query: queries[0], Schema: schema.Clone()}
	}
	return Relation{Query: &sqlast.Union{All: true, Queries: queries}, Schema: schema.Clone()}
}

func sameColumns(a, b Schema) bool {
	if len(a.Columns) != len(b.Columns) {
		return false
	}
	for i := range a.Columns {
		if a.Columns[i].Name != b.Columns[i].Name || a.Columns[i].Type != b.Columns[i].Type {
			return false
		}
	}
	return true
}

// discoverValues returns the distinct values of a column of the input, in
// order, failing when there are more than limit.
func (s *compileState) discoverValues(input Relation, column string, limit int, span kql.Span, feature string) ([]any, bool) {
	if s.compiler.values == nil {
		s.error("KQLL0394", span, feature, "partition requires an application values resolver")
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
		s.error("KQLL0394", span, feature, "partition over cached or correlated input is not supported")
		return nil, false
	}
	value := &sqlast.Identifier{Parts: []string{"ksql_partition", column}}
	discovery := &sqlast.Select{
		Distinct:    true,
		Projections: []sqlast.SelectItem{{Expr: value, Alias: "value"}},
		From:        &sqlast.Subquery{Query: input.Query, Alias: "ksql_partition"},
		OrderBy:     []sqlast.Order{{Expr: &sqlast.Identifier{Parts: []string{"value"}}}},
		Limit:       &sqlast.Literal{Kind: sqlast.NumberLiteral, Value: fmt.Sprint(limit + 1)},
	}
	args := sqlast.NormalizeParameters(discovery, append([]any(nil), s.args...))
	values, err := s.compiler.values(discovery, args)
	if err != nil {
		s.error("KQLL0395", span, feature, "partition key discovery failed: "+err.Error())
		return nil, false
	}
	if len(values) > limit {
		s.bindError("KQLB0359", span, feature, fmt.Sprintf("partition column has more than %d distinct values", limit))
		return nil, false
	}
	for i, value := range values {
		if bytes, ok := value.([]byte); ok {
			if bytes == nil {
				values[i] = nil
			} else {
				values[i] = string(bytes)
			}
		}
	}
	return values, true
}
