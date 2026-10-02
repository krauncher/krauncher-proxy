# Outputs

Two outputs from the same shape records:

1. **Prometheus metrics** — aggregated, for Grafana dashboards and alerts.
2. **JSONL records** — one line per request, for offline analysis and custom
   visualizations.

## Prometheus metrics

Served at `http://<metrics.listen>/metrics`. Prefix `llm_shape_`.

### Labels

| Label | Values | Cardinality control |
|---|---|---|
| `route` | route names | From config, fixed |
| `precision` | route `precision` or `unknown` | From config, fixed per route |
| `engine` | route `engine` or `unknown` | From config, fixed per route |
| `model` | model names | `metrics.models` allowlist, rest → `other`; without an allowlist, first `metrics.max_models` per instance (see `configuration.md`) |
| `usage` | `reported`, `estimated` | Fixed; `reported` covers `response`, `stream_final`, `injected` |
| `endpoint` | normalized endpoint | Fixed enum |
| `stream` | `true`, `false`, `unknown` | Fixed |
| `status_class` | `2xx`, `4xx`, `5xx`, `other` | Fixed |
| `outcome` | outcome enum | Fixed |

`precision` and `engine` are fixed per route, so they add no series; they are
attached to `llm_shape_cell_total` only, where they are needed to match
cells against measurements taken for a given precision and engine.

Token histograms, token counters and the shape cell carry the `usage` label,
so reported and estimated values are never mixed silently; a dashboard shows
them together or filters to `usage="reported"`.

Histograms carry `route`, `model`, `endpoint`, `stream` only — not status or
outcome — to keep series count bounded. Failed requests are excluded from
token and timing histograms and counted in `llm_shape_requests_total`.

### Shape metrics

| Metric | Type | Labels | Buckets / notes |
|---|---|---|---|
| `llm_shape_requests_total` | counter | route, model, endpoint, stream, status_class, outcome | |
| `llm_shape_prompt_tokens` | histogram | route, model, endpoint, stream, usage | 2^4 … 2^20, ×2 |
| `llm_shape_completion_tokens` | histogram | route, model, endpoint, stream, usage | 2^2 … 2^17, ×2 |
| `llm_shape_cached_prompt_tokens_total` | counter | route, model, endpoint, stream | Reported usage only |
| `llm_shape_prompt_tokens_total` | counter | route, model, endpoint, stream, usage | Cache hit share uses `usage="reported"` |
| `llm_shape_completion_tokens_total` | counter | route, model, endpoint, stream, usage | |
| `llm_shape_ttft_seconds` | histogram | route, model, endpoint, stream | 0.01 … 60, exponential |
| `llm_shape_latency_seconds` | histogram | route, model, endpoint, stream | 0.01 … 600, exponential |
| `llm_shape_decode_tokens_per_second` | histogram | route, model, endpoint, stream, usage | 1 … 2000, exponential |
| `llm_shape_cell_total` | counter | route, precision, engine, model, endpoint, usage, prompt_bucket, output_bucket | Joint prompt × output distribution, see below |
| `llm_shape_request_bytes` | histogram | route, endpoint | 2^8 … 2^24 |
| `llm_shape_response_bytes` | histogram | route, endpoint | 2^8 … 2^24 |
| `llm_shape_concurrency_at_arrival` | histogram | route | 1 … 4096, ×2; per instance |
| `llm_shape_inflight` | gauge | route | Current, per instance; `sum` across instances gives the total |
| `llm_shape_prefix_repeat_bytes_total` | counter | route, model | Optional estimator |
| `llm_shape_prefix_total_bytes_total` | counter | route, model | Optional estimator |
| `llm_shape_usage_missing_total` | counter | route, model, endpoint | Requests with `usage_source: none` (neither reported nor estimated) |

### Shape cell

The prompt and completion histograms are independent, so they cannot tell how
much traffic falls into a given pair such as "8k prompt → 128 output". The
shape cell counter keeps that pair.

- `prompt_bucket` and `output_bucket` are the upper bound of the bucket the
  request's `prompt_tokens` / `completion_tokens` fall into: a value `v` goes
  to the smallest bound `b` with `v ≤ b`; values above the last bound go to
  `inf`.
- Default bounds, powers of two:
  - prompt: `512, 1024, 2048, 4096, 8192, 16384, 32768, inf`;
  - output: `32, 64, 128, 256, 512, 1024, 2048, inf`.
  That is 64 cells per `(route, model, endpoint)`. Bounds are configurable
  (`metrics.shape_cell`); more bounds mean more series.
- Counted for successful requests with both token counts known, reported or
  estimated, split by the `usage` label. The share of requests without a cell
  is `llm_shape_usage_missing_total`.
- Estimated output counts are underestimates on servers that send several
  tokens per event, which shifts cells toward smaller output buckets. Compare
  `usage="estimated"` with `usage="reported"` on a route where both occur
  before trusting estimated cells.
- Cached tokens are not subtracted: the cell describes the request as sent.
  The cache share is reported separately.
- Embeddings: `output_bucket` is always `0`.

Traffic share per cell over a window:

