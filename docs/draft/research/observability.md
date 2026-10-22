# Spike: observability for the grid overview and monitoring pages

Read-only research, 2026-10-15. Sources: the AI Grid dev hub (queried with `oc get/explain` and the
Prometheus HTTP API of both Thanos endpoints; everything below is anonymised), the POC repo
`krisv/ai-grid-cloud-provider-poc` (clone in `scratchpad/poc-obs`), the Praxis AI filter docs in
`scratchpad/praxis-ai-docs/`, this repo's `ai-grid-provider-console/docs/plan/bff-api.md` and `ai-grid-provider-console/docs/plan/work-breakdown.md`, and
the vendor documentation listed at the end. Sample values in this document are fictional.

## Summary

1. The hub has two query endpoints: OpenShift's `thanos-querier` (openshift-monitoring; platform + user-workload Prometheus, single cluster, ~4000 metric names) and ACM's fleet Thanos behind `rbac-query-proxy` (open-cluster-management-observability; ~330 metric names, every series carries `cluster=<ManagedCluster name>` and `clusterID`). The fleet endpoint is the one `Grid/GetOverview` must use; the `cluster` label is exactly the ManagedCluster name, as the POC assumed.
2. ACM observability is installed with the MCOA "user workloads" capability. The path for AI metrics is: RHOAI OpenTelemetry collector `data-science-collector` scrapes every ServiceMonitor/PodMonitor labelled `monitoring.opendatahub.io/scrape: "true"` (30 s) and a DCGM exporter job, exposes them on a Prometheus endpoint, and the ACM `user-workload-metrics-collector` PrometheusAgent scrapes that endpoint every **300 s** (the agent's own `spec.scrapeInterval: 300s`; the `ScrapeConfig`s `rhai-metrics-sourced` (relabel `scrape_source="rhai-metrics-sourced"`) and `rhai-metrics` (no label) set no interval and are **both** selected by the agent, so twin series exist, see below) and remote-writes to Observatorium. Fleet samples therefore arrive at a 5-minute cadence; `rate()` windows must be at least 10-15 minutes.
3. vLLM metrics are real and already fleet-visible: `vllm:request_success_total`, `vllm:e2e_request_latency_seconds_*`, `vllm:prompt_tokens_total`, `vllm:generation_tokens_total`, `vllm:kv_cache_usage_perc`, `vllm:num_requests_running|waiting`, `vllm:time_to_first_token_seconds_*`, all with `model_name`, `namespace`, `pod`, `llm_isvc_name`, and (fleet) `cluster`. Each also exists as a `kserve_vllm:*` duplicate (KServe's second PodMonitor renames). Per-model usage can be built from these today.
4. None of the POC's assumed names exist: `ai_gateway_requests_total` and `ai_gateway_request_duration_seconds_bucket` are absent everywhere; `DCGM_FI_DEV_GPU_UTIL` is absent on the hub only because the hub has no GPU nodes. The RHOAI collector is pre-configured to forward nine DCGM metrics from `nvidia-gpu-operator` and **divides `DCGM_FI_DEV_GPU_UTIL` by 100** (fleet value is a 0..1 ratio, not a percentage).
5. Gateway request/error/latency metrics are wired but empty: a PodMonitor on the `openshift-ai-inference` Gateway keeps `istio_requests_total`, `istio_request_duration_milliseconds_*`, `istio_request/response_bytes_*` and adds `gateway_name`, `llm_isvc_gateway="true"`; the scrape returns ~390 samples but 0 survive the keep-filter (no traffic has produced Istio request stats yet). The MaaS gateway (`maas-default-gateway`) has no PodMonitor at all.
6. Kuadrant is installed (Limitador, Authorino, TokenRateLimitPolicy per model route) but only `limitador_up` and `datastore_partitioned` reach any Prometheus; `authorized_calls`/`limited_calls` appear lazily on first hit, Authorino has no ServiceMonitor, and no `TelemetryPolicy` exists, so there is **no tenant label on any metric today**.
7. Praxis AI's metering filters do not export token usage to Prometheus. They write `token.input/output/total` to filter metadata, emit `Praxis-Token-Input|Output|Total|Status` response headers, and POST CloudEvents (`inference.tokens.used`, `inference.request.error`) with `user`, `group`, `subscription`, `model`, `prompt_tokens`, `completion_tokens`, `cached_input_tokens` to an external metering service. The only Prometheus metric in that module is `praxis_ai_metering_report_failures_total`. Per-tenant usage must therefore come from that metering service, from Limitador counters once TelemetryPolicy labels are added, or stay "unavailable".
8. The POC overview contract is small and worth copying verbatim: `{updatedAt, inventorySource, demo, metricsSource, clusters[], metrics[]}` with four metrics (`requests`, `errors`, `latency`, `gpu`), each `{key,label,unit,status,message?,points[{timestamp,value}]}` and `status ∈ ok|not-configured|unavailable|no-data`. The backend never falls back to demo when Thanos is configured and never converts missing samples to zero.
9. Real API shapes observed: `query_range` returns `resultType:"matrix"` with `values:[[unixSeconds,"string"],...]`; Thanos 0.41 adds a `data.analysis` object; parse errors are HTTP 400 `{"status":"error","errorType":"bad_data",...}` (on the ACM proxy that JSON body is served with `Content-Type: text/plain; charset=utf-8`, on the OpenShift querier with `application/json`, so clients must parse the body regardless of content type); too-fine `step` is a **plain-text** 400 ("exceeded maximum resolution of 11,000 points") on the ACM proxy and a JSON `bad_data` 400 with the same text on the OpenShift querier; the OpenShift querier answers 401 plain `Unauthorized`, the ACM proxy answers **302** (OAuth redirect, `text/html` body) for a missing/invalid token.
10. Proposal: keep the POC JSON as the `GetOverview` response, add `GetModelUsage` and `GetTenantUsage` RPCs with the same per-metric status envelope, drive all three from configurable PromQL templates with `__CLUSTERS__` substitution, and make the fake backend reproduce the POC's noise-based generator (no sine waves), the 5-minute sample cadence, and the four status states.

## Facts found on the dev hub (anonymised)

### Monitoring stack

| Item | Observed |
|---|---|
| `cluster-monitoring-config` (openshift-monitoring) | `enableUserWorkload: true` |
| `openshift-user-workload-monitoring` | present: `prometheus-user-workload-0/1`, `thanos-ruler-user-workload-0/1`, `prometheus-operator`. UWM config adds an `additionalAlertmanagerConfigs` entry pointing at the ACM Alertmanager and a `remoteWrite` to Red Hat telemetry. |
| Hub query endpoints (openshift-monitoring) | services `thanos-querier` (aggregates platform + UWM Prometheus), `prometheus-k8s`, `prometheus-k8s-thanos-sidecar`; routes `thanos-querier`, `prometheus-k8s`, `prometheus-k8s-federate`, `alertmanager-main`. Thanos build 0.41.x. |
| Other Prometheus CRs | `openshift-monitoring/k8s`, `openshift-user-workload-monitoring/user-workload`, `openshift-storage/odf-prometheus`. |
| `MultiClusterObservability` | one CR named `observability`. `instanceSize: minimal`, `enableDownsampling: true`, retention raw 24h / 5m 7d / 1h 14d, `observabilityAddonSpec.interval: 300`, `capabilities.platform.metrics.default+ui` and `capabilities.userWorkloads.metrics.default` enabled (MCOA). Status at the time: `Installing` with `Failed: statefulset observability-thanos-compact is not ready` (queries still work). |
| ACM services (open-cluster-management-observability) | `observability-thanos-query`, `observability-thanos-query-frontend`, `observability-observatorium-api`, `rbac-query-proxy`, `observability-thanos-receive*`, `observability-thanos-store-shard-0..2`, `observability-thanos-rule`, `observability-thanos-compact`, `grafana`, `alertmanager`, `metrics-collector`, `uwl-metrics-collector`. |
| ACM routes | `rbac-query-proxy` (https, Prometheus API with per-cluster RBAC), `observatorium-api`, `grafana`, `alertmanager`. |
| Managed clusters | only `local-cluster` (the hub itself), Available=True; its `ai-grid/enabled` label was present on 2026-10-15 and is absent on 2026-10-16. `multicluster-observability-addon` Available. |
| ACM label allowlist | `observability-managed-cluster-label-allowlist` includes `ai-grid/enabled`, `cloud`, `name`, `openshiftVersion*`, `vendor`; these become labels on `acm_managed_cluster_labels` (sanitised to `ai_grid_enabled`, etc.) while the cluster carries them. Observed 2026-10-16 over 7 days: on the OpenShift querier one historical series with `ai_grid_enabled="false"` (never `"true"`), none current; on the fleet endpoint no series with `ai_grid_enabled` at all. |
| ACM metric allowlist | default `observability-metrics-allowlist` (platform `metrics_list.yaml`, `uwl_metrics_list.yaml` = apiserver/etcd/`up` for hosted control planes). No `observability-metrics-custom-allowlist`. |

### The fleet pipeline for AI metrics (why vLLM is on the ACM Thanos)

```
ServiceMonitor/PodMonitor labelled monitoring.opendatahub.io/scrape="true"   (any namespace, 30 s)
pods labelled monitoring.opendatahub.io/scrape (port 8080; router-scheduler pods dropped by this pod-label job)
pods nvidia-dcgm-exporter* in namespace nvidia-gpu-operator (port 9400; keep 9 DCGM_* names; job relabelled to rhoai-accelerator-metrics)
        │  OpenTelemetryCollector data-science-collector (redhat-ods-monitoring, target allocator)
        │  processors: k8sattributes, resourcedetection(openshift), transform/gpu_metrics, batch
        ▼
prometheus exporter :8889 (resource_to_telemetry_conversion) and prometheus/rhacm :8890
        │  ScrapeConfig rhai-metrics-sourced (job=rhai-metrics-sourced, relabel scrape_source="rhai-metrics-sourced")
        │  AND ScrapeConfig rhai-metrics (job=rhai-metrics, no scrape_source label); both carry the agent's selector
        │  label app.kubernetes.io/component=user-workload-metrics-collector, neither sets an interval
        ▼
PrometheusAgent user-workload-metrics-collector (open-cluster-management-agent-addon), spec.scrapeInterval 300s
        │  remoteWrite: add cluster=<ManagedCluster name>, clusterID=<uuid>; exported_job→job
        ▼
Observatorium API → Thanos receive/store → observability-thanos-query → rbac-query-proxy
```

Consequences for the backend and the mock:

- Fleet samples are 300 s apart (the `PrometheusAgent user-workload-metrics-collector` and `platform-metrics-collector` both have `spec.scrapeInterval: 300s`; the ScrapeConfigs set none; observed sample deltas 300 s); the OpenShift querier has 30 s samples. A 1h range on the fleet endpoint yields 12-13 raw points per series. Use `rate(...[15m])` or larger and `step >= 300`.
- `transform/gpu_metrics` sets `DCGM_FI_DEV_GPU_UTIL` and `DCGM_FI_DEV_MEM_COPY_UTIL` to `value / 100`. The POC's `avg(DCGM_FI_DEV_GPU_UTIL{...})` must be multiplied by 100 on the fleet endpoint (or the mock must emit 0..1).
- Two ScrapeConfigs target the same exporter (`rhai-metrics` and `rhai-metrics-sourced`) and **both are active**: both carry the agent's `scrapeConfigSelector` label `app.kubernetes.io/component=user-workload-metrics-collector`. Verified 2026-10-15: `count by (scrape_source)(vllm:request_success_total)` (both variants are reliably present only with a range selector, e.g. `count_over_time(...[1h])`; an instant query can miss the unlabelled twin) returns series **without** `scrape_source` (the `rhai-metrics` twin) alongside the `rhai-metrics-sourced` ones (`vllm:num_requests_running[20m]` shows one series per variant), so every unfiltered fleet counter is doubled today. **Every fleet PromQL must pin `scrape_source="rhai-metrics-sourced"`** (or the pipeline must drop the `rhai-metrics` ScrapeConfig). The fake Thanos should emit both label variants so the filter is exercised.
- Labels added on the fleet: `cluster`, `clusterID`, `tenant_id` (Observatorium tenant uuid, not an AI tenant), `receive="true"`, `prometheus="open-cluster-management-agent-addon/user-workload-metrics-collector"`, `scrape_source`, `otel_scope_name`, `otel_scope_version`. `/api/v1/label/cluster/values` returns ManagedCluster names.
- k8s resource attributes (`k8s_namespace_name`, `k8s_pod_name`, `k8s_node_name`, `k8s_cluster_name`, `cloud_region`, ...) appear as labels on the `:8889` exporter path and as `target_info`; the `:8890` path keeps the plain Prometheus labels.

### Metric families present / absent

Checked on both endpoints with `/api/v1/label/__name__/values` and `/api/v1/series` (small `limit`).

| Family | OpenShift querier | ACM fleet Thanos | Notes |
|---|---|---|---|
| `vllm:*` (96 names) | yes | yes | one `LLMInferenceService` (`<publisher>/<small-model>`, namespace `<internal-models-namespace>`) |
| `kserve_vllm:*` (same 96, renamed) | yes | yes | KServe PodMonitor `<isvc>-kserve-llmisvc-engine` applies `__name__ → kserve_$1`; `<isvc>-kserve-llmisvc-engine-default` keeps original names |
| `llm_d_epp_*`, `kserve_llm_d_epp_*` (10 names each) | yes (UWM ServiceMonitors `kserve-llm-isvc-scheduler[-default]`, EPP target `up=1`) | **no** (0 names: the RHOAI collector discovers the EPP through ServiceMonitor `kserve-llm-isvc-scheduler` (label `monitoring.opendatahub.io/scrape=true`), but that scrape fails, so the fleet has `up{job="<isvc>-epp-service"}` = 0 over the last hour (2026-10-16) while UWM has `up=1`; its pod-label job also drops router-scheduler pods) | llm-d endpoint picker (router-scheduler); `name=<inference pool>`; cause corrected 2026-10-16 |
| `kserve_http_request_duration_highr_seconds_*`, `http_request_duration_*`, `http_requests_total` | yes | partially | the KServe python server's HTTP histograms |
| `kserve_controller_runtime_*`, `kserve_go_*` | yes | no | controller runtime noise |
| `istio_requests_total`, `istio_request_duration_milliseconds_*` | **no series** | **no series** | PodMonitor `openshift-ingress/openshift-ai-inference-metrics` keeps exactly these names; scrape `up=1`, `scrape_samples_scraped≈392`, `scrape_samples_post_metric_relabeling=0` |
| `envoy_*` | no | no | the Gateway pods are scraped on `/stats/prometheus` but only `istio_*` names are kept |
| `limitador_up`, `datastore_partitioned` | no | yes | ServiceMonitor `redhat-ods-monitoring/limitador-metrics` (`namespaceSelector.any`, so UWM ignores it; the RHOAI collector honours it) |
| `authorized_calls`, `limited_calls`, `report_calls`, `datastore_latency` | no | no | created lazily by Limitador on first hit; no traffic yet |
| `auth_server_*` (Authorino) | no | no | no ServiceMonitor for Authorino |
| `kuadrant_*` | no | no | Kuadrant CR `spec.observability` is empty (not enabled) |
| `DCGM_*`, `nvidia_*` | no | no | no GPU nodes, no `nvidia-gpu-operator` namespace on the hub; collector scrape job `dcgm-exporter-accelerator-metrics` is configured and relabels `job` to `rhoai-accelerator-metrics` |
| `ai_gateway_*`, `praxis_*`, `maas_*`, `inference_*`, `gateway_api_*`, `otel_*` | no | no | POC names and Praxis names do not exist. `openshift:gateway_api_usage:count`, `kube_customresource_gateway_info` exist (inventory only) |
| `acm_managed_cluster_labels`, `acm_managed_cluster_info`, `acm_managed_cluster_status_condition`, `acm_managed_cluster_count` | yes | `acm_managed_cluster_labels` only | fleet inventory metrics |
| `kube_pod_container_resource_requests`, `kube_node_status_allocatable` | yes | yes | can give `resource="nvidia_com_gpu"` allocation once GPU nodes exist |

Label sets observed (fleet endpoint unless marked; the `llm_d_epp_*` and `acm_managed_cluster_info` lines are from the OpenShift querier, and `ai_grid_enabled` on `acm_managed_cluster_labels` only appears there, while the cluster label is set; redacted):

```
vllm:request_success_total{cluster, clusterID, container="main", endpoint="8000", engine="0",
  finished_reason="abort|error|length|repetition|stop", job="<ns>/<isvc>-kserve-llmisvc-engine-default",
  llm_isvc_component="workload", llm_isvc_name="<isvc>", llm_isvc_role="both",
  model_name="<org>/<model>", namespace, pod, prometheus, receive="true",
  scrape_source="rhai-metrics-sourced", tenant_id, otel_scope_name, otel_scope_version}
vllm:e2e_request_latency_seconds_bucket{..., le="0.3|0.5|...|+Inf"}
vllm:prompt_tokens_by_source_total{..., source="external_kv_transfer|local_cache_hit|local_compute"}
vllm:cache_config_info{..., block_size, gpu_memory_utilization, kv_cache_size_tokens, num_gpu_blocks, ...}
llm_d_epp_average_kv_cache_utilization{name="<isvc>-inference-pool", llm_isvc_component="router-scheduler",
  exported_namespace, service="<isvc>-epp-service", ...}
acm_managed_cluster_labels{name="<cluster>", ai_grid_enabled="false" (OpenShift querier only), cloud, vendor, openshiftVersion,
  hub_cluster_id, managed_cluster_id, local_cluster, cluster_open_cluster_management_io_clusterset, ...}
acm_managed_cluster_info{available="True", cloud, core_worker, hub_type="acm", product, vendor, version, managed_cluster_id}
limitador_up{cluster, container="limitador", job="limitador-limitador", namespace="kuadrant-system", service, ...}
up{job="openshift-ingress/openshift-ai-inference-metrics", gateway_name="openshift-ai-inference",
  llm_isvc_gateway="true", container="istio-proxy", namespace="openshift-ingress"}
```

### Gateway and policy inventory relevant to tenant usage

- Gateways (openshift-ingress): `openshift-ai-inference` (class `openshift-ai-inference`; has the metrics PodMonitor and an AuthPolicy), `maas-default-gateway` (class `maas-gateway-class`; AuthPolicy + default-deny TokenRateLimitPolicy; **no PodMonitor**), `data-science-gateway`.
- `TokenRateLimitPolicy` per model HTTPRoute (namespace `models-as-a-service`, target `external-model-<model>`): counters `auth.identity.selected_subscription_id` and `auth.identity.userid`, 1000 tokens / 1m. Limitador therefore keys counters by subscription and user, with `limitador_namespace="<ns>/<route>"`. Limitador exposes `authorized_calls` (requests), `authorized_hits` (hits; under a TokenRateLimitPolicy hits are **tokens**) and `limited_calls`; `limit_name` only appears when the `Limitador` CR has `spec.telemetry: exhaustive` (the operator then passes the flag; `kuadrant-operator doc/user-guides/observability/limitador-metrics.md`). The hub `Limitador` CR has `telemetry` unset and `spec.metricLabelsDefault: "descriptors[1]"`.
- `TelemetryPolicy` CRD (`extensions.kuadrant.io/v1alpha1`, `spec.metrics.default.labels: map<label, CEL>`) exists; **no instances**. This is the only documented way to put `user`/`group`/`model` labels on `authorized_calls`/`limited_calls`.
- MaaS: `AITenant`, `Tenant`, `MaaSSubscription`, `MaaSModelRef` CRDs; `maas-api-metrics` ServiceMonitor is labelled for the RHOAI collector but has no `up` series anywhere (target not discovered or TLS scrape failing), so maas-api's metric names are unknown; `maas-controller-metrics` only exposes controller-runtime metrics.
- Istio: 13 `EnvoyFilter`s in `openshift-ingress` (Kuadrant auth/ratelimiting for both gateways, authn-ssl, MaaS payload processing, external-model payload processing; count verified 2026-10-15); no `Telemetry` resources, no `WasmPlugin`s.

## The POC's overview contract and demo logic

Files read: `docs/grid-monitoring.md`, `backend/grid-monitoring.mjs`, `shared/grid-monitoring.mjs`,
`src/services/monitoring/demoObservability.ts`.

### `GET /api/grid/overview?range=1h|6h|24h&site=<site>&metrics=false`

- `range` → seconds 3600/21600/86400 (422 `validation` otherwise); `end = floor(now)`, `start = end - seconds`, `step = ceil(seconds/60)` (60, 360, 1440 s → 61 points).
- Clusters: all enrolled (`aiGridEnabled`) clusters, optionally filtered by `site`. `__CLUSTERS__` is replaced by `JSON.stringify(names.map(regexEscape).join('|'))`, i.e. a quoted regex: `cluster=~"a|b|c"`.
- `metrics=false` → `metrics: []`. No `THANOS_QUERY_URL` → demo series, `metricsSource: "demo"`.
- Thanos mode: per metric, `GET {THANOS_QUERY_URL}/api/v1/query_range?query&start&end&step&partial_response=false`, `Accept: application/json`, optional `Authorization: Bearer <THANOS_TOKEN_FILE contents>`, `redirect: 'error'`, 10 s timeout. The query is `env.THANOS_<KEY>_QUERY ?? default`. Accepted only if `status==="success"`, `resultType==="matrix"`, no `warnings`, at most **one** series; otherwise `unavailable`. Empty values → `no-data`. Points are filtered to finite numbers. A failing Thanos never triggers demo.
- Default queries (zod `metricDefinitions`):

| key | label | unit | default PromQL |
|---|---|---|---|
| `requests` | Gateway request rate | `req/s` | `sum(rate(ai_gateway_requests_total{cluster=~__CLUSTERS__}[5m]))` |
| `errors` | Gateway error rate | `req/s` | `sum(rate(ai_gateway_requests_total{cluster=~__CLUSTERS__,status=~"5.."}[5m]))` |
| `latency` | Gateway p95 latency | `ms` | `1000 * histogram_quantile(0.95, sum by (le) (rate(ai_gateway_request_duration_seconds_bucket{cluster=~__CLUSTERS__}[5m])))` |
| `gpu` | GPU utilization | `%` | `avg(DCGM_FI_DEV_GPU_UTIL{cluster=~__CLUSTERS__})` |

Response schema (`gridOverviewSchema`):

```json
{
  "updatedAt": "2026-10-15T09:00:00.000Z",
  "inventorySource": "acm",
  "demo": false,
  "metricsSource": "thanos",
  "clusters": [ { "name": "site-a-cluster-1", "status": "Ready", "aiGridEnabled": true, "site": "site-a", "siteName": "Site A", "latitude": 50.1, "longitude": 4.3, "location": "site-a" } ],
  "metrics": [
    { "key": "requests", "label": "Gateway request rate", "unit": "req/s", "status": "ok",
      "points": [ { "timestamp": 1792000000, "value": 123.456 }, { "timestamp": 1792000060, "value": 118.902 } ] },
    { "key": "errors",   "label": "Gateway error rate",   "unit": "req/s", "status": "no-data",
      "message": "No samples available for this selection.", "points": [] },
    { "key": "latency",  "label": "Gateway p95 latency",  "unit": "ms",    "status": "unavailable",
      "message": "This metric could not be queried. Check backend Thanos configuration and connectivity.", "points": [] },
    { "key": "gpu",      "label": "GPU utilization",      "unit": "%",     "status": "not-configured",
      "message": "Thanos is not configured.", "points": [] }
  ]
}
```

`inventorySource ∈ acm|git|demo`, `metricsSource ∈ thanos|demo` (default `thanos`), `metrics[].status ∈ ok|unavailable|not-configured|no-data`, `points[].value` must be finite. `demo` is the inventory flag (browser mock), `metricsSource` the telemetry flag; both are shown to the user ("simulation banner").

### Demo generator (`shared/grid-monitoring.mjs::demoMetrics`)

Deterministic, timestamp-keyed, no periodic waves. Reproduce exactly so mock and backend stay consistent across ranges:

```
hash(name)      = fold over chars: n = (n*31 + code) >>> 0, seed 7
random(seed,b)  = 32-bit hash mix of (seed ^ imul(b, 0x45d9f3b)) → [0,1)
                  (n ^= n>>>16; n*=0x7feb352d; n ^= n>>>15; n*=0x846ca68b; n ^= n>>>16) / 2^32
                  # NOTE: the browser-side noise() below is a SHORTER mixer, not this one
traffic(t,seed) = max(0.04, baseline + offset + spike) where
                  baseline = 0.6 + (random(seed+3, floor(t/1800)) - 0.5) * 0.4      # drifts every 30 min
                  offset   = (random(seed, floor(t/30)) - 0.5) * 0.9                 # per 30 s bucket
                  spike    = random(seed+2, floor(t/30)) > 0.91 ? 0.5 + random(seed+4, floor(t/30)) * 1.2 : 0
per Ready cluster at timestamp t:
  seed = hash(name); capacity = 90 + seed % 120                                      # 90..209 req/s
  load = traffic(t); recent = 0.8*load + 0.15*traffic(t-30) + 0.05*traffic(t-60)     # short lag
  requests = capacity * load
  gpu      = min(98, 8 + recent*66 + (random(seed+1, floor(t/30)) - 0.5) * 4)        # follows traffic, ceiling 98
  pressure = max(0, recent - 0.85)
  errors   = requests * (0.001 + pressure*0.018 + random(seed+2, floor(t/30)) * 0.0015)
  latency  = 65 + recent*95 + pressure*220                                           # ms
fleet row: requests = Σ, errors = Σ, gpu = Σ gpu_i / max(1, n),
           latency = Σ(latency_i * requests_i) / max(1, Σ requests_i)   (request-weighted)
points: floor((end-start)/step)+1 timestamps from start;
values rounded to 3 decimals (toFixed(3)); Offline/Unknown clusters emit nothing (only status === 'Ready' counts);
no Ready clusters → status "no-data" with a message; otherwise "ok".
```

### Model and tenant simulation (`demoObservability.ts`, browser side)

- `seed(name)` = `imul(n,31)+code` folded from 7 with a final `>>>0` (numerically identical to the shared `hash`). `noise(name, t)` is a **different, two-step mixer** (`demoObservability.ts:6`, verified 2026-10-15): `n = (seed(name) ^ imul(floor(t/30), 0x45d9f3b)) >>> 0; n = imul(n ^ (n>>>16), 0x7feb352d); return ((n ^ (n>>>15)) >>> 0) / 2^32` -- it omits the `0x846ca68b` multiply and the final `>>>16` step of `random()`. Port it separately or model/tenant demo values will not match the browser mock. `total(metric)` = trapezoidal integral of `points` when `status === 'ok'`, else undefined (used for "requests in window", "tokens in window").
- `modelMetrics(models, metrics, simulated)` derives six series from the fleet `requests` (`load`) and `latency` points: `queue` (`max(0, round(load*0.07 + jitter*6 - 2))`), `replicas` (Σ model.replicas, constant), `p90` (`latency*(0.8+profile/300) + (jitter-0.5)*25` ms), `ttft` (`30 + load*0.35 + profile*0.3 + (jitter-0.5)*15` ms), `tokens` (`load*(14+profile/10)*(0.8+jitter*0.4)` tokens/s), `cache` (`min(98, 15 + load*0.18 + profile*0.2 + (jitter-0.5)*12)` %), where `profile = mean(seed(model) % 97)`, `jitter = noise(<model names joined by ','> + key, t)`, `load` = the fleet `requests` value at that timestamp, and the p90 fallback latency is 180 when the latency point is missing. Values are rounded to **2** decimals (`toFixed(2)`), unlike the overview's 3. Status is `ok` only in demo mode with `requests.status === 'ok'` and models selected; otherwise `no-data` with message "No model telemetry for this selection." (demo) or "Per-model Thanos telemetry is not configured." (Thanos mode).
- `tenantUsage(metrics, tenants)`: per sample and tenant, weight `0.3 + noise(tenant+'-traffic', floor(t/900)*900)*1.8` normalised over tenants by `max(0.001, Σ weights)`; `inputPerRequest = 150 + seed(tenant)%350 + noise(tenant+'-prompt', t)*220`, `outputPerRequest = 60 + seed(tenant+'-output')%180 + noise(tenant+'-generation', t)*120` (note the `-output` suffix on that seed), `cacheFraction = 0.05 + noise(tenant+'-cache'…, floor(t/300)*300)*0.65`; `input = req*w*inputPerRequest*(1-cacheFraction)`, `cached = req*w*inputPerRequest*cacheFraction`, `output = req*w*outputPerRequest`; each integrated over the window (trapezoid) and `Math.round`ed; rows are `{id: name, name, input, output, cached}`. Tenant identities always come from the tenant API; only measurements are simulated. The function has no mode check of its own; the "unavailable in Thanos mode" behaviour is applied by its caller (not verified).

## Metric catalog

"Confirmed on hub" = series observed on the dev hub (either endpoint). "config" = scrape wiring observed but no series yet. "n/a" = not applicable on the hub (no GPUs / component not deployed).

| Metric | Source component | Key labels | Confirmed on hub | Docs |
|---|---|---|---|---|
| `vllm:request_success_total` (counter) | vLLM engine | `model_name`, `finished_reason` (`abort\|error\|length\|repetition\|stop` observed), `engine`, `namespace`, `pod`, `llm_isvc_name`, fleet `cluster` | yes | https://docs.vllm.ai/en/latest/usage/metrics/ ; names cross-checked against `vllm/v1/metrics/loggers.py` |
| `vllm:e2e_request_latency_seconds_{bucket,sum,count}` | vLLM | `model_name`, `le` | yes | same |
| `vllm:time_to_first_token_seconds_*`, `vllm:inter_token_latency_seconds_*`, `vllm:request_queue_time_seconds_*` | vLLM | `model_name`, `le` | yes (ttft/itl) | same |
| `vllm:prompt_tokens_total`, `vllm:generation_tokens_total` (counters) | vLLM | `model_name` | yes | same |
| `vllm:prompt_tokens_cached_total`, `vllm:prompt_tokens_by_source_total{source}` | vLLM | `model_name`, `source` | yes | same |
| `vllm:kv_cache_usage_perc` (gauge, 0..1) | vLLM | `model_name` | yes | same (the older `vllm:gpu_cache_usage_perc` is not on the hub) |
| `vllm:num_requests_running`, `vllm:num_requests_waiting` | vLLM | `model_name` | yes | same |
| `kserve_vllm:*` | KServe PodMonitor rename of the above | same | yes | KServe LLMInferenceService monitoring (odh-model-controller PodMonitors observed) |
| `llm_d_epp_average_kv_cache_utilization`, `llm_d_epp_average_queue_size`, `llm_d_epp_ready_endpoints`, `llm_d_epp_per_endpoint_queue_size` | llm-d endpoint picker (Gateway API Inference Extension) | `name` (InferencePool), `exported_namespace` | yes (UWM, `up=1`); **absent on the fleet endpoint** (the collector's scrape of the EPP target fails, fleet `up=0`) | upstream names are `inference_pool_*` / `inference_model_*`: https://gateway-api-inference-extension.sigs.k8s.io/ (assumed mapping; the llm-d scheduler renames with `llm_d_epp_`) |
| `istio_requests_total` (counter) | Istio gateway proxy | `reporter`, `response_code`, `response_flags`, `destination_service`, `destination_service_name`, `destination_service_namespace`, `source_workload`, `request_protocol`; hub adds `gateway_name`, `llm_isvc_gateway` | config (0 samples) | https://istio.io/latest/docs/reference/config/metrics/ |
| `istio_request_duration_milliseconds_{bucket,sum,count}` | Istio | same + `le` | config | same |
| `istio_request_bytes_*`, `istio_response_bytes_*` | Istio | same | config | same |
| `envoy_cluster_upstream_rq_time_bucket`, `envoy_cluster_upstream_rq_total`, `envoy_cluster_upstream_rq_xx{envoy_response_code_class}` | Envoy stats (`cluster.<name>.upstream_rq_*`) | `envoy_cluster_name` | no (filtered out by the PodMonitor) | https://www.envoyproxy.io/docs/envoy/latest/configuration/upstream/cluster_manager/cluster_stats |
| `limitador_up` (gauge), `datastore_partitioned` (gauge) | Limitador | fleet `cluster`, `namespace="kuadrant-system"` | yes (ACM) | https://github.com/Kuadrant/limitador/blob/main/limitador-server/src/prometheus_metrics.rs |
| `authorized_calls` (requests), `authorized_hits` (hits = **tokens** under a TokenRateLimitPolicy), `limited_calls` (counters), `datastore_latency` (histogram) | Limitador | `limitador_namespace="<ns>/<route or gateway>"`, optional `limit_name` (requires `Limitador.spec.telemetry: exhaustive`; unset on the hub), plus TelemetryPolicy labels (e.g. `user`, `group`) | no (lazy, no traffic) | same; https://docs.kuadrant.io/latest/kuadrant-operator/doc/user-guides/observability/token-metrics/ ; TelemetryPolicy: https://docs.kuadrant.io/latest/kuadrant-operator/doc/overviews/telemetrypolicy/ |
| `auth_server_authconfig_response_status{status}`, `auth_server_response_status`, `auth_server_authconfig_total`, `auth_server_authconfig_duration_seconds`, `auth_server_evaluator_*` (opt-in) | Authorino (`/server-metrics`, port 8080) | `namespace`, `authconfig`, `status=OK|UNAUTHENTICATED|PERMISSION_DENIED|NOT_FOUND`, `evaluator_type`, `evaluator_name` | no (no ServiceMonitor) | https://docs.kuadrant.io/latest/authorino/docs/user-guides/observability/ |
| `kuadrant_policies_total{kind}`, `kuadrant_policies_enforced{kind,status}`, `kuadrant_component_ready{component,namespace}` | kuadrant-operator (needs `observability.enable: true`) | | no | https://docs.kuadrant.io/latest/kuadrant-operator/doc/observability/metrics/ |
| `DCGM_FI_DEV_GPU_UTIL` (gauge, % at exporter; **/100 on the fleet**) | DCGM exporter via the RHOAI collector scrape config `dcgm-exporter-accelerator-metrics`, whose relabelling sets the `job` label to `rhoai-accelerator-metrics` (`OpenTelemetryCollector` `data-science-collector` config, `hub:` 2026-10-16) | `gpu`, `UUID`, `device`, `modelName`, `Hostname`, `DCGM_FI_DRIVER_VERSION`; with `-k`: `pod`, `namespace`, `container`; hub relabels add `node`, `component` | n/a (no GPUs) | https://github.com/NVIDIA/dcgm-exporter/blob/main/etc/default-counters.csv ; https://docs.nvidia.com/datacenter/cloud-native/gpu-telemetry/latest/dcgm-exporter.html |
| `DCGM_FI_DEV_MEM_COPY_UTIL` (/100 on fleet), `DCGM_FI_DEV_FB_USED`, `DCGM_FI_DEV_FB_FREE` (MiB), `DCGM_FI_DEV_POWER_USAGE` (W), `DCGM_FI_DEV_GPU_TEMP` (°C), `DCGM_FI_DEV_SM_CLOCK`, `DCGM_FI_DEV_MEM_CLOCK` (MHz), `DCGM_FI_PROF_GR_ENGINE_ACTIVE` (ratio) | same (the nine names kept by the collector) | same | n/a | same |
| `kube_pod_container_resource_requests{resource="nvidia_com_gpu"}`, `kube_node_status_allocatable{resource="nvidia_com_gpu"}` | kube-state-metrics (ACM default allowlist forwards both) | `namespace`, `pod`, `node`, `resource`, `unit` | yes (metric), n/a (resource) | ACM allowlist observed |
| `acm_managed_cluster_labels{name, cloud, vendor, openshiftVersion, ...}` (`ai_grid_enabled` only while the cluster label is set, seen on the OpenShift querier only) | ACM clusterlifecycle-state-metrics | | yes | ACM observability docs |
| `acm_managed_cluster_info{available, product, version, core_worker}` | ACM | | yes (hub only) | same |
| `ai_gateway_requests_total`, `ai_gateway_request_duration_seconds_bucket` | POC assumption | `cluster`, `status` | **no, do not exist** | POC `docs/grid-monitoring.md` |
| `praxis_ai_metering_report_failures_total` (counter) | Praxis `external_metering` filter | none | no (Praxis not scraped) | `praxis-ai-docs/metering_mod.rs` |
| Praxis headers `Praxis-Token-Input|Output|Total|Status`; CloudEvents `inference.tokens.used` / `inference.request.error` | Praxis `token_count` → `token_usage_headers` / `external_metering` | event `data`: `user`, `group`, `subscription`, `provider`, `model`, `duration_ms`, `prompt_tokens`, `completion_tokens`, `total_tokens`, `cached_input_tokens`, `cache_creation_tokens`, `reasoning_tokens` | not a metric | `praxis-ai-docs/*.md`, `external-metering.yaml` |

Identity labels: **model** = `model_name` (vLLM), `llm_isvc_name` (KServe), `destination_service_name`/HTTPRoute (Istio), `limitador_namespace` suffix (Limitador). **Tenant** = none on any Prometheus metric today; candidates are TelemetryPolicy labels on Limitador counters (`auth.identity.userid`, `auth.identity.groups`, `auth.identity.selected_subscription_id`) or the Praxis metering service's CloudEvents. **Cluster** = `cluster` (fleet only).

## Prometheus / Thanos HTTP API shapes

Both endpoints speak the Prometheus v1 API (https://prometheus.io/docs/prometheus/latest/querying/api/). Thanos Query (https://thanos.io/tip/components/query.md/) adds the parameters `dedup` (default true), `partial_response` (default from `--query.partial-response`; the POC sends `false`), `replicaLabels`, `max_source_resolution` (`auto|0|5m|1h`; `auto` observed to work for 24h ranges), `storeMatch[]`, and reports partial data as `warnings[]` with HTTP 200. Thanos 0.41 (both endpoints) also returns `data.analysis` (`{}` or `{"name":"","executionTime":"0s","children":null}`); ignore unknown fields.

Range query request the backend sends (POC shape plus `dedup=true`, which the POC itself does not send; `start/end` unix seconds, `step` seconds):

```
GET /api/v1/query_range?query=<promql>&start=1792000000&end=1792003600&step=60&partial_response=false&dedup=true
Accept: application/json
Authorization: Bearer <service-account token>
```

Success, matrix (fictional):

```json
{
  "status": "success",
  "data": {
    "resultType": "matrix",
    "result": [
      { "metric": { "cluster": "site-a-cluster-1" },
        "values": [[1792000000, "12.345"], [1792000300, "13.001"], [1792000600, "NaN"]] },
      { "metric": { "cluster": "site-b-cluster-1" },
        "values": [[1792000000, "4.2"], [1792000300, "4.9"]] }
    ],
    "analysis": {}
  }
}
```

Notes: `values[i] = [unixSecondsAsNumber, valueAsString]`; strings may be `"NaN"`, `"+Inf"`, `"-Inf"`; gaps are simply missing timestamps (observed: a 1h range at 300 s step returned 10 of 13 expected points). Fleet series have no `__name__` after `sum by`. An empty result is `{"status":"success","data":{"resultType":"matrix","result":[],"analysis":{}}}` → POC maps to `no-data`.

Instant query, vector:

```json
{ "status": "success",
  "data": { "resultType": "vector",
    "result": [ { "metric": { "cluster": "site-a-cluster-1", "model_name": "org/model-7b" }, "value": [1792003600.802, "3"] } ],
    "analysis": {} } }
```

Series discovery: `GET /api/v1/series?match[]=vllm:request_success_total&limit=3` → `{"status":"success","data":[{ "__name__": "...", "cluster": "...", ... }]}`; label values: `GET /api/v1/label/cluster/values` → `{"status":"success","data":["site-a-cluster-1","site-b-cluster-1"]}`; names: `GET /api/v1/label/__name__/values`.

Error cases observed:

| Case | HTTP | Body |
|---|---|---|
| PromQL parse error (both) | 400 | `{"status":"error","errorType":"bad_data","error":"1:9: parse error: unclosed left parenthesis"}`; OpenShift querier sends `Content-Type: application/json`, the ACM proxy sends the same JSON body with **`Content-Type: text/plain; charset=utf-8`** (verified 2026-10-15), so parse the body regardless of content type |
| `step` too small for range (ACM) | 400 | **plain text**: `exceeded maximum resolution of 11,000 points per timeseries. Try decreasing the query resolution (?step=XX)` |
| `step` too small for range (OpenShift querier) | 400 | JSON `{"status":"error","errorType":"bad_data","error":"exceeded maximum resolution of 11,000 points per timeseries. ..."}` |
| Missing/invalid bearer token, OpenShift `thanos-querier` | 401 | plain text `Unauthorized` |
| Missing/invalid bearer token, ACM `rbac-query-proxy` | 302 | redirect to the OAuth login (no JSON). The POC's `redirect: 'error'` turns this into `unavailable`; the Go client must also refuse to follow redirects. |
| Expression cannot execute (docs) | 422 | `{"status":"error","errorType":"execution",...}` |
| Timeout / cancelled (docs) | 503 | `{"status":"error","errorType":"timeout"|"canceled",...}` |
| Partial store failure with `partial_response=true` | 200 | success plus `"warnings":["..."]` (POC treats any warning as `unavailable`) |
| More than one series when one aggregate was expected | 200 | valid matrix; POC rejects → `unavailable` |

RBAC on the fleet endpoint (from ACM docs; not verified with a restricted token): `rbac-query-proxy` filters by the caller's access to `managedclusters` (a token for a user/service account that can `get` managed clusters; fine-grained `metrics/<namespace>` resourceNames are Tech Preview). Inside the cluster the backend should target the `rbac-query-proxy` service (https, port named `https`) or `observability-thanos-query` directly (no RBAC; needs network policy consideration); the route exists for development only.

## Proposal

### Proto for `Grid/GetOverview` and usage RPCs

Mirror the POC contract one-to-one so the BFF mapping is a straight JSON rename; add explicit enums instead of strings.

```proto
syntax = "proto3";
package aigrid.grid.v1;
import "google/protobuf/timestamp.proto";

enum TimeRange { TIME_RANGE_UNSPECIFIED = 0; TIME_RANGE_1H = 1; TIME_RANGE_6H = 2; TIME_RANGE_24H = 3; }
enum MetricsSource { METRICS_SOURCE_UNSPECIFIED = 0; METRICS_SOURCE_THANOS = 1; METRICS_SOURCE_DEMO = 2; }
enum MetricStatus { METRIC_STATUS_UNSPECIFIED = 0; OK = 1; NOT_CONFIGURED = 2; UNAVAILABLE = 3; NO_DATA = 4; }
enum InventorySource { INVENTORY_SOURCE_UNSPECIFIED = 0; ACM = 1; GIT = 2; DEMO = 3; }

message Point { int64 timestamp = 1; double value = 2; }           // unix seconds; value finite

message MetricSeries {
  string key = 1;            // "requests" | "errors" | "latency" | "gpu" | "queue" | "p90" | "ttft" | "tokens" | "cache" | "replicas"
  string label = 2;          // display label from the backend's metric definition
  string unit = 3;           // "req/s" | "ms" | "%" | "tokens/s" | "requests" | "replicas"
  MetricStatus status = 4;
  string message = 5;        // set when status != OK
  repeated Point points = 6; // empty unless OK
  string query = 7;          // optional: the PromQL that ran (debug; strip in BFF)
}

message GetOverviewRequest {
  TimeRange range = 1;       // default 1H
  string site = 2;           // optional ai-grid/site filter
  bool skip_metrics = 3;     // POC metrics=false (inventory only)
}
message GetOverviewResponse {
  google.protobuf.Timestamp updated_at = 1;
  InventorySource inventory_source = 2;
  bool demo = 3;
  MetricsSource metrics_source = 4;
  repeated ManagedCluster clusters = 5;   // existing message; needs status/site/siteName/lat/lon
  repeated MetricSeries metrics = 6;      // requests, errors, latency, gpu (always these four, in this order)
}

message GetModelUsageRequest  { TimeRange range = 1; string site = 2; repeated string model_names = 3; }
message GetModelUsageResponse {
  google.protobuf.Timestamp updated_at = 1; MetricsSource metrics_source = 2;
  repeated ModelUsage models = 3;
}
message ModelUsage {
  string model_name = 1;              // vLLM model_name
  repeated string clusters = 2;       // clusters reporting it
  repeated MetricSeries metrics = 3;  // queue, replicas, p90, ttft, tokens (prompt+generation/s), cache, plus requests/errors per model
}

message GetTenantUsageRequest  { TimeRange range = 1; string site = 2; }
message GetTenantUsageResponse {
  google.protobuf.Timestamp updated_at = 1; MetricsSource metrics_source = 2;
  MetricStatus status = 3; string message = 4;   // whole-table status (UNAVAILABLE until a tenant label exists)
  repeated TenantUsage tenants = 5;
}
message TenantUsage {
  string tenant_id = 1; string name = 2;         // identity from the Tenants service, never from metrics
  int64 input_tokens = 3; int64 cached_input_tokens = 4; int64 output_tokens = 5; int64 requests = 6;
  repeated MetricSeries series = 7;              // optional per-tenant time series
}

service Grid {
  rpc GetOverview(GetOverviewRequest) returns (GetOverviewResponse);
  rpc GetModelUsage(GetModelUsageRequest) returns (GetModelUsageResponse);
  rpc GetTenantUsage(GetTenantUsageRequest) returns (GetTenantUsageResponse);
}
```

Backend configuration (keep the POC env names so the ops contract is familiar): `THANOS_QUERY_URL` (fleet `rbac-query-proxy` or `observability-thanos-query` service URL), `THANOS_TOKEN_FILE` (projected SA token, re-read per query), `THANOS_{REQUESTS,ERRORS,LATENCY,GPU}_QUERY` overrides with `__CLUSTERS__`; add `THANOS_SCRAPE_SOURCE` (default `rhai-metrics-sourced`) and `THANOS_GPU_SCALE` (default `100` for the fleet path, `1` when querying DCGM directly). Query parameters: `start/end/step` as in the POC but `step = max(ceil(range/60), 300)` on the fleet endpoint; send `partial_response=false`, `dedup=true`; 10 s timeout; no redirects.

### PromQL per metric (fleet endpoint, `cluster=~__CLUSTERS__` substituted)

Status legend: **confirmed** = metric and labels observed; **wired** = scrape config observed, no samples yet; **assumed** = from docs only.

Overview:

| key | PromQL | Status |
|---|---|---|
| `requests` (gateway) | `sum(rate(istio_requests_total{cluster=~__CLUSTERS__, llm_isvc_gateway="true", reporter="source"}[15m]))` | wired (gateway PodMonitor keeps it; 0 samples today) |
| `requests` (fallback, engine side) | `sum(rate(vllm:request_success_total{cluster=~__CLUSTERS__, scrape_source="rhai-metrics-sourced"}[15m]))` | confirmed metric; counts completed requests, not gateway hits |
| `errors` | `sum(rate(istio_requests_total{cluster=~__CLUSTERS__, llm_isvc_gateway="true", reporter="source", response_code=~"5.."}[15m]))` | wired; fallback `sum(rate(vllm:request_success_total{..., finished_reason="error"}[15m]))` (confirmed label value) |
| `latency` p95 (ms) | `histogram_quantile(0.95, sum by (le) (rate(istio_request_duration_milliseconds_bucket{cluster=~__CLUSTERS__, llm_isvc_gateway="true", reporter="source"}[15m])))` (already ms) | wired; fallback `1000 * histogram_quantile(0.95, sum by (le) (rate(vllm:e2e_request_latency_seconds_bucket{cluster=~__CLUSTERS__, scrape_source="rhai-metrics-sourced"}[15m])))` confirmed |
| `gpu` (%) | `100 * avg(DCGM_FI_DEV_GPU_UTIL{cluster=~__CLUSTERS__, job="rhoai-accelerator-metrics", scrape_source="rhai-metrics-sourced"})` | assumed (collector scrape job `dcgm-exporter-accelerator-metrics` relabels `job` to `rhoai-accelerator-metrics` in the OTel config; metric absent on hub; `×100` because of `transform/gpu_metrics`) |
| `gpu` alternative (no DCGM) | `100 * avg(vllm:kv_cache_usage_perc{cluster=~__CLUSTERS__, scrape_source="rhai-metrics-sourced"})` (KV-cache fill, not SM utilisation) | confirmed metric; different semantics, label it as such |
| per-cluster variants | same expressions with `sum by (cluster)` / `avg by (cluster)`; the POC rejects multi-series results, so either keep one aggregate per call or extend the parser to a `cluster`-keyed map | |

Per model (group by `model_name`, optionally `cluster`):

| series | PromQL | Status |
|---|---|---|
| requests/s | `sum by (model_name) (rate(vllm:request_success_total{cluster=~__CLUSTERS__, scrape_source="rhai-metrics-sourced"}[15m]))` | confirmed |
| error share | `sum by (model_name) (rate(vllm:request_success_total{..., finished_reason="error"}[15m]))` | confirmed |
| queue | `sum by (model_name) (vllm:num_requests_waiting{...})` | confirmed |
| running / replicas | `sum by (model_name) (vllm:num_requests_running{...})`; replicas `count by (model_name) (vllm:num_requests_running{...})` on the fleet (`llm_d_epp_ready_endpoints{name=~".*"}` exists only on the per-cluster OpenShift querier, not on the fleet endpoint) | confirmed (replicas via count assumed) |
| p90 e2e (ms) | `1000 * histogram_quantile(0.90, sum by (model_name, le) (rate(vllm:e2e_request_latency_seconds_bucket{...}[15m])))` | confirmed |
| p90 TTFT (ms) | `1000 * histogram_quantile(0.90, sum by (model_name, le) (rate(vllm:time_to_first_token_seconds_bucket{...}[15m])))` | confirmed |
| tokens/s | `sum by (model_name) (rate(vllm:prompt_tokens_total{...}[15m]) + rate(vllm:generation_tokens_total{...}[15m]))` | confirmed |
| tokens in window (prompt / generation / cached) | `sum by (model_name) (increase(vllm:prompt_tokens_total{...}[<range>]))`, same for `vllm:generation_tokens_total`, `vllm:prompt_tokens_cached_total` | confirmed |
| KV cache % | `100 * avg by (model_name) (vllm:kv_cache_usage_perc{...})` | confirmed |

Per tenant: nothing confirmed. Options in order of likelihood:

1. Limitador counters with a `TelemetryPolicy` on the inference gateway adding `user: auth.identity.userid`, `subscription: auth.identity.selected_subscription_id`, `group: auth.identity.groups` → tokens: `sum by (user, subscription) (increase(authorized_hits{cluster=~__CLUSTERS__, limitador_namespace=~"models-as-a-service/.*"}[<range>]))` (under a TokenRateLimitPolicy each hit is a token); requests: the same over `authorized_calls`; rejected calls: `limited_calls`. Requires: TelemetryPolicy instance, Limitador scraped (already), `Limitador.spec.telemetry: exhaustive` if the `limit_name` label is wanted. **assumed**.
2. Praxis `external_metering` CloudEvents consumed by a metering service that the backend queries (not Prometheus). Tenant key = `user`/`group`/`subscription`, model key = `model`, counts = `prompt_tokens`, `completion_tokens`, `cached_input_tokens`. **documented, not deployed on the hub**.
3. Until one exists, `GetTenantUsage` returns `status: UNAVAILABLE, message: "Per-tenant telemetry is not configured."` exactly as the POC does in Thanos mode; the frontend keeps the tenant list from the Tenants service and shows no numbers.

### What the fake backend must simulate

- Inputs: `range ∈ {1h,6h,24h}`, `site`, `skip_metrics`; `end = floor(now)`, `start = end - range`, `step = ceil(range/60)` for demo (61 points) and, when emulating the fleet, `step = 300` with 300-s-aligned timestamps and occasional dropped points (observed gaps).
- `metricsSource`: `DEMO` when `THANOS_QUERY_URL` is unset (banner); `THANOS` when set, in which case the fake must emulate a Thanos instead of inventing numbers. Provide a fake Thanos HTTP server (or an in-process client stub) with: success matrix/vector (including both `scrape_source="rhai-metrics-sourced"` and unlabelled twin series so the pin filter is exercised), empty result (`NO_DATA`), 400 `bad_data` JSON (both `Content-Type: application/json` and the ACM proxy's `text/plain` carrying the same JSON body), 400 plain-text "11,000 points" (ACM) and its JSON `bad_data` twin (OpenShift querier), 401 plain text and 302 redirect with a `text/html` body (`UNAVAILABLE`), 200 with `warnings` (`UNAVAILABLE`), >1 series (`UNAVAILABLE`), `NaN`/`+Inf` strings (dropped points), and a configurable delay (fleet query latency of 0.2-1.5 s was reported in the first pass but not re-measured; the POC's timeout is 10 s, so also simulate a >10 s stall).
- `NOT_CONFIGURED` for all four metrics when no URL is set but demo is disabled (POC: only in the Thanos branch; our mock should expose the state explicitly).
- Demo series: port `demoMetrics` and `demoObservability.ts` verbatim (hash, mixer, bucketing, ranges above) so browser mock, fake backend and future backend demo mode agree sample-for-sample across overlapping ranges; Offline/Unknown clusters emit nothing; `no-data` when no Ready cluster matches the site.
- Per-cluster series: generate per cluster first, then fold to fleet (sum, mean, request-weighted mean) exactly as the POC; expose both per-cluster and fleet in the proto if the page wants per-site charts.
- Model usage: derive from fleet `requests` as the POC does, but name the series after real vLLM metrics (`model_name` label values from the catalog), and emit the fleet-cadence timestamps.
- Tenant usage: `UNAVAILABLE` in Thanos-emulation mode; demo numbers in demo mode using the POC weights so proportions vary by timeframe.
- Values: overview series round to 3 decimals, model series to 2 decimals, tenant totals to integers (as the POC does); never emit non-finite numbers; GPU ≤ 98 in demo, 0..100 after scaling in Thanos mode.
- Inventory: `clusters[]` from the ManagedCluster list with `ai-grid/site`, `ai-grid/region`, `ai-grid/site-name`, `ai-grid/latitude`, `ai-grid/longitude` conventions from the POC doc; `acm_managed_cluster_labels` can carry `ai_grid_enabled` only on the OpenShift querier and only while the label is set (the fleet endpoint never showed it in 7 days), so a metrics-only inventory cross-check is not reliable; use the ManagedCluster list.

## Open questions

1. Which endpoint does the real backend get credentials for: `rbac-query-proxy` (needs a token with `managedclusters` read access; 302 on auth failure) or `observability-thanos-query` directly (no RBAC, in-cluster only)? This decides the 401-vs-302 handling and whether per-cluster RBAC filtering applies.
2. Gateway metrics: will AI Grid traffic go through `openshift-ai-inference` (has the Istio PodMonitor) or `maas-default-gateway` (no PodMonitor today)? Until the MaaS gateway is scraped the overview's gateway request/error/latency cannot be fleet-wide. Also confirm that the gateway's istio-proxy emits `istio_requests_total` at all (0 kept samples with 392 scraped suggests either no traffic or stats disabled for the Gateway API deployment).
3. The duplicate `rhai-metrics` / `rhai-metrics-sourced` ScrapeConfigs: intentional? Both **are** active (verified 2026-10-15: unlabelled twin series exist on the fleet), so every fleet counter is doubled unless queries pin `scrape_source="rhai-metrics-sourced"`. Who owns removing `rhai-metrics`?
4. `DCGM_FI_DEV_GPU_UTIL` is divided by 100 by the RHOAI collector; is that stable (RHOAI dashboards expect a ratio) and does the GPU operator on spokes run DCGM with `-k` so `pod`/`namespace` are present for per-model GPU attribution?
5. Tenant attribution: who owns adding a `TelemetryPolicy` (user/subscription labels on Limitador counters) or deploying the Praxis metering service, and does `models-as-a-service` consider `auth.identity.selected_subscription_id` the tenant key? Will the `Limitador` CR be set to `spec.telemetry: exhaustive` so `limit_name` appears on the counters?
6. `vllm:*` vs `kserve_vllm:*`: the backend should query one name set only; the `kserve_` copies double the fleet cardinality. Is the duplicate PodMonitor (`-engine` with rename and `-engine-default` without) intentional in RHOAI?
7. `maas-api-metrics` ServiceMonitor has no `up` series anywhere; which metrics does maas-api expose (per-API-key request counts?) once the scrape works?
8. Fleet sample cadence is 300 s; is a 1h chart with 12 points acceptable for the overview, or should the backend query the OpenShift `thanos-querier` of each spoke (30 s) via the cluster-proxy for the 1h range?
9. Retention: raw 24h / 5m 7d / 1h 14d on the ACM store. Anything beyond 24h must tolerate downsampled data (`max_source_resolution=auto` works); do we need ranges beyond 24h later?
10. Should `GetOverview` keep the POC's "exactly one aggregate series" rule, or return `sum by (cluster)` so the map and drawer can show per-cluster rates without N extra queries?

## Sources

- Dev hub, observed read-only (anonymised): `oc get` on `configmap/cluster-monitoring-config`, `multiclusterobservability/observability`, `prometheusagent`, `scrapeconfig`, `opentelemetrycollector/data-science-collector`, ServiceMonitors/PodMonitors, `tokenratelimitpolicy`, `limitador`, `gateway`; Prometheus API on both query routes: `/api/v1/label/__name__/values`, `/api/v1/labels`, `/api/v1/series`, `/api/v1/query`, `/api/v1/query_range`, `/api/v1/status/buildinfo`.
- POC: `scratchpad/poc-obs/docs/grid-monitoring.md`, `backend/grid-monitoring.mjs`, `shared/grid-monitoring.mjs`, `src/services/monitoring/demoObservability.ts`.
- Praxis: `scratchpad/praxis-ai-docs/token_count.md`, `stream_usage_inject.md`, `token_usage_headers.md`, `external_metering.md`, `external-metering.yaml`, `metering_mod.rs`.
- Repo: `ai-grid-provider-console/docs/plan/bff-api.md` (Grid/GetOverview row), `ai-grid-provider-console/docs/plan/work-breakdown.md` (backend item 4 and the "which Thanos / which metric names" open question).
- Prometheus HTTP API: https://prometheus.io/docs/prometheus/latest/querying/api/
- Thanos Query: https://thanos.io/tip/components/query.md/
- ACM observability (2.13/2.14 docs; custom allowlist ConfigMap `observability-metrics-custom-allowlist`, `rbac-query-proxy` route, fine-grained RBAC): https://docs.redhat.com/en/documentation/red_hat_advanced_cluster_management_for_kubernetes/2.14/html-single/observability/index
- vLLM metrics: https://docs.vllm.ai/en/latest/usage/metrics/
- Istio standard metrics: https://istio.io/latest/docs/reference/config/metrics/
- Envoy cluster stats: https://www.envoyproxy.io/docs/envoy/latest/configuration/upstream/cluster_manager/cluster_stats
- Kuadrant metrics overview: https://docs.kuadrant.io/latest/kuadrant-operator/doc/observability/metrics/ ; TelemetryPolicy: https://docs.kuadrant.io/latest/kuadrant-operator/doc/overviews/telemetrypolicy/ and `doc/reference/telemetrypolicy.md`; token metrics guide: `doc/user-guides/observability/token-metrics.md` (Kuadrant/kuadrant-operator)
- Limitador metrics source: https://github.com/Kuadrant/limitador/blob/main/limitador-server/src/prometheus_metrics.rs
- Authorino observability: https://docs.kuadrant.io/latest/authorino/docs/user-guides/observability/
- DCGM exporter: https://docs.nvidia.com/datacenter/cloud-native/gpu-telemetry/latest/dcgm-exporter.html and https://github.com/NVIDIA/dcgm-exporter/blob/main/etc/default-counters.csv
- Gateway API Inference Extension (EPP metrics, upstream names): https://gateway-api-inference-extension.sigs.k8s.io/

## Verification (2026-10-15)

Independent read-only re-check. Re-run against the hub: `oc get` on the ScrapeConfigs, both PrometheusAgents, the `OpenTelemetryCollector` config, the PodMonitors/ServiceMonitors labelled for the RHOAI collector, the `MultiClusterObservability` spec/status, the `Limitador` and `Kuadrant` CRs, EnvoyFilter/Telemetry/WasmPlugin/TelemetryPolicy counts, the monitoring ConfigMaps, Prometheus CRs, routes and services (names only); and the Prometheus HTTP API on both query routes: `/api/v1/status/buildinfo`, `/api/v1/label/__name__/values`, `/api/v1/label/{cluster,scrape_source,finished_reason}/values`, `/api/v1/series` (limit 1-3), `/api/v1/query` (counts by `scrape_source`, `up`/`scrape_samples_*` for the gateway PodMonitor, EPP and maas-api targets), `/api/v1/query_range` (1h/300 s and 24h/`max_source_resolution=auto`), plus the exact error requests (unclosed parenthesis, `step=1` over 24h, no token, bogus token). Sources re-read: `krisv/ai-grid-cloud-provider-poc` (`backend/grid-monitoring.mjs`, `shared/grid-monitoring.mjs`, `src/services/monitoring/demoObservability.ts`), `Kuadrant/kuadrant-operator` `doc/user-guides/observability/limitador-metrics.md` and `doc/reference/telemetrypolicy.md`, `vllm/v1/metrics/loggers.py`, the Praxis filter docs in the scratchpad.

Confirmed: both endpoints (Thanos 0.41.0), name counts (4060 / 328), `cluster`+`clusterID` on every fleet series, the agent remote-write relabels, the 300 s sample cadence, the `transform/gpu_metrics` `/100` statements and the nine kept DCGM names, 96 `vllm:` + 96 `kserve_vllm:` names on both endpoints with `model_name`, absence of `ai_gateway_*`, `DCGM_*`, `envoy_*`, `auth_server_*`, `kuadrant_*`, `praxis_*`, `maas_*`, the istio PodMonitor (392 scraped / 0 kept, `up=1`), `limitador_up` and `datastore_partitioned` only on the fleet, no `authorized_*`, no TelemetryPolicy, MCO spec/status, the POC overview contract and `demoMetrics` line by line, `data.analysis`, 401 plain / 302 behaviour, the vLLM metric names.

Corrected in this pass: the 300 s cadence is the PrometheusAgent's `spec.scrapeInterval` (the ScrapeConfig sets none) (Summary 2, pipeline diagram, consequences); the `rhai-metrics` ScrapeConfig is active and unlabelled twin series exist, so every fleet query must pin `scrape_source` (Summary 2, consequences, fake-backend list, open question 3); `llm_d_epp_*` is absent on the fleet and the EPP target is `up=1` on the OpenShift querier (families table, catalog, per-model PromQL; the cause given then, the router-scheduler pod drop, was corrected by the 2026-10-16 audit); `finished_reason` includes `repetition` (label set, catalog); Limitador exposes `authorized_hits` (tokens under a TRLP) and `limit_name` needs `Limitador.spec.telemetry: exhaustive` (inventory, catalog, tenant PromQL); 13 EnvoyFilters (inventory); the DCGM scrape job is `dcgm-exporter-accelerator-metrics` (catalog, gpu PromQL; the exported `job` label is `rhoai-accelerator-metrics`, corrected by the 2026-10-16 audit); the browser `noise()` is a two-step mixer, model series round to 2 decimals, `outputPerRequest` seeds on `name+'-output'`, fleet-row guards and point count (demo transcription, fake-backend values); the ACM proxy returns the parse-error JSON with `Content-Type: text/plain` and the OpenShift querier returns the step error as JSON (Summary 9, error table, fake Thanos cases); `dedup=true` marked as the proposal's addition; RBAC paragraph and query latency marked not verified.

Not checked: `rbac-query-proxy` RBAC filtering with a restricted token; 422/503 error bodies; query latency; the `reporter` label value on Gateway API istio-proxy stats (no samples exist); the `k8s_*` resource attributes on the `:8889` exporter path; whether the POC UI suppresses `tenantUsage` in Thanos mode (the function has no mode check); maas-api metric names (no `up` series anywhere).

Audit (2026-10-16). Re-checked read-only on the hub (`oc get` on the `OpenTelemetryCollector` config, the
ServiceMonitors and the ManagedCluster; Prometheus API instant queries on both endpoints; no writes, no tokens or
hostnames recorded). Corrected: the collector's DCGM scrape config `dcgm-exporter-accelerator-metrics` relabels
`job` to `rhoai-accelerator-metrics`, so the proposed GPU query now pins that value (the earlier one would never
match); `llm_d_epp_*` is absent on the fleet because the collector's scrape of the EPP target (discovered through
ServiceMonitor `kserve-llm-isvc-scheduler`) fails, `up{job="<isvc>-epp-service"}` = 0 on the fleet, not because
router-scheduler pods are dropped; `acm_managed_cluster_labels` never carried `ai_grid_enabled` on the fleet in 7
days (only one historical `"false"` series on the OpenShift querier), `local-cluster` no longer has the
`ai-grid/enabled` label, and the metrics-only inventory cross-check is withdrawn; the label-set block now says
which lines come from the OpenShift querier. Re-confirmed: the twin `scrape_source` series (both variants present
over a range window, `[1h]` and `[24h]`, on 2026-10-16; an instant query that day returned only
`rhai-metrics-sourced`, so the unlabelled twin is intermittent and checks must use a range selector). Still not verified: the items listed under "Not checked" above.
