package ksql

import (
	"strconv"
	"strings"

	"github.com/kawijayaa/ksql/kql"
)

// Visualization is the render operator's annotation. It never changes rows.
// Property values are strings, bools, float64 (ymin, ymax) or []string
// (series, ycolumns, anomalycolumns).
type Visualization struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties,omitempty"`
}

var renderVisualizations = map[string]bool{
	"anomalychart": true, "areachart": true, "barchart": true, "card": true, "columnchart": true,
	"ladderchart": true, "linechart": true, "piechart": true, "pivotchart": true, "scatterchart": true,
	"stackedareachart": true, "table": true, "timechart": true, "timepivot": true, "treemap": true,
}

var renderKinds = map[string]map[string]bool{
	"areachart":    {"default": true, "unstacked": true, "stacked": true, "stacked100": true},
	"barchart":     {"default": true, "unstacked": true, "stacked": true, "stacked100": true},
	"columnchart":  {"default": true, "unstacked": true, "stacked": true, "stacked100": true},
	"scatterchart": {"map": true},
	"piechart":     {"map": true},
}

var renderEnums = map[string]map[string]bool{
	"legend": {"visible": true, "hidden": true},
	"xaxis":  {"linear": true, "log": true},
	"yaxis":  {"linear": true, "log": true},
	"ysplit": {"none": true, "axes": true, "panels": true},
}

// lastRender splits a final top-level render from its pipeline.
func lastRender(pipeline *kql.Pipeline) (*kql.Pipeline, *kql.Operator) {
	if pipeline == nil || len(pipeline.Operators) == 0 || pipeline.Operators[len(pipeline.Operators)-1].Kind != "render" {
		return pipeline, nil
	}
	copy := *pipeline
	copy.Operators = pipeline.Operators[:len(pipeline.Operators)-1]
	return &copy, &pipeline.Operators[len(pipeline.Operators)-1]
}

// render validates a render operator against the final schema, following
// Microsoft's visualization, kind and property tables.
func (s *compileState) render(operator kql.Operator, schema Schema) *Visualization {
	feature := "operator.render"
	s.use(feature, "equivalent", operator.Span)
	spec := operator.Body.(kql.RenderSpec)
	if !renderVisualizations[spec.Visualization] {
		s.bindError("KQLB0397", spec.Span, feature, "unknown render visualization "+spec.Visualization)
		return nil
	}
	result := &Visualization{Type: spec.Visualization, Properties: map[string]any{}}
	fail := func(span kql.Span, message string) *Visualization {
		s.bindError("KQLB0398", span, feature, message)
		return nil
	}
	single := func(property kql.RenderProperty) ([]kql.Token, bool) {
		if len(property.Values) != 1 || len(property.Values[0]) == 0 {
			return nil, false
		}
		return property.Values[0], true
	}
	column := func(tokens []kql.Token, span kql.Span) (string, bool) {
		name := ""
		if len(tokens) == 1 && tokens[0].Kind == kql.IdentifierToken {
			name = tokens[0].Text
		} else if len(tokens) >= 3 && tokens[0].Text == "[" && tokens[len(tokens)-1].Text == "]" {
			parts := []string{}
			for _, token := range tokens[1 : len(tokens)-1] {
				if token.Kind != kql.StringToken {
					return "", false
				}
				parts = append(parts, token.Text)
			}
			decoded, err := kql.StringValue(&kql.LiteralExpression{Parts: parts})
			if err != nil {
				return "", false
			}
			name = decoded
		} else {
			return "", false
		}
		if !schema.Unknown {
			found, _, ok, _ := schema.Lookup(name)
			if !ok {
				s.bindError("KQLB0401", span, feature, "unknown render column "+name)
				return "", false
			}
			name = found.Name
		}
		return name, true
	}
	for _, property := range spec.Properties {
		if _, duplicate := result.Properties[property.Name]; duplicate {
			return fail(property.Span, "duplicate render property "+property.Name)
		}
		switch property.Name {
		case "series", "ycolumns", "anomalycolumns":
			if property.Name == "anomalycolumns" && spec.Visualization != "anomalychart" {
				return fail(property.Span, "anomalycolumns applies only to anomalychart")
			}
			columns := []string{}
			for _, value := range property.Values {
				name, ok := column(value, property.Span)
				if !ok {
					if s.hasErrors() {
						return nil
					}
					return fail(property.Span, "render "+property.Name+" requires column names")
				}
				columns = append(columns, name)
			}
			result.Properties[property.Name] = columns
			continue
		}
		tokens, ok := single(property)
		if !ok {
			return fail(property.Span, "render "+property.Name+" takes one value")
		}
		text := strings.ToLower(tokens[0].Text)
		switch property.Name {
		case "xcolumn":
			name, ok := column(tokens, property.Span)
			if !ok {
				if s.hasErrors() {
					return nil
				}
				return fail(property.Span, "render xcolumn requires a column name")
			}
			result.Properties[property.Name] = name
		case "title", "xtitle", "ytitle":
			if len(tokens) != 1 || tokens[0].Kind != kql.StringToken {
				return fail(property.Span, "render "+property.Name+" requires a string literal")
			}
			value, err := kql.DecodeString(tokens[0].Text)
			if err != nil {
				return fail(property.Span, "invalid render string literal")
			}
			result.Properties[property.Name] = value
		case "ymin", "ymax":
			literal := ""
			for _, token := range tokens {
				literal += token.Text
			}
			value, err := strconv.ParseFloat(literal, 64)
			if err != nil || (tokens[len(tokens)-1].Kind != kql.NumberToken) {
				return fail(property.Span, "render "+property.Name+" requires a number")
			}
			result.Properties[property.Name] = value
		case "accumulate":
			if len(tokens) != 1 || (text != "true" && text != "false") {
				return fail(property.Span, "render accumulate requires true or false")
			}
			result.Properties[property.Name] = text == "true"
		case "kind":
			if len(tokens) != 1 || !renderKinds[spec.Visualization][text] {
				return fail(property.Span, "render kind "+tokens[0].Text+" is not valid for "+spec.Visualization)
			}
			result.Properties[property.Name] = text
		case "legend", "xaxis", "yaxis", "ysplit":
			if len(tokens) != 1 || !renderEnums[property.Name][text] {
				return fail(property.Span, "invalid render "+property.Name+" value "+tokens[0].Text)
			}
			result.Properties[property.Name] = text
		default:
			return fail(property.Span, "unknown render property "+property.Name)
		}
	}
	if len(result.Properties) == 0 {
		result.Properties = nil
	}
	return result
}

func (s *compileState) hasErrors() bool {
	for _, diagnostic := range s.diagnostics {
		if diagnostic.Severity == SeverityError {
			return true
		}
	}
	return false
}
