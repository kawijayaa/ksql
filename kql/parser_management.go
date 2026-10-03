package kql

import "strings"

// ManagementCommand preserves a command header independently of its result
// pipeline. Only the documented table metadata forms are validated here;
// other commands remain opaque and require a command runtime.
type ManagementCommand struct {
	Kind   string
	Tables []string
	Table  string
	Format string
	Raw    []Token
	Span   Span
}

func (command *ManagementCommand) NodeSpan() Span { return command.Span }

func parseManagement(source Source, tokens []Token) (Source, []ParseError) {
	source.Kind = "management"
	command := &ManagementCommand{Raw: tokens, Span: source.Span}
	source.Command = command
	fail := func(message string) (Source, []ParseError) {
		return source, []ParseError{{Code: "KQLP0250", Message: message, Span: source.Span}}
	}
	if len(tokens) < 2 || tokens[1].Kind != IdentifierToken {
		return fail("management command requires a command name after '.'")
	}
	command.Kind = lower(tokens[1])
	if command.Kind != "show" {
		return source, nil
	}
	if len(tokens) < 3 {
		return fail("show requires a command target")
	}
	if lower(tokens[2]) == "tables" {
		command.Kind = "show.tables"
		if len(tokens) == 3 {
			return source, nil
		}
		if lower(tokens[len(tokens)-1]) == "details" {
			command.Kind = "show.tables.details"
			return source, nil
		}
		if tokens[3].Text != "(" || tokens[len(tokens)-1].Text != ")" {
			return fail("show tables accepts an optional parenthesized table list")
		}
		parts := splitTopLevel(tokens[4:len(tokens)-1], ",")
		if len(parts) == 0 {
			return fail("show tables requires a nonempty table list")
		}
		for _, part := range parts {
			name, ok := managementName(part)
			if !ok {
				return fail("show tables requires table names separated by commas")
			}
			command.Tables = append(command.Tables, name)
		}
		return source, nil
	}
	if lower(tokens[2]) != "table" {
		return source, nil
	}
	command.Kind = "show.table"
	if len(tokens) < 4 {
		return fail("show table requires a table name")
	}
	formatAt := 4
	if tokens[3].Text == "[" {
		close := matchingClose(tokens, 3)
		if close < 0 {
			return fail("show table requires a closed quoted table name")
		}
		formatAt = close + 1
	}
	if formatAt >= len(tokens) {
		return source, nil
	}
	if lower(tokens[formatAt]) != "schema" && lower(tokens[formatAt]) != "cslschema" && lower(tokens[formatAt]) != "kqlschema" {
		return source, nil
	}
	name, ok := managementName(tokens[3:formatAt])
	if !ok {
		return fail("show table schema requires one table name")
	}
	command.Table = name
	command.Kind = "show.table.schema"
	tail := tokens[formatAt:]
	if len(tail) == 1 && (lower(tail[0]) == "cslschema" || lower(tail[0]) == "kqlschema") {
		command.Format = "csl"
		return source, nil
	}
	if len(tail) == 3 && lower(tail[0]) == "schema" && lower(tail[1]) == "as" && lower(tail[2]) == "json" {
		command.Format = "json"
		return source, nil
	}
	return fail("expected cslschema, kqlschema or schema as json")
}

func managementName(tokens []Token) (string, bool) {
	if len(tokens) == 1 && tokens[0].Kind == IdentifierToken {
		return tokens[0].Text, true
	}
	if len(tokens) == 1 && tokens[0].Kind == StringToken {
		s, err := DecodeString(tokens[0].Text)
		return s, err == nil && s != ""
	}
	if len(tokens) >= 3 && tokens[0].Text == "[" && tokens[len(tokens)-1].Text == "]" {
		expr, errs := parseExpressionTokens(tokens)
		if len(errs) == 0 {
			if name, ok := expr.(*NameExpression); ok {
				s, err := DecodeString(name.Name)
				return s, err == nil && strings.TrimSpace(s) != ""
			}
		}
	}
	return "", false
}
