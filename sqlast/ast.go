// Package sqlast defines the dialect-neutral SQL representation emitted by
// the KQL compiler.
package sqlast

// Query is a SQL query expression.
type Query interface {
	queryNode()
}

// Select is one SQL SELECT query block.
type Select struct {
	Distinct    bool
	Projections []SelectItem
	From        Source
	Where       Expr
	GroupBy     []Expr
	OrderBy     []Order
	Limit       Expr
	// Fence asks dialects that inline subqueries to evaluate this select's
	// projections once per row. It never changes results.
	Fence bool
}

func (*Select) queryNode() {}

// Union combines query operands. All selects UNION ALL when All is true.
type Union struct {
	All     bool
	Queries []Query
}

func (*Union) queryNode() {}

// With materializes reusable query values before evaluating Query.
type With struct {
	Bindings []Binding
	Query    Query
}
type Binding struct {
	Name  string
	Query Query
}

func (*With) queryNode() {}

// RecursiveQuery is WITH RECURSIVE Name(Columns) AS (Anchor UNION ALL Step)
// Query; Step and Query reference the binding through CTE sources.
type RecursiveQuery struct {
	Name    string
	Columns []string
	Anchor  Query
	Step    Query
	Query   Query
}

func (*RecursiveQuery) queryNode() {}

// CTE references a common table expression by name.
type CTE struct {
	Name  string
	Alias string
}

func (*CTE) sourceNode() {}

type SelectItem struct {
	Expr  Expr
	Alias string
}

type Order struct {
	Expr      Expr
	Direction string
	Nulls     string
}

// Source is a FROM-clause relation.
type Source interface {
	sourceNode()
}

type Table struct {
	Parts []string
	Alias string
}

func (*Table) sourceNode() {}

// MaterializedReference reads a reusable table binding without inlining its query.
type MaterializedReference struct{ Name string }

func (*MaterializedReference) sourceNode() {}

type Subquery struct {
	Query Query
	Alias string
}

func (*Subquery) sourceNode() {}

type Join struct {
	Left  Source
	Right Source
	Kind  string
	On    Expr
	Using []string
}

func (*Join) sourceNode() {}

type Values struct {
	Columns []string
	Rows    [][]Expr
	Alias   string
}

func (*Values) sourceNode() {}

type Series struct {
	Column string
	From   Expr
	To     Expr
	Step   Expr
	Alias  string
}

func (*Series) sourceNode() {}

// JSONEach expands one JSON array value into value/key rows.
type JSONEach struct {
	Value Expr
	Alias string
}

func (*JSONEach) sourceNode() {}

// Expr is a SQL scalar expression.
type Expr interface {
	exprNode()
}

type Identifier struct{ Parts []string }
type Literal struct {
	Kind  LiteralKind
	Value string
}
type Parameter struct {
	Index int
	Kind  LiteralKind
}
type Star struct{}
type QualifiedStar struct{ Parts []string }
type Random struct{}
type Unary struct {
	Operator string
	Operand  Expr
}
type Binary struct {
	Left     Expr
	Operator string
	Right    Expr
}
type Call struct {
	Name string
	Args []Expr
}

// Window applies a SQL window function to a partition and ordering.
type Window struct {
	Expr        Expr
	PartitionBy []Expr
	OrderBy     []Order
	// Cumulative frames the window from the partition start to the current
	// row, so ordering ties never aggregate peer rows.
	Cumulative bool
	// Rows, when set, frames the window from Preceding rows before to
	// Following rows after the current row.
	Rows *RowsFrame
}

// RowsFrame is ROWS BETWEEN Preceding PRECEDING AND Following FOLLOWING.
type RowsFrame struct{ Preceding, Following int64 }
type List struct{ Items []Expr }
type Case struct {
	Branches []When
	Else     Expr
}
type When struct {
	Condition Expr
	Result    Expr
}
type Cast struct {
	Expr Expr
	Type string
	Safe bool
}

// DynamicValue encodes a known scalar as a JSON dynamic value. Kind carries
// the KQL type, since SQLite stores Booleans using integer values.
type DynamicValue struct {
	Value Expr
	Kind  string
}
type Index struct {
	Value Expr
	Index Expr
	// Numeric records a bound integer or Boolean index, including expressions.
	Numeric bool
}
type Regex struct {
	Value         Expr
	Pattern       Expr
	CaseSensitive bool
}

