package ksql

import (
	"math/big"
	"regexp"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// Timespans are 100-nanosecond ticks, Kusto's own representation, stored as
// SQL integers. Datetime arithmetic is a dialect capability because each
// application chooses its datetime storage.

const ticksPerDay = 864000000000

var timespanUnits = map[string]int64{
	"d": ticksPerDay, "day": ticksPerDay, "days": ticksPerDay,
	"h": 36000000000, "hr": 36000000000, "hrs": 36000000000, "hour": 36000000000, "hours": 36000000000,
	"m": 600000000, "min": 600000000, "minute": 600000000, "minutes": 600000000,
	"s": 10000000, "sec": 10000000, "second": 10000000, "seconds": 10000000,
	"ms": 10000, "milli": 10000, "millis": 10000, "millisec": 10000, "millisecond": 10000, "milliseconds": 10000,
	"microsec": 10, "microsecond": 10, "microseconds": 10,
	"nanosec": 0, "nanosecond": 0, "nanoseconds": 0,
	"tick": 1, "ticks": 1,
}

var (
	timespanUnitPattern  = regexp.MustCompile(`^(-?)(\d+(?:\.\d+)?)\s*([A-Za-z]+)$`)
	timespanClockPattern = regexp.MustCompile(`^(-?)(?:(\d+)\.)?(\d{1,2}):(\d{2})(?::(\d{2})(?:\.(\d{1,7}))?)?$`)
	timespanDaysPattern  = regexp.MustCompile(`^(-?)(\d+(?:\.\d+)?)$`)
)

// timespanTicks parses a timespan literal: a number with a unit (1.5h, 10ms,
// 1tick), [-][d.]hh:mm[:ss[.fffffff]], or a bare number of days as in time(2).
// Fractions round to the nearest tick.
func timespanTicks(text string) (int64, bool) {
	text = strings.TrimSpace(text)
	scaled := func(sign, number string, unit *big.Rat) (int64, bool) {
		value, ok := new(big.Rat).SetString(number)
		if !ok {
			return 0, false
		}
		value.Mul(value, unit)
		value.Add(value, big.NewRat(1, 2))
		ticks := new(big.Int).Quo(value.Num(), value.Denom())
		if !ticks.IsInt64() {
			return 0, false
		}
		if sign == "-" {
			return -ticks.Int64(), true
		}
		return ticks.Int64(), true
	}
	if match := timespanUnitPattern.FindStringSubmatch(text); match != nil {
		unit, ok := timespanUnits[strings.ToLower(match[3])]
		if !ok {
			return 0, false
		}
		rate := big.NewRat(unit, 1)
		if strings.HasPrefix(strings.ToLower(match[3]), "nano") {
			rate = big.NewRat(1, 100)
		}
		return scaled(match[1], match[2], rate)
	}
	if match := timespanClockPattern.FindStringSubmatch(text); match != nil {
		parts := []string{match[2], match[3], match[4], match[5]}
		var ticks int64
		for i, part := range parts {
			value := int64(0)
			for _, digit := range part {
				value = value*10 + int64(digit-'0')
			}
			if (i == 1 && value > 23) || (i > 1 && value > 59) {
				return 0, false
			}
			if i == 0 && value > 10675199 {
				return 0, false
			}
			ticks += value * []int64{ticksPerDay, 36000000000, 600000000, 10000000}[i]
		}
		if match[6] != "" {
			fraction := (match[6] + "0000000")[:7]
			value := int64(0)
			for _, digit := range fraction {
				value = value*10 + int64(digit-'0')
			}
			ticks += value
		}
		if match[1] == "-" {
			ticks = -ticks
		}
		return ticks, true
	}
	if match := timespanDaysPattern.FindStringSubmatch(text); match != nil {
		return scaled(match[1], match[2], big.NewRat(ticksPerDay, 1))
	}
	return 0, false
}

func (s *compileState) timespanLiteral(text string, span kql.Span) boundExpr {
	if strings.EqualFold(strings.TrimSpace(text), "null") {
		return boundExpr{SQL: &sqlast.Literal{Kind: sqlast.NullLiteral}, Type: TypeTimespan}
	}
	ticks, ok := timespanTicks(text)
	if !ok {
		s.bindError("KQLB0411", span, "expression.timespan", "invalid timespan literal "+text)
		return boundExpr{}
	}
	return boundExpr{SQL: s.bind(ticks), Type: TypeTimespan}
}

func supportsDatetimeArithmetic(target any) bool {
	capability, ok := target.(interface {
		DatetimeAdd(datetime, ticks string) (string, bool)
		DatetimeDiff(left, right string) (string, bool)
	})
	if !ok {
		return false
	}
	_, added := capability.DatetimeAdd("", "")
	_, subtracted := capability.DatetimeDiff("", "")
	return added && subtracted
}

func isNumeric(t ScalarType) bool {
	return t == TypeInt || t == TypeLong || t == TypeReal
}

// temporalArithmetic types +, -, *, / and % over datetimes and timespans,
// following Kusto's operator overloads. handled is false when neither operand
// is temporal.
func (s *compileState) temporalArithmetic(op string, left, right boundExpr, span kql.Span) (result boundExpr, handled bool) {
	if left.Type != TypeDateTime && left.Type != TypeTimespan && right.Type != TypeDateTime && right.Type != TypeTimespan {
		return boundExpr{}, false
	}
	invalid := func() (boundExpr, bool) {
		s.bindError("KQLB0412", span, scalarOperatorID(op), "operator "+op+" is not defined for "+string(left.Type)+" and "+string(right.Type))
		return boundExpr{}, true
	}
	if left.Type == TypeUnknown || right.Type == TypeUnknown {
		// An untyped operand cannot select the overload; keep plain SQL.
		resultType := left.Type
		if resultType == TypeUnknown {
			resultType = right.Type
		}
		return boundExpr{SQL: &sqlast.Binary{Left: left.SQL, Operator: op, Right: right.SQL}, Type: resultType}, true
	}
	datetimeCall := func(expression sqlast.Expr, kind ScalarType) (boundExpr, bool) {
		if !supportsDatetimeArithmetic(s.compiler.dialect) {
			s.error("KQLL0412", span, scalarOperatorID(op), "datetime arithmetic requires a dialect datetime capability")
			return boundExpr{}, true
		}
		return boundExpr{SQL: expression, Type: kind}, true
	}
	switch {
	case op == "+" && left.Type == TypeDateTime && right.Type == TypeTimespan:
		return datetimeCall(&sqlast.DatetimeAdd{Datetime: left.SQL, Ticks: right.SQL}, TypeDateTime)
	case op == "+" && left.Type == TypeTimespan && right.Type == TypeDateTime:
		return datetimeCall(&sqlast.DatetimeAdd{Datetime: right.SQL, Ticks: left.SQL}, TypeDateTime)
	case op == "-" && left.Type == TypeDateTime && right.Type == TypeTimespan:
		return datetimeCall(&sqlast.DatetimeAdd{Datetime: left.SQL, Ticks: &sqlast.Unary{Operator: "-", Operand: right.SQL}}, TypeDateTime)
	case op == "-" && left.Type == TypeDateTime && right.Type == TypeDateTime:
		return datetimeCall(&sqlast.DatetimeDiff{Left: left.SQL, Right: right.SQL}, TypeTimespan)
	case (op == "+" || op == "-" || op == "%") && left.Type == TypeTimespan && right.Type == TypeTimespan:
		return boundExpr{SQL: &sqlast.Binary{Left: left.SQL, Operator: op, Right: right.SQL}, Type: TypeTimespan}, true
	case op == "/" && left.Type == TypeTimespan && right.Type == TypeTimespan:
		real := &sqlast.Cast{Expr: left.SQL, Type: "DOUBLE PRECISION"}
		return boundExpr{SQL: &sqlast.Binary{Left: real, Operator: "/", Right: right.SQL}, Type: TypeReal}, true
	case (op == "*" || op == "/") && left.Type == TypeTimespan && isNumeric(right.Type),
		op == "*" && isNumeric(left.Type) && right.Type == TypeTimespan:
		product := sqlast.Expr(&sqlast.Binary{Left: left.SQL, Operator: op, Right: right.SQL})
		if left.Type == TypeReal || right.Type == TypeReal || op == "/" {
			// Ticks are integral; scaled timespans truncate toward zero.
			if op == "/" {
				product = &sqlast.Binary{Left: &sqlast.Cast{Expr: left.SQL, Type: "DOUBLE PRECISION"}, Operator: "/", Right: right.SQL}
			}
			product = &sqlast.Cast{Expr: product, Type: "BIGINT"}
		}
		return boundExpr{SQL: product, Type: TypeTimespan}, true
	}
	return invalid()
}
