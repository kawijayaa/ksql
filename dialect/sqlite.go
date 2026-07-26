package dialect

type sqlite struct{}

// SQLite returns the SQLite renderer with JSON1-compatible access.
func SQLite() Dialect { return sqlite{} }

func (sqlite) Name() string                    { return "sqlite" }
func (sqlite) QuoteIdentifier(s string) string { return quoteDouble(s) }
func (sqlite) Placeholder(index int) string    { return "?" }
func (sqlite) Boolean(value bool) string {
	if value {
		return "1"
	}
	return "0"
}
func (sqlite) Regex(_, _ string, _ bool) (string, bool) { return "", false }
func (sqlite) JSONIndex(value, index string) string {
	return "json_extract(" + value + ", '$.' || " + index + ")"
}
func (sqlite) SafeCast(_, _ string) (string, bool)        { return "", false }
func (sqlite) Series(_, _, _, _, _ string) (string, bool) { return "", false }
func (sqlite) ApplyLimit(sql, limit string) string        { return sql + " LIMIT " + limit }
func (sqlite) SupportsNullOrdering() bool                 { return true }
func (sqlite) SupportsJoinUsing() bool                    { return true }
