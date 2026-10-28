package runtime

import (
	"math"
	"strconv"
	"strings"
)

// ScalarText converts a database value to Kusto's text form for tostring and
// strcat: reals use the shortest round-trip digits, switching to E notation
// (as in 1E+15 and 1E-05) for exponents of 15 and above or below -4. Other
// values become their usual text; SQL NULL stays NULL.
func ScalarText(value any) any {
	switch current := value.(type) {
	case nil:
		return nil
	case []byte:
		if current == nil {
			return nil
		}
		return string(current)
	case string:
		return current
	case int64:
		return strconv.FormatInt(current, 10)
	case float64:
		return FormatReal(current)
	case bool:
		return strconv.FormatBool(current)
	}
	return nil
}

// FormatReal formats a real as Kusto (.NET) does.
func FormatReal(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	case value == 0:
		return "0"
	}
	scientific := strconv.FormatFloat(value, 'e', -1, 64)
	mantissa, exponentText, _ := strings.Cut(scientific, "e")
	exponent, _ := strconv.Atoi(exponentText)
	if exponent >= -4 && exponent < 15 {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	sign := "+"
	if exponent < 0 {
		sign, exponent = "-", -exponent
	}
	digits := strconv.Itoa(exponent)
	if len(digits) < 2 {
		digits = "0" + digits
	}
	return mantissa + "E" + sign + digits
}
