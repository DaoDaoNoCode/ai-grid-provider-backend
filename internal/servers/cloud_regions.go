package servers

// claimPlatform is the cloud the cluster's agent reports: "AWS", "Azure" or
// "GCP" for the clouds below (stolostron multicloud-operators-foundation,
// the klusterlet's ClusterClaims; the hub's `cloud` label, "Amazon",
// "Azure" or "Google", is derived from it).
const claimPlatform = "platform.open-cluster-management.io"

// regionPoint is the approximate position of a cloud region, in decimal
// degrees.
type regionPoint struct{ lat, lon float64 }

// cloudRegions is where each public cloud region roughly is, keyed by the
// platform claim and then the region claim. A cluster without coordinate
// annotations is placed here, with source CLOUD_REGION, so a map can show
// it and say the position is approximate.
//
// The providers don't publish data center positions. Each point is the
// city or area the provider names for the region, rounded to two decimals
// (about a kilometre), which is plenty for a pin on a world map:
//   - AWS: the region names in "AWS Regions"
//     (https://docs.aws.amazon.com/global-infrastructure/latest/regions/aws-regions.html),
//     for example us-east-2 "US East (Ohio)" at Columbus;
//   - Azure: the latitude and longitude Azure itself gives each region in
//     `az account list-locations` (metadata.latitude, metadata.longitude);
//   - GCP: the locations in "Regions and zones"
//     (https://cloud.google.com/compute/docs/regions-zones),
//     for example europe-west4 "Eemshaven, Netherlands".
//
// GovCloud, sovereign and other restricted regions are left out. A region
// that isn't listed (one of those, or a newer one) gives no coordinates,
// and that isn't a problem; add it here when one shows up on a hub.
var cloudRegions = map[string]map[string]regionPoint{
	"AWS": {
		"us-east-1":      {39.04, -77.49},  // US East (N. Virginia): Ashburn
		"us-east-2":      {39.96, -83.00},  // US East (Ohio): Columbus
		"us-west-1":      {37.34, -121.89}, // US West (N. California): San Jose
		"us-west-2":      {45.84, -119.70}, // US West (Oregon): Boardman
		"ca-central-1":   {45.50, -73.57},  // Canada (Central): Montreal
		"ca-west-1":      {51.05, -114.07}, // Canada West (Calgary)
		"mx-central-1":   {20.59, -100.39}, // Mexico (Central): Queretaro
		"sa-east-1":      {-23.55, -46.63}, // South America (Sao Paulo)
		"eu-west-1":      {53.35, -6.26},   // Europe (Ireland): Dublin
		"eu-west-2":      {51.51, -0.13},   // Europe (London)
		"eu-west-3":      {48.86, 2.35},    // Europe (Paris)
		"eu-central-1":   {50.11, 8.68},    // Europe (Frankfurt)
		"eu-central-2":   {47.37, 8.54},    // Europe (Zurich)
		"eu-north-1":     {59.33, 18.07},   // Europe (Stockholm)
		"eu-south-1":     {45.46, 9.19},    // Europe (Milan)
		"eu-south-2":     {41.65, -0.88},   // Europe (Spain): Aragon, Zaragoza
		"af-south-1":     {-33.92, 18.42},  // Africa (Cape Town)
		"me-south-1":     {26.07, 50.56},   // Middle East (Bahrain)
		"me-central-1":   {24.45, 54.38},   // Middle East (UAE): Abu Dhabi
		"il-central-1":   {32.09, 34.78},   // Israel (Tel Aviv)
		"ap-east-1":      {22.32, 114.17},  // Asia Pacific (Hong Kong)
		"ap-east-2":      {25.03, 121.57},  // Asia Pacific (Taipei)
		"ap-south-1":     {19.08, 72.88},   // Asia Pacific (Mumbai)
		"ap-south-2":     {17.39, 78.49},   // Asia Pacific (Hyderabad)
		"ap-northeast-1": {35.68, 139.69},  // Asia Pacific (Tokyo)
		"ap-northeast-2": {37.57, 126.98},  // Asia Pacific (Seoul)
		"ap-northeast-3": {34.69, 135.50},  // Asia Pacific (Osaka)
		"ap-southeast-1": {1.35, 103.82},   // Asia Pacific (Singapore)
		"ap-southeast-2": {-33.87, 151.21}, // Asia Pacific (Sydney)
		"ap-southeast-3": {-6.21, 106.85},  // Asia Pacific (Jakarta)
		"ap-southeast-4": {-37.81, 144.96}, // Asia Pacific (Melbourne)
		"ap-southeast-5": {3.14, 101.69},   // Asia Pacific (Malaysia): Kuala Lumpur
		"ap-southeast-7": {13.76, 100.50},  // Asia Pacific (Thailand): Bangkok
	},
	"Azure": {
		"eastus":             {37.37, -79.82},  // East US (Virginia)
		"eastus2":            {36.67, -78.39},  // East US 2 (Virginia)
		"centralus":          {41.59, -93.62},  // Central US (Iowa)
		"northcentralus":     {41.88, -87.63},  // North Central US (Illinois)
		"southcentralus":     {29.42, -98.49},  // South Central US (Texas)
		"westcentralus":      {40.89, -110.23}, // West Central US (Wyoming)
		"westus":             {37.78, -122.42}, // West US (California)
		"westus2":            {47.23, -119.85}, // West US 2 (Washington)
		"westus3":            {33.45, -112.07}, // West US 3 (Phoenix)
		"canadacentral":      {43.65, -79.38},  // Canada Central (Toronto)
		"canadaeast":         {46.82, -71.22},  // Canada East (Quebec)
		"brazilsouth":        {-23.55, -46.63}, // Brazil South (Sao Paulo State)
		"northeurope":        {53.35, -6.26},   // North Europe (Ireland)
		"westeurope":         {52.37, 4.90},    // West Europe (Netherlands)
		"uksouth":            {50.94, -0.80},   // UK South (London)
		"ukwest":             {53.43, -3.08},   // UK West (Cardiff)
		"francecentral":      {46.38, 2.37},    // France Central (Paris)
		"germanywestcentral": {50.11, 8.68},    // Germany West Central (Frankfurt)
		"switzerlandnorth":   {47.45, 8.56},    // Switzerland North (Zurich)
		"norwayeast":         {59.91, 10.75},   // Norway East (Oslo)
		"swedencentral":      {60.67, 17.14},   // Sweden Central (Gavle)
		"italynorth":         {45.47, 9.18},    // Italy North (Milan)
		"polandcentral":      {52.23, 21.02},   // Poland Central (Warsaw)
		"spaincentral":       {40.42, -3.70},   // Spain Central (Madrid)
		"eastasia":           {22.27, 114.19},  // East Asia (Hong Kong)
		"southeastasia":      {1.28, 103.83},   // Southeast Asia (Singapore)
		"japaneast":          {35.68, 139.77},  // Japan East (Tokyo, Saitama)
		"japanwest":          {34.69, 135.50},  // Japan West (Osaka)
		"koreacentral":       {37.57, 126.98},  // Korea Central (Seoul)
		"koreasouth":         {35.18, 129.08},  // Korea South (Busan)
		"australiaeast":      {-33.86, 151.21}, // Australia East (New South Wales)
		"australiasoutheast": {-37.81, 144.96}, // Australia Southeast (Victoria)
		"centralindia":       {18.58, 73.92},   // Central India (Pune)
		"southindia":         {12.98, 80.16},   // South India (Chennai)
		"westindia":          {19.09, 72.87},   // West India (Mumbai)
		"uaenorth":           {25.27, 55.32},   // UAE North (Dubai)
		"qatarcentral":       {25.55, 51.18},   // Qatar Central (Doha)
		"southafricanorth":   {-25.73, 28.22},  // South Africa North (Johannesburg)
	},
	"GCP": {
		"us-central1":             {41.26, -95.86},  // Council Bluffs, Iowa
		"us-east1":                {33.20, -80.01},  // Moncks Corner, South Carolina
		"us-east4":                {39.04, -77.49},  // Ashburn, Virginia
		"us-east5":                {39.96, -83.00},  // Columbus, Ohio
		"us-south1":               {32.78, -96.80},  // Dallas, Texas
		"us-west1":                {45.60, -121.18}, // The Dalles, Oregon
		"us-west2":                {34.05, -118.24}, // Los Angeles, California
		"us-west3":                {40.76, -111.89}, // Salt Lake City, Utah
		"us-west4":                {36.17, -115.14}, // Las Vegas, Nevada
		"northamerica-northeast1": {45.50, -73.57},  // Montreal, Quebec
		"northamerica-northeast2": {43.65, -79.38},  // Toronto, Ontario
		"northamerica-south1":     {20.59, -100.39}, // Queretaro, Mexico
		"southamerica-east1":      {-23.53, -46.79}, // Osasco, Sao Paulo
		"southamerica-west1":      {-33.45, -70.67}, // Santiago, Chile
		"europe-west1":            {50.45, 3.82},    // St. Ghislain, Belgium
		"europe-west2":            {51.51, -0.13},   // London, England
		"europe-west3":            {50.11, 8.68},    // Frankfurt, Germany
		"europe-west4":            {53.44, 6.83},    // Eemshaven, Netherlands
		"europe-west6":            {47.37, 8.54},    // Zurich, Switzerland
		"europe-west8":            {45.46, 9.19},    // Milan, Italy
		"europe-west9":            {48.86, 2.35},    // Paris, France
		"europe-west10":           {52.52, 13.40},   // Berlin, Germany
		"europe-west12":           {45.07, 7.69},    // Turin, Italy
		"europe-north1":           {60.57, 27.20},   // Hamina, Finland
		"europe-north2":           {59.33, 18.07},   // Stockholm, Sweden
		"europe-central2":         {52.23, 21.01},   // Warsaw, Poland
		"europe-southwest1":       {40.42, -3.70},   // Madrid, Spain
		"asia-east1":              {24.05, 120.52},  // Changhua County, Taiwan
		"asia-east2":              {22.32, 114.17},  // Hong Kong
		"asia-northeast1":         {35.68, 139.69},  // Tokyo, Japan
		"asia-northeast2":         {34.69, 135.50},  // Osaka, Japan
		"asia-northeast3":         {37.57, 126.98},  // Seoul, South Korea
		"asia-south1":             {19.08, 72.88},   // Mumbai, India
		"asia-south2":             {28.61, 77.21},   // Delhi, India
		"asia-southeast1":         {1.34, 103.71},   // Jurong West, Singapore
		"asia-southeast2":         {-6.21, 106.85},  // Jakarta, Indonesia
		"australia-southeast1":    {-33.87, 151.21}, // Sydney, Australia
		"australia-southeast2":    {-37.81, 144.96}, // Melbourne, Australia
		"me-west1":                {32.09, 34.78},   // Tel Aviv, Israel
		"me-central1":             {25.29, 51.53},   // Doha, Qatar
		"me-central2":             {26.43, 50.10},   // Dammam, Saudi Arabia
		"africa-south1":           {-26.20, 28.05},  // Johannesburg, South Africa
	},
}

// cloudRegionPoint looks a cluster's region claim up in cloudRegions, by its
// platform claim. ok is false for an unknown platform or region.
func cloudRegionPoint(platform, region string) (regionPoint, bool) {
	p, ok := cloudRegions[platform][region]
	return p, ok
}
