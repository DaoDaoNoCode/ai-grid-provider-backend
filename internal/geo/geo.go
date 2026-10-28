// Package geo reads the coordinates a hub admin writes in a ManagedCluster's
// ai-grid/latitude and ai-grid/longitude annotations, so the real server
// (status.location and Clusters/Update) and the draft fake read a value the
// same way.
package geo

import (
	"math"
	"regexp"
	"strconv"
)

// plainDecimal is a decimal number of degrees as people write it: an
// optional sign, digits and an optional fraction, nothing else. It refuses
// what strconv.ParseFloat would also accept ("1e1", "0x1p4", "Inf", "NaN",
// "1_0") and the usual mistakes (a decimal comma, a hemisphere letter,
// degrees-minutes-seconds), so a value is read the same way by every
// client.
var plainDecimal = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)$`)

// ParseDecimalDegrees reads a trimmed coordinate value. ok is false when it
// isn't a plain decimal number. A value with more digits than a float64
// holds reads as ±Inf, which every range check refuses.
func ParseDecimalDegrees(v string) (degrees float64, ok bool) {
	if !plainDecimal.MatchString(v) {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil && !math.IsInf(f, 0) {
		return 0, false
	}
	if f == 0 {
		f = 0 // "-0" is 0
	}
	return f, true
}
