package servers

import (
	"strings"
	"testing"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/proto"
)

const (
	sourceNone        = aigridv1.ManagedClusterLocationSource_MANAGED_CLUSTER_LOCATION_SOURCE_NONE
	sourceAnnotations = aigridv1.ManagedClusterLocationSource_MANAGED_CLUSTER_LOCATION_SOURCE_ANNOTATIONS
)

func coords(lat, lon float64) func(*aigridv1.ManagedClusterLocation) {
	return func(l *aigridv1.ManagedClusterLocation) {
		l.Latitude, l.Longitude = proto.Float64(lat), proto.Float64(lon)
	}
}

// withRegionClaim gives a cluster the claims an AWS agent reports.
func withRegionClaim(region string) []*aigridv1.ManagedClusterClaim {
	return []*aigridv1.ManagedClusterClaim{
		{Name: "platform.open-cluster-management.io", Value: "AWS"},
		{Name: "region.open-cluster-management.io", Value: region},
	}
}

func TestManagedClusterLocation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		annotations map[string]string
		claims      []*aigridv1.ManagedClusterClaim
		want        *aigridv1.ManagedClusterLocation
		coordinates func(*aigridv1.ManagedClusterLocation)
	}{
		{
			name: "on-premises, nothing set: no coordinates and no problem",
			want: &aigridv1.ManagedClusterLocation{Source: sourceNone},
		},
		{
			name:        "both annotations and a site name",
			annotations: map[string]string{"ai-grid/latitude": "40.7128", "ai-grid/longitude": "-74.0060", "ai-grid/site-name": "New York DC1"},
			want:        &aigridv1.ManagedClusterLocation{SiteName: "New York DC1", Source: sourceAnnotations},
			coordinates: coords(40.7128, -74.006),
		},
		{
			name:        "surrounding whitespace is trimmed",
			annotations: map[string]string{"ai-grid/latitude": " 40.7128\n", "ai-grid/longitude": "\t-74.0060 ", "ai-grid/site-name": "  New York DC1 "},
			want:        &aigridv1.ManagedClusterLocation{SiteName: "New York DC1", Source: sourceAnnotations},
			coordinates: coords(40.7128, -74.006),
		},
		{
			name:        "0,0 is a valid location",
			annotations: map[string]string{"ai-grid/latitude": "0", "ai-grid/longitude": "0.0"},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceAnnotations},
			coordinates: coords(0, 0),
		},
		{
			name:        "the limits are valid, and a sign and a bare fraction are plain decimals",
			annotations: map[string]string{"ai-grid/latitude": "-90", "ai-grid/longitude": "+180."},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceAnnotations},
			coordinates: coords(-90, 180),
		},
		{
			name:        "a bare fraction",
			annotations: map[string]string{"ai-grid/latitude": ".5", "ai-grid/longitude": "-.25"},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceAnnotations},
			coordinates: coords(0.5, -0.25),
		},
		{
			name:        "only latitude",
			annotations: map[string]string{"ai-grid/latitude": "40.7128"},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceNone, Problem: "ai-grid/latitude is set but ai-grid/longitude is not; set both or neither"},
		},
		{
			name:        "only longitude; a blank latitude is not set",
			annotations: map[string]string{"ai-grid/latitude": "  ", "ai-grid/longitude": "-74.006"},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceNone, Problem: "ai-grid/longitude is set but ai-grid/latitude is not; set both or neither"},
		},
		{
			name:        "a decimal comma",
			annotations: map[string]string{"ai-grid/latitude": "40,7", "ai-grid/longitude": "-74.006"},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceNone, Problem: `ai-grid/latitude "40,7" is not a decimal number of degrees, such as 40.7128 or -74.006`},
		},
		{
			name:        "a hemisphere letter",
			annotations: map[string]string{"ai-grid/latitude": "40.7N", "ai-grid/longitude": "74.0W"},
			want: &aigridv1.ManagedClusterLocation{Source: sourceNone, Problem: `ai-grid/latitude "40.7N" is not a decimal number of degrees, such as 40.7128 or -74.006; ` +
				`ai-grid/longitude "74.0W" is not a decimal number of degrees, such as 40.7128 or -74.006`},
		},
		{
			name:        "degrees, minutes and seconds",
			annotations: map[string]string{"ai-grid/latitude": `40°42'46"N`, "ai-grid/longitude": "-74.006"},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceNone, Problem: `ai-grid/latitude "40°42'46\"N" is not a decimal number of degrees, such as 40.7128 or -74.006`},
		},
		{
			name:        "NaN and Inf, which ParseFloat accepts",
			annotations: map[string]string{"ai-grid/latitude": "NaN", "ai-grid/longitude": "Inf"},
			want: &aigridv1.ManagedClusterLocation{Source: sourceNone, Problem: `ai-grid/latitude "NaN" is not a decimal number of degrees, such as 40.7128 or -74.006; ` +
				`ai-grid/longitude "Inf" is not a decimal number of degrees, such as 40.7128 or -74.006`},
		},
		{
			name:        "exponent and hex notation, which ParseFloat accepts",
			annotations: map[string]string{"ai-grid/latitude": "4e1", "ai-grid/longitude": "0x1p4"},
			want: &aigridv1.ManagedClusterLocation{Source: sourceNone, Problem: `ai-grid/latitude "4e1" is not a decimal number of degrees, such as 40.7128 or -74.006; ` +
				`ai-grid/longitude "0x1p4" is not a decimal number of degrees, such as 40.7128 or -74.006`},
		},
		{
			name:        "a Unicode minus sign",
			annotations: map[string]string{"ai-grid/latitude": "−33.8688", "ai-grid/longitude": "151.2093"},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceNone, Problem: `ai-grid/latitude "−33.8688" is not a decimal number of degrees, such as 40.7128 or -74.006`},
		},
		{
			name:        "latitude out of range, swapping doesn't help",
			annotations: map[string]string{"ai-grid/latitude": "95", "ai-grid/longitude": "120"},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceNone, Problem: `ai-grid/latitude "95" is outside -90..90`},
		},
		{
			name:        "longitude out of range",
			annotations: map[string]string{"ai-grid/latitude": "10", "ai-grid/longitude": "-180.5"},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceNone, Problem: `ai-grid/longitude "-180.5" is outside -180..180`},
		},
		{
			name:        "both out of range",
			annotations: map[string]string{"ai-grid/latitude": "200", "ai-grid/longitude": "200"},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceNone, Problem: `ai-grid/latitude "200" is outside -90..90; ai-grid/longitude "200" is outside -180..180`},
		},
		{
			name:        "swapped",
			annotations: map[string]string{"ai-grid/latitude": "151.2093", "ai-grid/longitude": "-33.8688"},
			want: &aigridv1.ManagedClusterLocation{Source: sourceNone, Problem: `ai-grid/latitude "151.2093" and ai-grid/longitude "-33.8688" look swapped: ` +
				`latitude must be within -90..90 and longitude within -180..180`},
		},
		{
			name:        "more digits than a float64 holds is out of range",
			annotations: map[string]string{"ai-grid/latitude": "1" + strings.Repeat("0", 400), "ai-grid/longitude": "0"},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceNone, Problem: `ai-grid/latitude "` + "1" + strings.Repeat("0", 39) + `"... is outside -90..90`},
		},
		{
			name:        "a site name without coordinates is kept",
			annotations: map[string]string{"ai-grid/site-name": "Edge closet 3"},
			want:        &aigridv1.ManagedClusterLocation{SiteName: "Edge closet 3", Source: sourceNone},
		},
		{
			name:        "a site name is kept beside a problem",
			annotations: map[string]string{"ai-grid/site-name": "Sydney DC1", "ai-grid/latitude": "151.2093", "ai-grid/longitude": "-33.8688"},
			want: &aigridv1.ManagedClusterLocation{SiteName: "Sydney DC1", Source: sourceNone, Problem: `ai-grid/latitude "151.2093" and ai-grid/longitude "-33.8688" look swapped: ` +
				`latitude must be within -90..90 and longitude within -180..180`},
		},
		{
			name:   "the region claim is reported",
			claims: withRegionClaim("us-east-2"),
			want:   &aigridv1.ManagedClusterLocation{Source: sourceNone, Region: "us-east-2"},
		},
		{
			name:        "annotations win, and the region is still reported",
			annotations: map[string]string{"ai-grid/latitude": "39.96", "ai-grid/longitude": "-83.00"},
			claims:      withRegionClaim("us-east-2"),
			want:        &aigridv1.ManagedClusterLocation{Source: sourceAnnotations, Region: "us-east-2"},
			coordinates: coords(39.96, -83),
		},
		{
			name:        "invalid annotations: a problem, no coordinates, the region still reported",
			annotations: map[string]string{"ai-grid/latitude": "40,7", "ai-grid/longitude": "-74.006"},
			claims:      withRegionClaim("us-east-2"),
			want: &aigridv1.ManagedClusterLocation{Source: sourceNone, Region: "us-east-2",
				Problem: `ai-grid/latitude "40,7" is not a decimal number of degrees, such as 40.7128 or -74.006`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.coordinates != nil {
				tc.coordinates(tc.want)
			}
			if got := managedClusterLocation(tc.annotations, tc.claims); !proto.Equal(got, tc.want) {
				t.Errorf("location = %v\nwant %v", got, tc.want)
			}
		})
	}
}

