package dialect

import "strings"

type sqlite struct{ regexFunction string }

// SQLiteOption configures optional SQLite capabilities supplied by an
// application (usually through registered UDFs).
type SQLiteOption func(*sqlite)

// WithRegexFunction configures a two-argument regex UDF accepting pattern,
// value. Invalid SQL identifiers are ignored.
func WithRegexFunction(name string) SQLiteOption {
	return func(d *sqlite) {
		if validFunctionName(name) {
			d.regexFunction = name
		}
	}
}

// SQLite returns the SQLite renderer with JSON1-compatible access.
func SQLite(options ...SQLiteOption) Dialect {
	d := sqlite{}
	for _, option := range options {
		if option != nil {
			option(&d)
		}
	}
	return d
}

func (sqlite) Name() string                    { return "sqlite" }
func (sqlite) QuoteIdentifier(s string) string { return quoteDouble(s) }
func (sqlite) Placeholder(index int) string    { return "?" }
func (sqlite) Boolean(value bool) string {
	if value {
		return "1"
	}
	return "0"
}
func (d sqlite) Regex(left, right string, caseSensitive bool) (string, bool) {
	if d.regexFunction == "" {
		return "", false
	}
	if !caseSensitive {
		left, right = "lower("+left+")", "lower("+right+")"
	}
	return d.regexFunction + "(" + right + ", " + left + ")", true
}
func (d sqlite) JSONIndex(value, index string) string {
	return d.JSONIndexExpression(value, index, false)
}
func (sqlite) JSONIndexExpression(value, index string, numeric bool) string {
	if numeric {
		return "json_extract(" + value + ", '$[' || " + index + " || ']')"
	}
	// json_each key comparison works for every object key, including quotes
	// and backslashes, on SQLite versions whose JSON path parser cannot address
	// those keys directly.
	return "(SELECT value FROM json_each(" + value + ") WHERE key = " + index + " LIMIT 1)"
}
func (sqlite) SafeCast(_, _ string) (string, bool)        { return "", false }
func (sqlite) Series(_, _, _, _, _ string) (string, bool) { return "", false }
func (sqlite) JSONEach(value, alias string) (string, bool) {
	value = "CASE WHEN " + value + " IS NULL THEN json_array(NULL) ELSE " + value + " END"
	return "json_each(" + value + ") AS " + alias, true
}
func (sqlite) Random() (string, bool)              { return "RANDOM()", true }
func (sqlite) ApplyLimit(sql, limit string) string { return sql + " LIMIT " + limit }
func (sqlite) SupportsNullOrdering() bool          { return true }
func (sqlite) SupportsJoinUsing() bool             { return true }
func (sqlite) Concat(parts []string) (string, bool) {
	return "(" + strings.Join(parts, " || ") + ")", true
}

func validFunctionName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}
