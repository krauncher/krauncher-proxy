# Data model

## Pending, RequestEvent, ResponseEvent (internal, stage 1 → stage 2)

One `Pending` object per request; `RequestEvent` and `ResponseEvent` both point
to it. The table lists the fields across the three.

Never serialized, never leaves the process.

| Field | Type | Notes |
|---|---|---|
| `id` | string | Request ID assigned on arrival |
| `route` | string | Route name from configuration |
| `method`, `path` | string | Path without query string |
| `t_wall` | time | Wall clock at arrival (UTC) |
| `t0` | monotonic ns | Arrival |
| `t_req_end` | monotonic ns | Request body fully sent upstream |
| `t_headers` | monotonic ns | Upstream response headers received |
| `t_end` | monotonic ns | Last response byte written to client, or abort |
| `inflight_global`, `inflight_route` | int | Counters at arrival, including this request |
| `req_bytes`, `resp_bytes` | int64 | Full sizes, independent of capture caps |
| `req_capture` | buffer | Up to `request_max_bytes`; released after request parsing |
| `req_shape` | RequestShape | Set by the worker that parsed the request side |
| `req_done` | channel | Closed when the request side is resolved |
| `resp_body` | buffer | Full body up to `response_max_bytes` (non-streaming) |
| `resp_head`, `resp_tail` | buffers | Head and tail (streaming or oversized) |
| `timeline` | []point | `(offset, monotonic_ns)` per write, capped |
| `timeline_points_total` | int | Number of writes, including those beyond the cap |
| `sse_events` | int | Event separators written (SSE responses only) |
| `status` | int | HTTP status sent to client |
| `content_type` | string | Upstream response content type |
| `outcome` | enum | `ok`, `client_cancelled`, `upstream_error`, `proxy_error`, `unauthorized` |
| `client` | string | Authenticated client name, empty if `client_auth.mode: off` |
| `capture` | enum | `full`, `truncated`, `skipped` |

## ShapeRecord (exported)

One record per proxied request. All fields are numbers, enums, or identifiers
the proxy generated itself, plus the model name. Null means "not known", never zero.

### Identity and context

| Field | Type | Unit | Derivation |
|---|---|---|---|
| `ts` | string | RFC 3339, UTC | `t_wall`, truncated to `sink.jsonl.time_resolution` (default 1 ms) |
| `id` | string | — | Proxy-generated, random; not the upstream ID |
| `instance` | string | — | `instance.name` from config, default hostname |
| `route` | string | — | Route name |
| `client` | string\|null | — | Name from the tokens file or the client certificate; null if authentication is off. Same sanitization as `model` |
| `dialect` | enum | — | `openai`, `anthropic`, `generic` |
| `precision` | string\|null | — | Route config, operator-declared; null if empty |
| `engine` | string\|null | — | Route config, operator-declared; null if empty |
| `endpoint` | enum | — | Normalized operation: `chat`, `completion`, `responses`, `messages`, `embeddings`, `other` |
| `model` | string\|null | — | Request `model` field; if absent, response `model`. Sanitized: see below |
| `stream` | bool\|null | — | Request `stream` flag |
| `status` | int | — | HTTP status |
| `outcome` | enum | — | From the capture event |
| `error_class` | enum\|null | — | Non-2xx only, mapped to a closed set: see below |
| `finish_reason` | string\|null | — | Normalized: `stop`, `length`, `tool_use`, `content_filter`, `other` |

### Request shape

| Field | Type | Unit | Derivation |
|---|---|---|---|
| `req_bytes` | int | bytes | Full request body size |
| `resp_bytes` | int | bytes | Full response body size sent to the client |
| `message_count` | int\|null | — | Number of messages / input items |
| `has_system` | bool\|null | — | System prompt present |
| `tool_count` | int\|null | — | Number of tool definitions |
| `max_tokens_requested` | int\|null | tokens | `max_tokens` / `max_completion_tokens` / `max_output_tokens` |
| `n` | int\|null | — | Number of choices requested, when the dialect has it |
| `embedding_inputs` | int\|null | — | Number of inputs for embeddings |
| `concurrency_at_arrival` | int | requests | `inflight_route`, **this instance only** |
| `concurrency_global_at_arrival` | int | requests | `inflight_global`, this instance only |
| `text_bytes` | int\|null | bytes | Sum of text content lengths in the prompt (decoded strings), for estimation; null if truncated before the end of messages |
| `image_inputs` | int\|null | — | Number of non-text input parts (images, audio, files); not included in estimates |

### Token usage (as reported by the upstream)

| Field | Type | Unit | Derivation |
|---|---|---|---|
| `prompt_tokens` | int\|null | tokens | Input tokens, including cached ones |
| `completion_tokens` | int\|null | tokens | Output tokens, including reasoning |
| `cached_prompt_tokens` | int\|null | tokens | Input tokens served from the upstream prefix cache |
| `cache_write_tokens` | int\|null | tokens | Input tokens written to cache, where reported |
| `reasoning_tokens` | int\|null | tokens | Where reported |
| `output_text_bytes` | int\|null | bytes | Decoded length of generated text plus tool-call arguments, excluding reasoning text. Needed to convert the shape between tokenizers of different models |
| `output_text_bytes_source` | enum\|null | — | `exact` (non-streaming, full body captured) or `derived` (streaming, see below) |
| `usage_source` | enum | — | `response`, `stream_final`, `injected`, `estimated`, `none` |

