package servers

import (
	"errors"
	"math"
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/proto"
)

const (
	sourceNone        = aigridv1.ManagedClusterLocationSource_MANAGED_CLUSTER_LOCATION_SOURCE_NONE
	sourceAnnotations = aigridv1.ManagedClusterLocationSource_MANAGED_CLUSTER_LOCATION_SOURCE_ANNOTATIONS
	sourceCloudRegion = aigridv1.ManagedClusterLocationSource_MANAGED_CLUSTER_LOCATION_SOURCE_CLOUD_REGION
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
			name:        "no annotations in a known region: the region's approximate coordinates",
			claims:      withRegionClaim("us-east-2"),
			want:        &aigridv1.ManagedClusterLocation{Source: sourceCloudRegion, Region: "us-east-2"},
			coordinates: coords(39.96, -83),
		},
		{
			name:        "a site name without coordinates is kept beside the region's",
			annotations: map[string]string{"ai-grid/site-name": "Frankfurt cloud"},
			claims:      withRegionClaim("eu-central-1"),
			want:        &aigridv1.ManagedClusterLocation{SiteName: "Frankfurt cloud", Source: sourceCloudRegion, Region: "eu-central-1"},
			coordinates: coords(50.11, 8.68),
		},
		{
			name: "an Azure region",
			claims: []*aigridv1.ManagedClusterClaim{
				{Name: "platform.open-cluster-management.io", Value: "Azure"},
				{Name: "region.open-cluster-management.io", Value: "westeurope"},
			},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceCloudRegion, Region: "westeurope"},
			coordinates: coords(52.37, 4.9),
		},
		{
			name: "a GCP region",
			claims: []*aigridv1.ManagedClusterClaim{
				{Name: "platform.open-cluster-management.io", Value: "GCP"},
				{Name: "region.open-cluster-management.io", Value: "europe-west4"},
			},
			want:        &aigridv1.ManagedClusterLocation{Source: sourceCloudRegion, Region: "europe-west4"},
			coordinates: coords(53.44, 6.83),
		},
		{
			name:   "an unknown region: no coordinates, and that isn't a problem",
			claims: withRegionClaim("mars-north-1"),
			want:   &aigridv1.ManagedClusterLocation{Source: sourceNone, Region: "mars-north-1"},
		},
		{
			name: "a region looked up by the wrong cloud is unknown",
			claims: []*aigridv1.ManagedClusterClaim{
				{Name: "platform.open-cluster-management.io", Value: "GCP"},
				{Name: "region.open-cluster-management.io", Value: "us-east-2"},
			},
			want: &aigridv1.ManagedClusterLocation{Source: sourceNone, Region: "us-east-2"},
		},
		{
			name:   "a region without a platform claim is unknown",
			claims: []*aigridv1.ManagedClusterClaim{{Name: "region.open-cluster-management.io", Value: "us-east-2"}},
			want:   &aigridv1.ManagedClusterLocation{Source: sourceNone, Region: "us-east-2"},
		},
		{
			name:        "one coordinate is a problem, and the region isn't used",
			annotations: map[string]string{"ai-grid/latitude": "39.96"},
			claims:      withRegionClaim("us-east-2"),
			want: &aigridv1.ManagedClusterLocation{Source: sourceNone, Region: "us-east-2",
				Problem: "ai-grid/latitude is set but ai-grid/longitude is not; set both or neither"},
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
			got := managedClusterLocation(tc.annotations, tc.claims)
			if !proto.Equal(got, tc.want) {
				t.Errorf("location = %v\nwant %v", got, tc.want)
			}
			if err := protovalidate.Validate(got); err != nil {
				t.Errorf("location %v breaks its buf.validate rules: %v", got, err)
			}
		})
	}
}

// The buf.validate rules on ManagedClusterLocation state what the mapper
// guarantees: both coordinates or neither, each finite and within range.
// TestManagedClusterLocation checks every mapped location against them;
// this checks that they refuse what they should.
func TestManagedClusterLocationValidationRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		lat, lon *float64
		ok       bool
	}{
		{"neither", nil, nil, true},
		{"both at 0,0", proto.Float64(0), proto.Float64(0), true},
		{"both at the limits", proto.Float64(-90), proto.Float64(180), true},
		{"only latitude", proto.Float64(40.7), nil, false},
		{"only longitude", nil, proto.Float64(-74), false},
		{"latitude out of range", proto.Float64(90.01), proto.Float64(0), false},
		{"longitude out of range", proto.Float64(0), proto.Float64(-180.01), false},
		{"NaN", proto.Float64(math.NaN()), proto.Float64(0), false},
		{"infinite", proto.Float64(0), proto.Float64(math.Inf(1)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := protovalidate.Validate(&aigridv1.ManagedClusterLocation{Latitude: tc.lat, Longitude: tc.lon})
			var verr *protovalidate.ValidationError
			switch {
			case tc.ok && err != nil:
				t.Fatalf("Validate() error = %v, want none", err)
			case !tc.ok && !errors.As(err, &verr):
				// A compile or runtime error is not a refusal.
				t.Fatalf("Validate() error = %v, want a ValidationError", err)
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

// Every point in the table is a real position, and no region sits at 0,0,
// which would mean a missing entry. The location it gives a cluster keeps
// the buf.validate rules.
func TestCloudRegions(t *testing.T) {
	t.Parallel()
	for platform, regions := range cloudRegions {
		if len(regions) == 0 {
			t.Errorf("%s: no regions", platform)
		}
		for region, p := range regions {
			if p.lat < -maxLatitude || p.lat > maxLatitude || p.lon < -maxLongitude || p.lon > maxLongitude || (p.lat == 0 && p.lon == 0) {
				t.Errorf("%s %s = %v, want a position within range", platform, region, p)
			}
			loc := managedClusterLocation(nil, []*aigridv1.ManagedClusterClaim{
				{Name: claimPlatform, Value: platform},
				{Name: claimRegion, Value: region},
			})
			if loc.GetSource() != sourceCloudRegion {
				t.Errorf("%s %s: source = %v, want CLOUD_REGION", platform, region, loc.GetSource())
			}
			if err := protovalidate.Validate(loc); err != nil {
				t.Errorf("%s %s: location %v breaks its buf.validate rules: %v", platform, region, loc, err)
			}
		}
	}
}
