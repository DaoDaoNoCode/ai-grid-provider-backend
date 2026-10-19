package draft

// Ported from krisv/ai-grid-cloud-provider-poc shared/grid-monitoring.mjs at 62319b8.
//
// The PoC's backend answers /api/grid/overview with demoMetrics when Thanos is
// not configured, which is the state the fake mirrors. The port is exact: the
// same 32-bit hash noise and the same arithmetic in the same order, pinned to
// the PoC's output by internal/fakeinfra/draft/testdata/demo-metrics.json (TestDemoMetricsMatchThePoC).
// Every product that is added to something is wrapped in float64(): Go may fuse
// x*y+z into one rounding, JavaScript never does, and the conversion forbids
// the fusion. The console's frontend/src/services/demoMetrics.ts is the same
// port for the browser mock.

import (
	"math"
	"math/big"
	"unicode/utf16"

	"google.golang.org/protobuf/proto"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// metricDefinition is one entry of the PoC's metricDefinitions
// (grid-monitoring.mjs:5-10) without the Thanos query, which the fake has no use for.
type metricDefinition struct {
	key, label, unit string
}

var metricDefinitions = []metricDefinition{
	{"requests", "Gateway request rate", "req/s"},
	{"errors", "Gateway error rate", "req/s"},
	{"latency", "Gateway p95 latency", "ms"},
	{"gpu", "GPU utilization", "%"},
}

// The PoC's no-data messages (grid-monitoring.mjs:45).
const (
	msgNoReadyClusters = "No available clusters report simulated telemetry for this selection."
	msgNoClusters      = "No enrolled clusters match this selection."
)

// demoHash is the PoC's hash (grid-monitoring.mjs:17): n*31+c over the code
// points, kept to 32 bits by >>>0. c is charCodeAt(0) of the code point, its
// first UTF-16 unit, which is the high surrogate for one outside the BMP.
func demoHash(name string) uint32 {
	n := uint32(7)
	for _, r := range name {
		c := uint32(r)
		if r > 0xFFFF {
			hi, _ := utf16.EncodeRune(r)
			c = uint32(hi)
		}
		n = n*31 + c
	}
	return n
}

// demoRandom is the PoC's random (grid-monitoring.mjs:18-22), a 32-bit integer
// hash of seed and bucket scaled to [0, 1). Math.imul and >>> are uint32
// arithmetic; the seed+k the callers pass wraps exactly as ToInt32 does.
func demoRandom(seed uint32, bucket int64) float64 {
	n := seed ^ (uint32(bucket) * 0x45d9f3b)
	n = (n ^ (n >> 16)) * 0x7feb352d
	n = (n ^ (n >> 15)) * 0x846ca68b
	return float64(n^(n>>16)) / 4294967296
}

// floorDiv is JavaScript's Math.floor(a/b) for the integer timestamps here.
func floorDiv(a, b int64) int64 {
	return int64(math.Floor(float64(a) / float64(b)))
}

// demoTraffic is the PoC's traffic (grid-monitoring.mjs:23-29): a baseline that
// drifts every 30 minutes, noise every 30 seconds and occasional spikes.
func demoTraffic(timestamp int64, seed uint32) float64 {
	bucket := floorDiv(timestamp, 30)
	baseline := 0.6 + float64((demoRandom(seed+3, floorDiv(timestamp, 1800))-0.5)*0.4)
	offset := float64((demoRandom(seed, bucket) - 0.5) * 0.9)
	spike := 0.0
	if demoRandom(seed+2, bucket) > 0.91 {
		spike = 0.5 + float64(demoRandom(seed+4, bucket)*1.2)
	}
	return math.Max(0.04, baseline+offset+spike)
}

// demoSample is one cluster's values at one timestamp, or the fleet's row.
type demoSample struct {
	requests, gpu, errors, latency float64
}

func (s demoSample) value(key string) float64 {
	switch key {
	case "requests":
		return s.requests
	case "errors":
		return s.errors
	case "latency":
		return s.latency
	default:
		return s.gpu
	}
}

// demoMetrics is the PoC's demoMetrics (grid-monitoring.mjs:14-47). Only
// clusters that are Ready (ManagedClusterConditionAvailable True) report; the
// fleet's requests and errors are sums, GPU the mean, latency the
// request-weighted mean. start, end and step are Unix seconds.
func demoMetrics(clusters []*aigriddraftv1.ManagedCluster, start, end, step int64) []*aigriddraftv1.Metric {
	// grid-monitoring.mjs:15
	var active []*aigriddraftv1.ManagedCluster
	for _, c := range clusters {
		if isAvailable(c) {
			active = append(active, c)
		}
	}

	// grid-monitoring.mjs:30-46
	var series []struct {
		timestamp int64
		row       demoSample
	}
	if len(active) > 0 {
		for i := int64(0); i < floorDiv(end-start, step)+1; i++ {
			timestamp := start + i*step
			values := make([]demoSample, 0, len(active))
			for _, c := range active {
				// grid-monitoring.mjs:33-39
				seed := demoHash(c.GetName())
				capacity := float64(90 + seed%120)
				load := demoTraffic(timestamp, seed)
				recent := float64(load*0.8) + float64(demoTraffic(timestamp-30, seed)*0.15) + float64(demoTraffic(timestamp-60, seed)*0.05)
				requests := capacity * load
				gpu := math.Min(98, 8+float64(recent*66)+float64((demoRandom(seed+1, floorDiv(timestamp, 30))-0.5)*4))
				pressure := math.Max(0, recent-0.85)
				errors := requests * (0.001 + float64(pressure*0.018) + float64(demoRandom(seed+2, floorDiv(timestamp, 30))*0.0015))
				latency := 65 + float64(recent*95) + float64(pressure*220)
				values = append(values, demoSample{requests: requests, gpu: gpu, errors: errors, latency: latency})
			}
			// grid-monitoring.mjs:41-44
			var requests, errors, gpu, weighted float64
			for _, v := range values {
				requests += v.requests
			}
			for _, v := range values {
				errors += v.errors
			}
			for _, v := range values {
				gpu += v.gpu
			}
			for _, v := range values {
				weighted += float64(v.latency * v.requests)
			}
			series = append(series, struct {
				timestamp int64
				row       demoSample
			}{timestamp, demoSample{
				requests: requests,
				errors:   errors,
				gpu:      gpu / math.Max(1, float64(len(values))),
				latency:  weighted / math.Max(1, requests),
			}})
		}
	}

	// grid-monitoring.mjs:46-47
	out := make([]*aigriddraftv1.Metric, 0, len(metricDefinitions))
	for _, d := range metricDefinitions {
		m := &aigriddraftv1.Metric{Key: d.key, Label: d.label, Unit: d.unit, Status: aigriddraftv1.MetricStatus_METRIC_STATUS_OK, Points: []*aigriddraftv1.MetricPoint{}}
		if len(active) == 0 {
			m.Status = aigriddraftv1.MetricStatus_METRIC_STATUS_NO_DATA
			if len(clusters) > 0 {
				m.Message = proto.String(msgNoReadyClusters)
			} else {
				m.Message = proto.String(msgNoClusters)
			}
		}
		for _, s := range series {
			m.Points = append(m.Points, &aigriddraftv1.MetricPoint{Timestamp: s.timestamp, Value: toFixed3(s.row.value(d.key))})
		}
		out = append(out, m)
	}
	return out
}

// toFixed3 is JavaScript's Number(x.toFixed(3)): the integer n closest to
// x*1000, computed exactly, the larger magnitude on a tie, then n/1000.
func toFixed3(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	sign := 1.0
	if x < 0 {
		sign, x = -1, -x
	}
	v := new(big.Float).SetPrec(128).SetFloat64(x)
	v.Mul(v, big.NewFloat(1000))
	v.Add(v, big.NewFloat(0.5))
	n, _ := v.Int(nil) // truncates toward zero, the floor for x >= 0
	f, _ := new(big.Float).SetInt(n).Float64()
	return sign * (f / 1000)
}

// isAvailable reports whether ManagedClusterConditionAvailable is True, the
// PoC's Ready.
func isAvailable(c *aigriddraftv1.ManagedCluster) bool {
	for _, cond := range c.GetStatus().GetConditions() {
		if cond.GetType() == CondAvailable {
			return cond.GetStatus() == aigriddraftv1.ConditionStatus_CONDITION_STATUS_TRUE
		}
	}
	return false
}