// ScalarSubquery is a subquery returning one value.
type ScalarSubquery struct{ Query Query }

// OrderedAggregate is an aggregate whose input rows are ordered, such as
// json_group_array(x ORDER BY k).
type OrderedAggregate struct {
	Name    string
	Args    []Expr
	OrderBy []Order
}

// ExpansionArray normalizes a dynamic value for mv-expand: arrays stay as
// they are, property bags become an array of single-property bags (or of
// [key, value] pairs when Pairs is set), and other values become null.
type ExpansionArray struct {
	Value Expr
	Pairs bool
}

// InSubquery tests whether Value equals a non-null Column of Query.
type InSubquery struct {
	Value           Expr
	Query           Query
	Column          string
	CaseInsensitive bool
}

// ArrayContains tests whether the JSON array Array has an element equal to
// Value, optionally comparing lowercase text.
type ArrayContains struct {
	Array, Value    Expr
	CaseInsensitive bool
}

// DatetimeAdd adds Ticks (100ns units) to Datetime.
type DatetimeAdd struct{ Datetime, Ticks Expr }

// DatetimeDiff returns Left - Right in ticks.
type DatetimeDiff struct{ Left, Right Expr }

// TimespanText formats ticks as [-][d.]hh:mm:ss[.fffffff].
type TimespanText struct{ Value Expr }

// ScalarText converts a non-string scalar to text. Dialects may format reals
// as Kusto does (shortest round-trip digits); otherwise it is a VARCHAR cast.
type ScalarText struct{ Value Expr }

// StringMatch tests whether Value contains, starts with or ends with Pattern
// literally (Mode "contains", "startswith" or "endswith").
type StringMatch struct {
	Value         Expr
	Pattern       Expr
	Mode          string
	CaseSensitive bool
}

// RegexCaptures returns a JSON array of the capture groups of the first
// match of Pattern in Value (group one at index zero), or null on no match.
type RegexCaptures struct {
	Value   Expr
	Pattern Expr
}

// KeyValues returns a JSON object of requested parse-kv keys extracted from
// Value, configured by a JSON runtime.KeyValueOptions document in Options.
type KeyValues struct {
	Value   Expr
	Options Expr
}
type Exists struct{ Query Query }

// ScalarReference reads the first value from a materialized scalar binding.
type ScalarReference struct{ Name string }

// RankedValue aggregates the value from the unique row numbered one.
// Rank is an identifier so renderer-specific argument order cannot duplicate parameters.
type RankedValue struct {
	Value Expr
	Rank  *Identifier
}

type Filtered struct {
	Expr  Expr
	Where Expr
}

func (*Identifier) exprNode()       {}
func (*Literal) exprNode()          {}
func (*Parameter) exprNode()        {}
func (*Star) exprNode()             {}
func (*QualifiedStar) exprNode()    {}
func (*Random) exprNode()           {}
func (*Unary) exprNode()            {}
func (*Binary) exprNode()           {}
func (*Window) exprNode()           {}
func (*Call) exprNode()             {}
func (*List) exprNode()             {}
func (*Case) exprNode()             {}
func (*Cast) exprNode()             {}
func (*DynamicValue) exprNode()     {}
func (*Index) exprNode()            {}
func (*Regex) exprNode()            {}
func (*RegexCaptures) exprNode()    {}
func (*KeyValues) exprNode()        {}
func (*StringMatch) exprNode()      {}
func (*DatetimeAdd) exprNode()      {}
func (*DatetimeDiff) exprNode()     {}
func (*TimespanText) exprNode()     {}
func (*ScalarText) exprNode()       {}
func (*ArrayContains) exprNode()    {}
func (*InSubquery) exprNode()       {}
func (*ExpansionArray) exprNode()   {}
func (*ScalarSubquery) exprNode()   {}
func (*OrderedAggregate) exprNode() {}
func (*Exists) exprNode()           {}
func (*ScalarReference) exprNode()  {}
func (*RankedValue) exprNode()      {}
func (*Filtered) exprNode()         {}

type LiteralKind uint8

const (
	NullLiteral LiteralKind = iota
	NumberLiteral
	StringLiteral
	BooleanLiteral
	DateTimeLiteral
	IntervalLiteral
	JSONLiteral
)
