package ksql

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

const maxQueryParameters = 1024
const maxQueryParameterBytes = 4 << 20

type queryParameterValues struct {
	values map[string]json.RawMessage
	err    string
}

// WithQueryParameters snapshots typed, JSON-encodable values for declarations in
// the query. JSON numbers preserve exact integers; strings are literal values,
// not encoded KQL text. RawMessage supports exact dynamic JSON and numeric input.
// time.Time is encoded as an ISO datetime string. Values never become query text.
func WithQueryParameters(values map[string]any) Option {
	snapshot := &queryParameterValues{values: make(map[string]json.RawMessage)}
	if len(values) > maxQueryParameters {
		snapshot.err = "query parameter count exceeds 1024"
	}
	bytesUsed := 0
	if snapshot.err == "" {
		for name, value := range values {
			if name == "" || !utf8.ValidString(name) || len(name) > 4096 {
				snapshot.err = "invalid query parameter name"
				break
			}
			if text, ok := value.(string); ok && !utf8.ValidString(text) {
				snapshot.err = "query parameter strings must be UTF-8"
				break
			}
			raw, err := json.Marshal(value)
			if err != nil {
				snapshot.err = "query parameter values must be JSON encodable"
				break
			}
			if !utf8.Valid(raw) {
				snapshot.err = "query parameter JSON must be UTF-8"
				break
			}
			bytesUsed += len(name) + len(raw)
			if bytesUsed > maxQueryParameterBytes {
				snapshot.err = "query parameter values exceed 4 MiB"
				break
			}
			snapshot.values[name] = raw
		}
	}
	if snapshot.err != "" {
		snapshot.values = nil
	}
	return func(c *Compiler) { c.parameters = snapshot }
}

func parameterType(name string) (ScalarType, bool) {
	switch strings.ToLower(name) {
	case "bool", "boolean", "int", "int32", "long", "int64", "real", "double", "string", "datetime", "date", "dynamic":
		return parseScalarType(name), true
	}
	return TypeUnknown, false
}

func (s *compileState) bindQueryParameters(node *kql.QueryParametersStatement) {
	s.use("statement.query_parameters", "equivalent", node.Span)
	if s.queryParameterCount+len(node.Parameters) > maxQueryParameters {
		s.bindError("KQLB0131", node.Span, "statement.query_parameters", "query parameter declarations exceed 1024")
		return
	}
	s.queryParameterCount += len(node.Parameters)
	for _, parameter := range node.Parameters {
		name := unquoteKQL(parameter.Name)
		key := strings.ToLower(name)
		fail := func(message string) { s.bindError("KQLB0131", parameter.Span, "statement.query_parameters", message) }
		if _, exists := s.scalarLets[key]; exists {
			fail("variable already declared: " + name)
			continue
		}
		if _, exists := s.tabularLets[key]; exists {
			fail("variable already declared: " + name)
			continue
		}
		if _, exists := s.functions[key]; exists {
			fail("variable already declared: " + name)
			continue
		}
		kind, valid := parameterType(parameter.Type)
		if !valid {
			fail("unsupported query parameter scalar type: " + parameter.Type)
			continue
		}
		var defaultRaw json.RawMessage
		if parameter.Default != nil {
			if kind == TypeDynamic {
				fail("dynamic query parameters cannot have defaults")
				continue
			}
			defaultRaw, valid = parameterDefault(parameter.Default, kind)
			if !valid {
				fail("query parameter default must be a literal of the declared type")
				continue
			}
			if _, valid = parameterValue(defaultRaw, kind); !valid {
				fail("query parameter default is invalid for its declared type")
				continue
			}
		}
		raw := defaultRaw
		if s.compiler.parameters != nil {
			if supplied, exists := s.compiler.parameters.values[name]; exists {
				raw = supplied
			}
		}
		if raw == nil {
			fail("query parameter value is required: " + name)
			continue
		}
		value, valid := parameterValue(raw, kind)
		if !valid {
			fail("query parameter value is invalid for its declared type: " + name)
			continue
		}
		// Request values always use SQL bindings, in both parameterization modes.
		// A parameter can be reused or captured; normalization assigns each
		// rendered occurrence its own position and removes unused bindings.
		s.args = append(s.args, value)
		literalKind := valueKind(value)
		if kind == TypeDateTime && value != nil {
			literalKind = sqlast.DateTimeLiteral
		}
		s.scalarLets[key] = boundExpr{SQL: &sqlast.Parameter{Index: len(s.args), Kind: literalKind}, Type: kind}
	}
}

