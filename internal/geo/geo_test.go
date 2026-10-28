package geo

import (
	"math"
	"testing"
)

func TestParseDecimalDegrees(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want float64
		ok   bool
	}{
		{"40.7128", 40.7128, true},
		{"-74.006", -74.006, true},
		{"+180.", 180, true},
		{".5", 0.5, true},
		{"-.25", -0.25, true},
		{"0", 0, true},
		{"95", 95, true}, // in range is the caller's check
		{"4e1", 0, false},
		{"0x1p4", 0, false},
		{"NaN", 0, false},
		{"Inf", 0, false},
		{"+Inf", 0, false},
		{"1_0", 0, false},
		{"40,7", 0, false},
		{"40.7N", 0, false},
		{"40°42'46\"N", 0, false},
		{" 40.7", 0, false}, // callers trim first
		{"", 0, false},
		{".", 0, false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseDecimalDegrees(tc.in)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Fatalf("ParseDecimalDegrees(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// "-0" is 0, not negative zero, so it encodes as 0 in JSON.
func TestParseDecimalDegreesNegativeZero(t *testing.T) {
	t.Parallel()
	f, ok := ParseDecimalDegrees("-0")
	if !ok || f != 0 || 1/f < 0 {
		t.Fatalf("ParseDecimalDegrees(-0) = %v, %v; want +0", f, ok)
	}
}

// More digits than a float64 holds read as ±Inf, which every range check
// refuses.
func TestParseDecimalDegreesOverflow(t *testing.T) {
	t.Parallel()
	big := "1"
	for range 400 {
		big += "0"
	}
	if f, ok := ParseDecimalDegrees(big); !ok || !math.IsInf(f, 1) {
		t.Fatalf("ParseDecimalDegrees(1e400) = %v, %v; want +Inf, true", f, ok)
	}
}