// A control character in a value is escaped in the problem, so it can't
// reach a log line or a page as is.
func TestManagedClusterLocationProblemEscapes(t *testing.T) {
	t.Parallel()
	got := managedClusterLocation(map[string]string{"ai-grid/latitude": "4\x1b[2J0", "ai-grid/longitude": "1"}, nil)
	if strings.ContainsRune(got.GetProblem(), '\x1b') || !strings.Contains(got.GetProblem(), `"4\x1b[2J0"`) {
		t.Fatalf("problem = %q, want the escape sequence quoted", got.GetProblem())
	}
}

// "-0" is 0, not negative zero, so it encodes as 0 in JSON.
func TestParseDecimalDegreesNegativeZero(t *testing.T) {
	t.Parallel()
	f, ok := parseDecimalDegrees("-0")
	if !ok || f != 0 || 1/f < 0 {
		t.Fatalf("parseDecimalDegrees(-0) = %v, %v; want +0", f, ok)
	}
}

// The mapper always sets the location, whatever the cluster carries.
func TestManagedClusterFromUnstructuredLocation(t *testing.T) {
	t.Parallel()
	object := managedClusterUnstructured("site-a", nil, map[string]string{"ai-grid/latitude": "1", "ai-grid/longitude": "2", "ai-grid/site-name": "A"}, true, nil)
	object.Object["status"] = map[string]interface{}{"clusterClaims": []interface{}{
		map[string]interface{}{"name": "region.open-cluster-management.io", "value": "eu-west-1"},
	}}
	want := &aigridv1.ManagedClusterLocation{SiteName: "A", Latitude: proto.Float64(1), Longitude: proto.Float64(2), Source: sourceAnnotations, Region: "eu-west-1"}
	if got := managedClusterFromUnstructured(*object).GetStatus().GetLocation(); !proto.Equal(got, want) {
		t.Fatalf("location = %v, want %v", got, want)
	}
	if got := managedClusterFromUnstructured(*managedClusterUnstructured("new", nil, nil, true, nil)).GetStatus().GetLocation(); got.GetSource() != sourceNone {
		t.Fatalf("location of a new cluster = %v, want source NONE", got)
	}
}