func parameterValue(raw json.RawMessage, kind ScalarType) (any, bool) {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) {
		return nil, kind != TypeString
	}
	if kind == TypeDynamic {
		if !json.Valid(raw) {
			return nil, false
		}
		var compact bytes.Buffer
		if json.Compact(&compact, raw) != nil {
			return nil, false
		}
		return compact.String(), true
	}
	if kind == TypeString || kind == TypeDateTime {
		var text string
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &text) != nil {
			return nil, false
		}
		if kind == TypeString {
			return text, true
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"} {
			if parsed, err := time.Parse(layout, text); err == nil {
				utc := parsed.UTC()
				if utc.Year() >= 1 && utc.Year() <= 9999 {
					return utc.Format(time.RFC3339Nano), true
				}
			}
		}
		return nil, false
	}
	if kind == TypeBool {
		var flag bool
		if json.Unmarshal(raw, &flag) != nil {
			return nil, false
		}
		return flag, true
	}
	if len(raw) == 0 || raw[0] == '"' || !json.Valid(raw) {
		return nil, false
	}
	if kind == TypeInt || kind == TypeLong {
		bits := 64
		if kind == TypeInt {
			bits = 32
		}
		value, err := strconv.ParseInt(string(raw), 10, bits)
		return value, err == nil
	}
	if kind == TypeReal {
		value, err := strconv.ParseFloat(string(raw), 64)
		return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
	}
	return nil, false
}

// Defaults are read as constants rather than lowered through overridable
// functions or caller scope. No callback, table or expression can supply them.
func parameterDefault(expression kql.Expression, expected ScalarType) (json.RawMessage, bool) {
	if call, ok := expression.(*kql.CallExpression); ok {
		name, ok := call.Function.(*kql.NameExpression)
		if !ok || len(call.Args) != 1 {
			return nil, false
		}
		kind, valid := parameterType(name.Name)
		if !valid || kind == TypeString || kind == TypeDynamic {
			return nil, false
		}
		if kind != expected && !(expected == TypeLong && kind == TypeInt) && !(expected == TypeReal && (kind == TypeInt || kind == TypeLong)) {
			return nil, false
		}
		if kind == TypeDateTime {
			if literal, ok := call.Args[0].(*kql.LiteralExpression); ok && strings.EqualFold(literal.Text, "null") {
				return json.RawMessage("null"), true
			}
			text := expressionText(call.Args[0])
			if text == "" {
				return nil, false
			}
			raw, _ := json.Marshal(text)
			return raw, true
		}
		// Typed constructors must contain a single literal, not another call.
		if _, nested := call.Args[0].(*kql.CallExpression); nested {
			return nil, false
		}
		raw, valid := parameterDefault(call.Args[0], kind)
		if !valid {
			return nil, false
		}
		value, valid := parameterValue(raw, kind)
		if !valid {
			return nil, false
		}
		raw, err := json.Marshal(value)
		return raw, err == nil
	}
	negative := false
	if unary, ok := expression.(*kql.UnaryExpression); ok {
		if unary.Operator != "-" && unary.Operator != "+" {
			return nil, false
		}
		literal, ok := unary.Operand.(*kql.LiteralExpression)
		if !ok || literal.Kind != kql.NumberToken {
			return nil, false
		}
		expression = literal
		negative = unary.Operator == "-"
	}
	literal, ok := expression.(*kql.LiteralExpression)
	if !ok {
		return nil, false
	}
	if literal.Kind == kql.StringToken {
		if expected != TypeString {
			return nil, false
		}
		raw, _ := json.Marshal(stringLiteralValue(literal))
		return raw, true
	}
	text := literal.Text
	if negative {
		text = "-" + text
	}
	if strings.EqualFold(text, "null") {
		return json.RawMessage("null"), expected != TypeString
	}
	if expected == TypeBool && (strings.EqualFold(text, "true") || strings.EqualFold(text, "false")) {
		return json.RawMessage(strings.ToLower(text)), true
	}
	if literal.Kind != kql.NumberToken {
		return nil, false
	}
	if expected == TypeInt || expected == TypeLong {
		bits := 64
		if expected == TypeInt {
			bits = 32
		}
		value, err := strconv.ParseInt(text, 10, bits)
		if err != nil {
			return nil, false
		}
		raw, _ := json.Marshal(value)
		return raw, true
	}
	if expected == TypeReal {
		value, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, false
		}
		raw, _ := json.Marshal(value)
		return raw, true
	}
	return nil, false
}
