package servers

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
)

// The annotations a hub admin sets once to place a cluster on a map, the
// names the provider console has read them by (its bff/internal/api
// clusters.go, AnnotationLatitude and its neighbours). Coordinates are
// annotations, not labels, because a label value can't start with "-".
const (
	annotationLatitude  = "ai-grid/latitude"
	annotationLongitude = "ai-grid/longitude"
	annotationSiteName  = "ai-grid/site-name"
)

// claimRegion is the cloud region the cluster's agent reports, for example
// "us-east-2" (stolostron multicloud-operators-foundation, the klusterlet's
// ClusterClaims); on-premises clusters usually have none.
const claimRegion = "region.open-cluster-management.io"

// Coordinate limits in decimal degrees.
const (
	maxLatitude  = 90
	maxLongitude = 180
)

// plainDecimal is a decimal number of degrees as people write it: an
// optional sign, digits and an optional fraction, nothing else. It refuses
// what strconv.ParseFloat would also accept ("1e1", "0x1p4", "Inf", "NaN",
// "1_0") and the usual mistakes (a decimal comma, a hemisphere letter,
// degrees-minutes-seconds), so a value is read the same way by every
// client.
var plainDecimal = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)$`)

// parseDecimalDegrees reads a trimmed coordinate value. ok is false when it
// isn't a plain decimal number. A value with more digits than a float64
// holds reads as ±Inf, which every range check refuses.
func parseDecimalDegrees(v string) (degrees float64, ok bool) {
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

// managedClusterLocation reads a cluster's location from its annotations and
// claims (ManagedClusterLocation in the proto). Explicit annotations are the
// only source of coordinates; when they are wrong there are none and
// `problem` says why.
func managedClusterLocation(annotations map[string]string, claims []*aigridv1.ManagedClusterClaim) *aigridv1.ManagedClusterLocation {
	loc := &aigridv1.ManagedClusterLocation{
		SiteName: strings.TrimSpace(annotations[annotationSiteName]),
		Region:   claimValue(claims, claimRegion),
		Source:   aigridv1.ManagedClusterLocationSource_MANAGED_CLUSTER_LOCATION_SOURCE_NONE,
	}
	lat, lon, set, problem := coordinatesFromAnnotations(annotations)
	switch {
	case problem != "":
		loc.Problem = problem
	case set:
		loc.Latitude, loc.Longitude = &lat, &lon
		loc.Source = aigridv1.ManagedClusterLocationSource_MANAGED_CLUSTER_LOCATION_SOURCE_ANNOTATIONS
	}
	return loc
}

// coordinatesFromAnnotations reads ai-grid/latitude and ai-grid/longitude,
// surrounding whitespace removed. set is false when neither holds a value,
// which is not a problem. Otherwise both must be plain decimal numbers
// within range; every problem found is reported, joined with "; ".
func coordinatesFromAnnotations(annotations map[string]string) (lat, lon float64, set bool, problem string) {
	rawLat := strings.TrimSpace(annotations[annotationLatitude])
	rawLon := strings.TrimSpace(annotations[annotationLongitude])
	if rawLat == "" && rawLon == "" {
		return 0, 0, false, ""
	}

	var problems []string
	switch {
	case rawLat == "":
		problems = append(problems, fmt.Sprintf("%s is set but %s is not; set both or neither", annotationLongitude, annotationLatitude))
	case rawLon == "":
		problems = append(problems, fmt.Sprintf("%s is set but %s is not; set both or neither", annotationLatitude, annotationLongitude))
	}

	lat, latNumber := parseDecimalDegrees(rawLat)
	lon, lonNumber := parseDecimalDegrees(rawLon)
	for _, c := range []struct {
		key, raw string
		number   bool
	}{{annotationLatitude, rawLat, latNumber}, {annotationLongitude, rawLon, lonNumber}} {
		if c.raw != "" && !c.number {
			problems = append(problems, fmt.Sprintf("%s %s is not a decimal number of degrees, such as 40.7128 or -74.006", c.key, quoteValue(c.raw)))
		}
	}

	latInRange := latNumber && math.Abs(lat) <= maxLatitude
	lonInRange := lonNumber && math.Abs(lon) <= maxLongitude
	if latNumber && lonNumber && !latInRange && math.Abs(lat) <= maxLongitude && math.Abs(lon) <= maxLatitude {
		problems = append(problems, fmt.Sprintf("%s %s and %s %s look swapped: latitude must be within -%d..%d and longitude within -%d..%d",
			annotationLatitude, quoteValue(rawLat), annotationLongitude, quoteValue(rawLon), maxLatitude, maxLatitude, maxLongitude, maxLongitude))
	} else {
		if latNumber && !latInRange {
			problems = append(problems, fmt.Sprintf("%s %s is outside -%d..%d", annotationLatitude, quoteValue(rawLat), maxLatitude, maxLatitude))
		}
		if lonNumber && !lonInRange {
			problems = append(problems, fmt.Sprintf("%s %s is outside -%d..%d", annotationLongitude, quoteValue(rawLon), maxLongitude, maxLongitude))
		}
	}

	if len(problems) > 0 {
		return 0, 0, false, strings.Join(problems, "; ")
	}
	return lat, lon, true, ""
}

// maxQuotedValue bounds how much of an annotation value a problem quotes:
// enough to recognise it, never a whole pasted document.
const maxQuotedValue = 40

// quoteValue quotes an annotation value for a problem message, Go-escaped so
// that control characters can't reach a log line or a page, and cut after
// maxQuotedValue characters.
func quoteValue(v string) string {
	if r := []rune(v); len(r) > maxQuotedValue {
		return strconv.Quote(string(r[:maxQuotedValue])) + "..."
	}
	return strconv.Quote(v)
}

// claimValue is the value of the first claim with the name, empty without one.
func claimValue(claims []*aigridv1.ManagedClusterClaim, name string) string {
	for _, c := range claims {
		if c.GetName() == name {
			return c.GetValue()
		}
	}
	return ""
}
