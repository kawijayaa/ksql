package ksql

import (
	"fmt"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// Option configures a Compiler during construction.
type Option func(*Compiler)

// FunctionRule lowers one KQL function call. Rules receive already-lowered
// arguments and must return a dialect-neutral SQL expression.
type FunctionRule func(arguments []sqlast.Expr) (sqlast.Expr, error)

// OperatorRule lowers a recognized KQL tabular operator. The input query is
// the relation produced by the preceding pipeline stage.
type OperatorRule func(input sqlast.Query, operator kql.Operator) (sqlast.Query, error)

// SourceRule lowers a recognized KQL tabular source.
type SourceRule func(source kql.Source) (sqlast.Query, error)

// WithFunction registers an explicit KQL function mapping. It overrides a
// built-in mapping with the same name. Invalid or empty names are ignored.
func WithFunction(name string, rule FunctionRule) Option {
	return func(compiler *Compiler) {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" && rule != nil {
			compiler.functionRules[name] = rule
		}
	}
}

// WithOperator registers a target-specific tabular operator adapter.
func WithOperator(name string, rule OperatorRule) Option {
	return func(compiler *Compiler) {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" && rule != nil {
			compiler.operatorRules[name] = rule
		}
	}
}

// WithSource registers a target-specific tabular source adapter.
func WithSource(kind string, rule SourceRule) Option {
	return func(compiler *Compiler) {
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind != "" && rule != nil {
			compiler.sourceRules[kind] = rule
		}
	}
}

// SQLFunction maps a KQL call to a SQL function with the supplied name.
// Callers should use this only after verifying semantic compatibility for the
// configured database or after installing a compatible UDF.
func SQLFunction(name string) FunctionRule {
	return func(arguments []sqlast.Expr) (sqlast.Expr, error) {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("SQL function name is empty")
		}
		return &sqlast.Call{Name: name, Args: arguments}, nil
	}
}