Normalization rule: `prompt_tokens` always means *total* input. Dialects that
report cached input separately (e.g. as a separate counter not included in the
input total) are summed so that `cached_prompt_tokens ≤ prompt_tokens` holds.
The rule per dialect is in `protocols.md`.

`injected` means the proxy requested usage on behalf of the client (route
option `stream_usage: inject`); the numbers are reported by the upstream and as
exact as `stream_final`. `estimated` means `prompt_tokens` and
`completion_tokens` are computed by the proxy (`protocols.md`, Token
estimation); `cached_prompt_tokens`, `cache_write_tokens` and
`reasoning_tokens` are always null for estimated records. `none` means no
usage and no estimate (non-text output, estimation disabled, or parse failure).

### Timing

| Field | Type | Unit | Derivation |
|---|---|---|---|
| `upload_ms` | float | ms | `t_req_end − t0` |
| `headers_ms` | float | ms | `t_headers − t0` |
| `ttft_ms` | float\|null | ms | Timestamp of the timeline point covering the first content-bearing event, `− t0`. Streaming only |
| `latency_ms` | float | ms | `t_end − t0` |
| `decode_ms` | float\|null | ms | `t_end − (t0 + ttft_ms)`; streaming only |
| `decode_tps` | float\|null | tokens/s | `completion_tokens / decode_ms × 1000`, if both known and `decode_ms > 0` |
| `chunks` | int\|null | — | `timeline_points_total`, streaming only |
| `sse_events` | int\|null | — | SSE events in the response, streaming only |

For non-streaming responses `ttft_ms` is null: the first byte arrives with the
whole answer, so it says nothing about first-token time.

### Prefix repetition (optional)

| Field | Type | Unit | Derivation |
|---|---|---|---|
| `prefix_repeat_bytes` | int\|null | bytes | Longest chained block match, see architecture |
| `prefix_total_bytes` | int\|null | bytes | Canonical prompt sequence size |

### Capture quality

| Field | Type | Notes |
|---|---|---|
| `capture` | enum | `full`, `truncated`, `skipped` |
| `parse_error` | string\|null | Short parser error code, never content |

### Output text bytes in streams

When the whole stream fits head + tail, every event is parsed and the value
is exact. Otherwise the data plane kept only the head and tail, and the value
is derived:

`output_text_bytes ≈ resp_bytes − sse_events × envelope_bytes − fixed_bytes`

where `envelope_bytes` is the mean per-event overhead (SSE framing + JSON
envelope − decoded text) measured on the complete content events in the head
and tail of the same response, and `fixed_bytes` covers the non-content events
seen there. Limits:

- reasoning text in the gap cannot be told apart from answer text, so a
  derived value includes it;
- the error is the envelope variance times the number of events in the gap.
  Where the envelope is large relative to the text per event (DeepSeek: about
  300 bytes around a few bytes of text) a derived value is rough; prefer exact
  values (short responses) when computing bytes per token;
- JSON escaping of non-ASCII text makes it an overestimate for such text.

Measured errors are in `benchmarks.md`. Token counts are never derived from
this when usage is reported.

### Sanitization of strings from clients and upstreams

All exported strings are either proxy-generated, from configuration, from a
closed enum, or `model`. Nothing else from a request or response body is
exported as a string.

- `model`: at most 128 bytes, characters `[A-Za-z0-9._:/@+-]`. A value that
  fails either rule is exported as `invalid` (the original is not logged).
- `error_class`: mapped from the dialect's error type and the HTTP status to
  `rate_limit`, `overloaded`, `invalid_request`, `context_length`,
  `authentication`, `permission`, `not_found`, `timeout`, `server_error`,
  `other`. Unknown upstream types map to `other`; the mapping table lives in
  each dialect package with its tests.
- `finish_reason`: closed enum, unknown values map to `other`.

## Derived quantities (not stored, computed by consumers)

- **Concurrency (canonical)**: number of records whose interval
  `[ts, ts + latency_ms]` contains a given instant, over all instances' JSONL
  files of a route. Exact across instances, unlike `concurrency_at_arrival`,
  which counts only the instance that served the request. Requires
  NTP-synchronized instance clocks; skew adds directly to the error.
- Request rate: count of records per time bucket.
- Inter-arrival time: difference of consecutive `ts` within a route.
- Input/output ratio: `prompt_tokens / completion_tokens`.
- Cache hit share: `cached_prompt_tokens / prompt_tokens`.
- Prefix repetition share: `prefix_repeat_bytes / prefix_total_bytes`.

## Versioning

Each JSONL record carries `"v": 1`. Adding nullable fields does not change the
version. Renaming, removing, or changing units of a field increments it.
