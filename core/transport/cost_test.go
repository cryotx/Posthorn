package transport

import (
	"strings"
	"testing"
)

func TestParseCost(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want Cost
	}{
		{"nil is free", nil, 0},
		{"int zero", int64(0), 0},
		{"int whole units", int64(2), 2 * CostScale},
		{"plain int", 3, 3 * CostScale},
		{"string zero", "0", 0},
		{"string two decimals", "0.10", 100_000},
		{"string trailing digit", "0.1", 100_000},
		{"string full precision", "1.234567", 1_234_567},
		{"string whole", "12", 12 * CostScale},
		{"string with sign", "+0.25", 250_000},
		{"string empty is free", "", 0},
		{"string spaces trimmed", "  0.35  ", 350_000},
		{"float", 0.10, 100_000},
		{"float rounds half up", 0.0000005, 1},
		{"float whole", 2.0, 2 * CostScale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCost(tc.in)
			if err != nil {
				t.Fatalf("ParseCost(%v): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseCost(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// The string form is the one an operator should reach for precisely
// because it never passes through a float. This pins that: 0.07 has no
// exact float64 representation, and the parsed value must still be the
// digits that were written.
func TestParseCost_StringFormIsExact(t *testing.T) {
	for _, s := range []string{"0.07", "0.29", "0.83", "0.000001"} {
		got, err := ParseCost(s)
		if err != nil {
			t.Fatalf("ParseCost(%q): %v", s, err)
		}
		if got.String() != strings.TrimPrefix(s, "+") {
			t.Errorf("round trip of %q = %q", s, got.String())
		}
	}
}

func TestParseCost_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"non-numeric string", "cheap", "not a decimal number"},
		{"too many decimals", "0.1234567", "more than 6 decimal places"},
		{"empty fraction", "1.", "not a decimal number"},
		{"bare dot", ".", "not a decimal number"},
		{"wrong type", []string{"0.1"}, "must be a number"},
		{"bool", true, "must be a number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCost(tc.in)
			if err == nil {
				t.Fatalf("ParseCost(%v) = nil error, want %q", tc.in, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestCost_Free(t *testing.T) {
	if !Cost(0).Free() {
		t.Error("Cost(0).Free() = false")
	}
	if Cost(1).Free() {
		t.Error("Cost(1).Free() = true — a tenth of a millionth is still a price")
	}
}

func TestCost_String(t *testing.T) {
	for _, tc := range []struct {
		in   Cost
		want string
	}{
		{0, "0"},
		{100_000, "0.1"},
		{1_234_567, "1.234567"},
		{2 * CostScale, "2"},
		{-100_000, "-0.1"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Cost(%d).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Routing compares costs as integers. Two prices an operator wrote as
// equal must compare equal, which is the property float arithmetic
// cannot promise.
func TestCost_ComparisonIsExact(t *testing.T) {
	a, _ := ParseCost("0.1")
	b, _ := ParseCost("0.10")
	c, _ := ParseCost(0.1)
	if a != b || b != c {
		t.Errorf("0.1 parsed three ways gave %d, %d, %d", a, b, c)
	}
	// The float sum that famously is not 0.3.
	sum, _ := ParseCost(0.1 + 0.2)
	exact, _ := ParseCost("0.3")
	if sum != exact {
		t.Errorf("quantised 0.1+0.2 = %d, want %d — quantisation should absorb float drift", sum, exact)
	}
}