```promql
sum by (prompt_bucket, output_bucket) (increase(llm_shape_cell_total{route="$route",model="$model"}[$__range]))
/ ignoring(prompt_bucket, output_bucket) group_left
sum(increase(llm_shape_cell_total{route="$route",model="$model"}[$__range]))
```

The JSONL records keep exact token counts, so finer or different grids can be
built offline without changing the proxy.

Prometheus native histograms may be enabled with `metrics.native_histograms:
true` (finer resolution, fewer series); classic buckets stay on for
compatibility.

### Self metrics

| Metric | Type | Notes |
|---|---|---|
| `llm_shape_events_dropped_total` | counter | Event queue full |
| `llm_shape_records_dropped_total{sink}` | counter | Record queue full |
| `llm_shape_capture_skipped_total{reason}` | counter | `budget`, `disabled` |
| `llm_shape_capture_truncated_total{part}` | counter | `request`, `response` |
| `llm_shape_parse_errors_total{dialect,code}` | counter | |
| `llm_shape_event_queue_length` | gauge | |
| `llm_shape_capture_budget_used_bytes` | gauge | |
| `llm_shape_worker_busy_seconds_total` | counter | Worker saturation |
| `llm_shape_sink_write_errors_total{sink}` | counter | |

Plus the standard Go and process collectors.

## JSONL records

- One `ShapeRecord` per line, fields as in `data-model.md`, nulls written as
  `null`, `"v": 1` first.
- Disabled by default (`sink.jsonl.enabled: false`). Per-request records with
  timestamps reveal usage volume and timing; they are the main artifact the
  proxy leaves on the host. Enable them where per-request analysis is needed.
- Files are created with mode `0600` in a directory with mode `0700`.
- Retention: files older than `sink.jsonl.retention` or beyond
  `sink.jsonl.max_total_bytes` (oldest first) are deleted.
- `sink.jsonl.time_resolution` truncates `ts` (e.g. `1s`, `1m`) when exact
  timing is not needed; concurrency reconstruction then loses precision.
- JSONL is meant to stay in the customer environment. What is shared outside
  is the aggregated output (Prometheus series, shape cells).
- File naming: `<dir>/<instance>/shape-YYYYMMDDTHHMMSSZ.jsonl`, rotated by
  `sink.jsonl.max_bytes` or `sink.jsonl.max_age`, whichever comes first.
  Rotated files are gzipped if `sink.jsonl.gzip: true`.
- The active file is append-only; readers should skip a partial last line.
- Writes are batched (`sink.jsonl.batch`, `sink.jsonl.flush_interval`).

Example line (wrapped for reading):

```json
{"v":1,"ts":"2026-01-15T10:00:00.000Z","id":"01J...","instance":"proxy-a",
 "route":"main","dialect":"openai","precision":"bf16","engine":"example-engine",
 "endpoint":"chat","model":"example-model",
 "stream":true,"status":200,"outcome":"ok","error_class":null,"finish_reason":"stop",
 "req_bytes":8000,"message_count":4,"has_system":true,"tool_count":0,
 "max_tokens_requested":1024,"n":null,"embedding_inputs":null,
 "concurrency_at_arrival":3,"concurrency_global_at_arrival":3,
 "text_bytes":7400,"image_inputs":0,
 "prompt_tokens":2000,"completion_tokens":150,"cached_prompt_tokens":500,
 "cache_write_tokens":null,"reasoning_tokens":null,
 "output_text_bytes":610,"output_text_bytes_source":"derived","usage_source":"stream_final",
 "upload_ms":0.5,"headers_ms":100.0,"ttft_ms":200.0,"latency_ms":2200.0,
 "decode_ms":2000.0,"decode_tps":75.0,"chunks":150,"sse_events":153,
 "prefix_repeat_bytes":null,"prefix_total_bytes":null,
 "capture":"full","parse_error":null}
```

JSONL is the contract for later tools (a visualization page, export to a
columnar store). It is not read back by the proxy.

## Grafana

`dashboards/shape-overview.json` — a provisionable dashboard on a Prometheus
datasource (`${DS_PROMETHEUS}`), variables `route`, `model`, `endpoint`.

| Row | Panels | Query basis |
|---|---|---|
| Traffic | Requests per second; in-flight (sum across instances); concurrency at arrival p50/p90/p99 (per instance) | `rate(llm_shape_requests_total)`, `llm_shape_inflight`, `histogram_quantile` |
| Size | Prompt tokens heatmap; completion tokens heatmap; input/output token ratio; shape cell table (prompt bucket × output bucket, share of requests) | histogram buckets; `rate(..._total)` ratio; `llm_shape_cell_total` |
| Latency | TTFT p50/p90/p99; latency p50/p90/p99; decode tokens/s p50 | `histogram_quantile` |
| Reuse | Upstream cache hit share; prefix repetition share | counter ratios |
| Health | Error rate by status class; estimated-usage share; usage-missing share; dropped events; queue length; capture budget | self metrics |

`deploy/compose.yaml` — local stack (proxy, Prometheus, Grafana with the
dashboard provisioned, a fake upstream) for development and demos.
