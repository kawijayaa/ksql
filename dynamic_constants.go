package ksql

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kawijayaa/ksql/kql"
)

// Datetime layouts accepted inside dynamic literals.
var dynamicDatetimeLayouts = []string{
	time.RFC3339Nano, "2006-01-02T15:04:05.9999999", "2006-01-02 15:04:05.9999999", "2006-01-02 15:04:05",
	"2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02", "01-02-2006", "01/02/2006",
}

// rewriteDynamicConstants replaces typed constants inside a dynamic literal
// with their JSON forms: nested dynamic(...) is inlined, datetime(...) and
// guid(...) become strings (datetimes as Kusto's ISO text), timespans their
// [d.]hh:mm:ss text, and int/long/real/bool(...) their values.
func rewriteDynamicConstants(tokens []kql.Token) ([]kql.Token, error) {
	result := make([]kql.Token, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		name := strings.ToLower(token.Text)
		if token.Kind != kql.IdentifierToken || i+1 >= len(tokens) || tokens[i+1].Text != "(" {
			if token.Kind == kql.NumberToken && isTimespan(token.Text) && hexOrTimespan(token.Text) {
				ticks, ok := timespanTicks(token.Text)
				if !ok {
					return nil, fmt.Errorf("invalid timespan %s", token.Text)
				}
				result = append(result, jsonToken(FormatTimespan(ticks), token.Span))
				continue
			}
			if value, ok := hexLiteral(token.Text); ok && token.Kind == kql.NumberToken {
				result = append(result, kql.Token{Kind: kql.NumberToken, Text: strconv.FormatInt(value, 10), Span: token.Span})
				continue
			}
			result = append(result, token)
			continue
		}
		close := -1
		depth := 0
		for j := i + 1; j < len(tokens); j++ {
			switch tokens[j].Text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			}
			if depth == 0 {
				close = j
				break
			}
		}
		if close < 0 {
			return nil, fmt.Errorf("unbalanced %s(", token.Text)
		}
		inner := tokens[i+2 : close]
		var raw strings.Builder
		for k, part := range inner {
			if k > 0 && part.Span.Offset > inner[k-1].Span.Offset+inner[k-1].Span.Length {
				raw.WriteByte(' ')
			}
			raw.WriteString(part.Text)
		}
		text := raw.String()
		if len(inner) == 1 && inner[0].Kind == kql.StringToken {
			text, _ = kql.StringValue(&kql.LiteralExpression{Parts: []string{inner[0].Text}, Kind: kql.StringToken})
		}
		var replacement []kql.Token
		switch name {
		case "dynamic":
			nested, err := rewriteDynamicConstants(inner)
			if err != nil {
				return nil, err
			}
			replacement = nested
		case "datetime", "date":
			parsed, ok := parseDynamicDatetime(text)
			if !ok {
				return nil, fmt.Errorf("invalid datetime %s", text)
			}
			replacement = []kql.Token{jsonToken(parsed, token.Span)}
		case "timespan", "time":
			ticks, ok := timespanTicks(text)
			if !ok {
				return nil, fmt.Errorf("invalid timespan %s", text)
			}
			replacement = []kql.Token{jsonToken(FormatTimespan(ticks), token.Span)}
		case "guid", "uniqueid":
			replacement = []kql.Token{jsonToken(strings.ToLower(text), token.Span)}
		case "int", "long", "real", "double", "decimal", "bool", "boolean":
			if strings.EqualFold(text, "null") {
				replacement = []kql.Token{{Kind: kql.IdentifierToken, Text: "null", Span: token.Span}}
				break
			}
			if !json.Valid([]byte(strings.ToLower(text))) {
				return nil, fmt.Errorf("invalid %s constant %s", name, text)
			}
			replacement = []kql.Token{{Kind: kql.NumberToken, Text: strings.ToLower(text), Span: token.Span}}
		default:
			result = append(result, token)
			continue
		}
		result = append(result, replacement...)
		i = close
	}
	return result, nil
}

// hexOrTimespan reports a number token that is a timespan rather than hex.
func hexOrTimespan(text string) bool {
	_, hex := hexLiteral(text)
	return !hex
}

func jsonToken(value string, span kql.Span) kql.Token {
	encoded, _ := json.Marshal(value)
	return kql.Token{Kind: kql.StringToken, Text: string(encoded), Span: span}
}

func parseDynamicDatetime(text string) (string, bool) {
	text = strings.TrimSpace(text)
	for _, layout := range dynamicDatetimeLayouts {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed.UTC().Format("2006-01-02T15:04:05.0000000Z"), true
		}
	}
	return "", false
}
