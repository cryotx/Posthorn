package transport

import (
	"fmt"
	"strconv"
	"strings"
)

// CostScale is the number of fixed-point units in one currency unit.
// Six decimal places: enough for per-1000 prices quoted to a tenth of a
// cent, with room to spare, and small enough that an int64 cannot
// realistically overflow.
const CostScale = 1_000_000

// Cost is a price per 1000 messages, held as a fixed-point integer in
// millionths of a currency unit. $0.10 per 1000 is Cost(100_000).
//
// Money never touches float64 here. Routing decisions compare costs, and
// two prices that differ in the seventh decimal place of a float — or
// that merely round differently — would make `cheapest` pick a different
// provider on different runs for no reason an operator could see. An
// integer comparison is exact, stable, and trivially testable.
//
// The currency is whatever the operator's provider bills in. Posthorn
// never converts between currencies; a pool priced in two currencies is
// an operator error we cannot detect, so the docs say to normalise
// before writing the config.
type Cost int64

// Free reports whether this provider costs nothing per message — the
// predicate the free_first strategy partitions on.
func (c Cost) Free() bool { return c <= 0 }

// String renders the cost with trailing zeros trimmed, for logs and
// error messages.
func (c Cost) String() string {
	neg := c < 0
	v := int64(c)
	if neg {
		v = -v
	}
	whole := v / CostScale
	frac := v % CostScale
	out := strconv.FormatInt(whole, 10)
	if frac != 0 {
		f := strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
		out += "." + f
	}
	if neg {
		out = "-" + out
	}
	return out
}

// ParseCost converts a TOML-decoded value to a Cost.
//
// Three input shapes are accepted, in decreasing order of preference:
//
//	cost_per_1000 = "0.10"   string  — parsed exactly, digit by digit
//	cost_per_1000 = 0        integer — whole currency units
//	cost_per_1000 = 0.10     float   — quantised to six decimal places
//
// The float form is accepted because it is what an operator will write
// first, and quantising it at parse time is deterministic. Everything
// after this function is integer arithmetic.
func ParseCost(v any) (Cost, error) {
	switch t := v.(type) {
	case nil:
		return 0, nil
	case string:
		return parseCostString(t)
	case int64:
		return costFromUnits(t)
	case int:
		return costFromUnits(int64(t))
	case float64:
		if t != t || t > 9e12 || t < -9e12 {
			return 0, fmt.Errorf("cost %v is not a finite, representable price", t)
		}
		// Round half away from zero at the sixth decimal place.
		scaled := t * CostScale
		if scaled < 0 {
			return Cost(int64(scaled - 0.5)), nil
		}
		return Cost(int64(scaled + 0.5)), nil
	default:
		return 0, fmt.Errorf("cost must be a number or a decimal string, got %T", v)
	}
}

func costFromUnits(n int64) (Cost, error) {
	if n > (1<<62)/CostScale || n < -((1<<62)/CostScale) {
		return 0, fmt.Errorf("cost %d is out of range", n)
	}
	return Cost(n * CostScale), nil
}

// parseCostString reads a plain decimal ("0.10", "1", "-0.5") without
// going through float64, so the written digits are the stored digits.
func parseCostString(s string) (Cost, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, nil
	}
	neg := false
	switch raw[0] {
	case '-':
		neg, raw = true, raw[1:]
	case '+':
		raw = raw[1:]
	}
	intPart, fracPart, hasFrac := strings.Cut(raw, ".")
	if intPart == "" && !hasFrac {
		return 0, fmt.Errorf("cost %q is not a decimal number", s)
	}
	if intPart == "" {
		intPart = "0"
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cost %q is not a decimal number", s)
	}
	var frac int64
	if hasFrac {
		if len(fracPart) > 6 {
			return 0, fmt.Errorf("cost %q has more than 6 decimal places (the fixed-point precision)", s)
		}
		if fracPart == "" {
			return 0, fmt.Errorf("cost %q is not a decimal number", s)
		}
		padded := fracPart + strings.Repeat("0", 6-len(fracPart))
		frac, err = strconv.ParseInt(padded, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("cost %q is not a decimal number", s)
		}
	}
	if whole > (1<<62)/CostScale {
		return 0, fmt.Errorf("cost %q is out of range", s)
	}
	total := whole*CostScale + frac
	if neg {
		total = -total
	}
	return Cost(total), nil
}
