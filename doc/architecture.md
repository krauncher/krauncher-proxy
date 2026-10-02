# Architecture

## Project principles

- **Free and neutral.** Apache-2.0. No vendor endpoints, links, telemetry or
  dependencies in code or docs. The proxy talks only to the upstreams the
  operator configures.
- **Trust through verifiability.** The exported fields are a published
  specification (`data-model.md`); anyone can check in the code that nothing
  else leaves the process.
- **Easy to fork.** Simple code over fast code. Optimizations are added only
  when a benchmark shows a need, and each one is justified in its pull request.
  A fork should be able to read the data plane in one sitting.
- **Cheap to run.** Single static binary, no external services required; Prometheus
  and Grafana are optional consumers.

## Goals

1. Forward LLM API traffic without changing it: same bytes, same headers, same
   streaming behaviour, negligible added latency.
2. Record the workload shape of every request as numbers.
3. Export the shape in a form Grafana reads directly, and as raw records for
   offline analysis.
4. Hold serious load by scaling horizontally (stateless instances), with one
   instance comfortably covering typical deployments. Performance targets in
   `performance.md` are guidance; work beyond the simple design is driven by
   measurement.
5. Never let statistics affect traffic. If the shape pipeline falls behind,
   statistics degrade; requests do not.

## Non-goals

- Routing, load balancing, retries, caching, rate limiting, rewriting requests (one opt-in exception:
  `stream_usage: inject`, see `protocols.md`).
- Holding upstream credentials. The client's upstream key passes through in
  its usual header and lives only in memory for the duration of the request;
  the proxy never stores, logs or captures it.
- Storing or exporting prompt or answer content, in any form.
- Exact tokenization. Token counts come from what the upstream reports.
- Batch / asynchronous APIs that submit work as uploaded files and return
  results later. The proxy sees only the file upload and the job calls, as
  `generic` records; their contents are not parsed. Known limitation.
- Intercepting TLS (MITM). Clients must point their base URL at the proxy.

## Overview

```
           ┌──────────────────────── stage 1: data plane ────────────────────────┐
client ──► │ listener ─► route match ─► capture tee ─► upstream transport ──────► │ ──► upstream
client ◄── │            (in-flight ++)   (bounded)     (pooled, HTTP/1.1 + h2)    │ ◄── upstream
           │                  │                                                 │
           │                  └─ body sent / response end: Request/ResponseEvent ─► try-enqueue ─┼──┐
           └─────────────────────────────────────────────────────────────────────┘  │
                                                                                     ▼
           ┌──────────────────────── stage 2: shape pipeline ────────────────────┐
           │ event queue (bounded ring) ─► worker pool ─► dialect parser          │
           │                                    │                                │
           │                                    ▼                                │
           │                              ShapeRecord ─► aggregators (Prometheus) │
           │                                         └─► record queue ─► JSONL    │
           └─────────────────────────────────────────────────────────────────────┘
```

### Stage 1 — data plane

- One goroutine per request, as provided by `net/http`. There is no explicit
  queue in front of forwarding: queuing a request before sending it upstream
  would add latency for nothing. The bound on stage 1 is the in-flight limit
  (`limits.max_inflight`), enforced by a semaphore; `0` means unlimited.
- On arrival: assign a request ID, take monotonic and wall timestamps, increment
  the in-flight counters (global and per route) and remember their values as
  `concurrency_at_arrival`.
- Request body: streamed to the upstream through a tee that copies up to
  `capture.request_max_bytes` into a buffer. The upstream receives the
  full body regardless of the cap. When the body has been fully sent, the
  request capture is handed to stage 2 as a `RequestEvent` (non-blocking
  enqueue). Its buffer is released as soon as a worker has parsed it, not at the
  end of the response, so a stream lasting minutes does not hold the request
  bytes.
- Response: headers are copied, then the body is streamed to the client with
  immediate flushing (required for SSE). The tee keeps:
  - a **head** buffer (first `capture.response_head_bytes`) — for first-token timing;
  - a **tail** ring (last `capture.response_tail_bytes`) — for final usage blocks;
  - for non-streaming responses, the full body up to `capture.response_max_bytes`;
  - a **chunk timeline**: `(byte_offset, monotonic_ns)` for each write to the
    client, capped at `capture.timeline_max_points`; after the cap only the
    count and the last point are kept;
  - for `text/event-stream` responses, an **SSE event counter**: the number of
    event separators (`\n\n`) written, counted with `bytes.Count` per write
    (with one carried byte across write boundaries). This is the only byte
    inspection on the data plane; it feeds token estimation (`protocols.md`).
- On completion (normal end, client disconnect or upstream error) the handler
  builds a `ResponseEvent` (buffers, timeline, event counter, timings, status,
  sizes, flags) and calls a **non-blocking** enqueue. If the queue is full, the event is
  dropped, its buffers are released, and
  `shape_events_dropped_total` is incremented.
- The data plane never parses bodies. It only copies bytes and timestamps.

### Stage 2 — shape pipeline

- **Event queue**: a bounded channel (`pipeline.queue_size`) carrying two
  event kinds, both pointing to one per-request `Pending` object:
  - `RequestEvent` — enqueued when the request body is sent;
  - `ResponseEvent` — enqueued when the response ends.
- **Pending object**: created on arrival, holds the request capture until
  parsed, then the parsed `RequestShape`, and a `done` channel closed when the
  request side is resolved (parsed, failed, dropped or skipped). If the
  `RequestEvent` cannot be enqueued, the handler resolves the request side as
  `dropped` immediately and releases the buffer.
- **Worker pool**: `pipeline.workers` goroutines (default `GOMAXPROCS`).
  On a `RequestEvent` a worker:
  1. selects the dialect parser by route;
  2. parses the request capture (model, stream flag, message count, tools,
     requested max tokens, text bytes for estimation, prefix input);
  3. runs the prefix estimator if enabled;
  4. zeroes and releases the request buffer, stores `RequestShape`, closes `done`.

  On a `ResponseEvent` a worker:
  1. waits on `done` with a short bound (`pipeline.request_wait`, default
     100 ms; request parsing takes microseconds, the wait covers two events of
     one request being picked by different workers); on timeout request fields
     are null and `parse_error: request_late`;
  2. parses the response capture (usage, first content event offset, finish
     reason, error class);
  3. maps the first content event offset to a timestamp via the chunk timeline;
  4. fills token fields: reported usage, else estimate (`protocols.md`);
  5. builds a `ShapeRecord` (see `data-model.md`);
  6. zeroes and releases the response buffers — content does not outlive this
     step;
  7. updates the in-process aggregators (Prometheus collectors);
  8. non-blocking enqueue of the record to the record queue (drop + counter on
     full).
- **Record writer**: one goroutine per sink. Batches records, writes JSON Lines
  through a buffered writer, rotates files by size and age, optionally gzips
  rotated files. Flushes on interval and on shutdown.
- **Metrics endpoint**: a separate listener (`metrics.listen`, default
  `127.0.0.1:9090`) serving `/metrics`, `/healthz`, `/readyz`. Never on the
  proxy listener, so an application cannot collide with it. Optional TLS and
  bearer token (`configuration.md`).

### Prefix repetition estimator (optional, stage 2)

Measures how much of each prompt repeats a prompt seen recently, independent of
whether the upstream reports cache hits.

- The request capture is split into the canonical prompt sequence (dialect
  defines it: system + messages in order, tool definitions first if present).
- The sequence is cut into fixed blocks of `prefix.block_bytes`. Block `i` is
  hashed together with the hash of block `i-1` (chained), with a per-process
  random salt, using a fast non-cryptographic 64-bit hash.
- The longest chained run found in an in-memory LRU (`prefix.max_entries`,
  `prefix.ttl`) gives `prefix_repeat_bytes`.
- Hashes stay in process memory only, are salted per process, and are never
  exported. The salt rotates with `prefix.ttl`, which also clears the LRU.
- Off by default (`prefix.enabled: false`), because it costs CPU per request.

## Failure behaviour

| Situation | Behaviour |
|---|---|
| Event queue full (request side) | Request fields null, record still produced from the response side |
| Event queue full (response side) | No record, counter incremented, request unaffected |
| Record queue full / sink slow | Record dropped from the sink, aggregators still updated, counter incremented |
| Capture budget exhausted | Request forwarded without capture, record has `capture: skipped` |
| Body larger than capture cap | Forwarded fully, record has `capture: truncated`, fields extracted from the kept prefix by the rules in `protocols.md` |
| Unknown path / dialect | Forwarded, record with timings and sizes only (`dialect: generic`) |
| Parser error | Record kept with `parse_error` set, counter incremented, never panics the worker |
| Client disconnects mid-stream | Upstream request cancelled via context, record with `outcome: client_cancelled` |
| Upstream unreachable | 502 to the client (standard reverse-proxy behaviour), record with `outcome: upstream_error` |
| Client authentication fails | 401 from the proxy, not forwarded, record with `outcome: unauthorized` |
| Sink write fails | Error logged with rate limit, counter incremented, retries on next batch |
| Shutdown (SIGTERM) | Stop accepting, wait for in-flight up to `shutdown.grace`, drain event queue, flush sinks, exit |

Every worker recovers from panics, logs them with the request ID (never with
content), and continues.

## Global capture budget

Per-request caps bound one request; the global budget bounds the process.
`capture.budget_bytes` is a counter acquired only with a non-blocking
compare-and-swap. Memory is reserved for what is actually captured, not for the
cap:

- **Request and response head/body**: reserved in steps of
  `capture.budget_step` (default 64 KiB) as bytes arrive, up to the cap. A
  failed reservation stops that buffer for good, so what it holds is always one
  contiguous prefix (`capture: truncated`); forwarding continues.
- **Response tail**: reserved in one piece (`response_tail_bytes`) the first
  time bytes overflow the head; if that fails, no tail is kept.
- **Release**: request reservation when the `RequestEvent` is parsed (or
  dropped), response reservation when the `ResponseEvent` is processed.

Sizing example: 20,000 open streams with typical 20 KiB requests hold the
request side only until parsing (milliseconds), and 80 KiB head + tail each on
the response side: about 1.6 GiB. Peak capture memory is at most
`budget_bytes`, whatever the load.

## Client authentication

The client authenticates to the proxy separately from the upstream. Its
upstream key still travels with each request and passes through.

What it protects, given that a client without a valid upstream key gets
nothing from the upstream anyway:

- integrity of the statistics — no one outside can pollute the shape;
- proxy resources — the proxy is not an open relay;
- attribution — each record carries the authenticated client name.

On a loopback-only sidecar it adds little and may stay `off`.

Modes (`client_auth.mode`):

- `off` — no client authentication.
- `header` — the client sends a proxy token in `client_auth.header` (default
  `X-Proxy-Key`; SDKs set it via their default-headers option). The proxy
  hashes it with SHA-256 and compares with the hashes in
  `client_auth.tokens_file` in constant time. Each line of the file is
  `name:sha256hex`. The header is removed before forwarding. The token is
  never logged.
- `mtls` — the client presents a certificate in the TLS handshake, verified
  against `listen.tls.client_ca_file`. The client name is taken from the
  certificate (`client_auth.mtls.name_from`: `cn` or `san_dns`) and, if
  `client_auth.mtls.allowed_names` is set, must be in that list. No proxy
  secret travels in request headers.

Failure: `401` from the proxy, the request is not forwarded,
`llm_shape_auth_failures_total{reason}` is incremented, and a record with
`outcome: unauthorized` is written (no body is captured or parsed).

Tokens are generated by the operator; `llm-shape-proxy -hash-token` reads a
token from stdin and prints its hash for the tokens file.

## Deployment model and threat model

The proxy runs on the customer's own hardware, next to the application (sidecar
or same host). Seeing prompts, answers and credentials in transit is accepted:
the host already holds them. The design goal is that **compromising the host
where the proxy runs yields nothing beyond what the host already had**, and
that the proxy itself is not a way into the host. Measures are listed in
`privacy.md` (data) and `development.md` (build and image).

## Planned: alternative input sources

The shape record does not depend on how the data was observed. A later
milestone adds an **importer** that builds `ShapeRecord`s from logs of an LLM
gateway the customer already runs, so a customer who will not put a new
component in the request path still gets the same outputs. Each gateway format
is a separate adapter with fixtures; field mapping is verified against that
gateway's documentation, not assumed. Fields the gateway does not log stay
null.

## Design decisions

1. **No queue before forwarding.** The "first queue" is the set of in-flight
   request goroutines bounded by the in-flight semaphore. A literal queue
   between accept and forward would add latency and a failure mode without
   adding throughput.
2. **Stage 1 never parses, never blocks on stage 2.** Parsing JSON or SSE on the
   hot path would tie added latency to body size. Copying bytes is cheap and
   bounded.
3. **Timeline instead of parsing for TTFT.** The data plane records when each
   chunk was written; stage 2 later finds which chunk carried the first token.
   This gives accurate TTFT without parsing in stage 1.
4. **Token counts: reported first, then estimated, never tokenized.** No
   tokenizer dependency. Order of sources: usage reported by the upstream;
   usage requested by the proxy on routes where the operator enabled it;
   estimate from SSE event count and text bytes. The source is always recorded
   (`usage_source`), so estimated values can be filtered out. See
   `protocols.md`.
5. **Drop over backpressure.** Statistics are sampled-by-overload rather than
   allowed to slow traffic. Drops are counted, so the loss is visible.
6. **Stateless instances.** No shared state between instances. Prometheus sums
   across instances; JSONL files are per instance and carry `instance` in each
   record. Anything that depends on traffic across instances, concurrency in
   particular, is per instance in the live metrics and reconstructed exactly
   from JSONL offline (`data-model.md`, derived quantities).
7. **Standard library first.** `net/http`, `net/http/httputil` concepts,
   `encoding/json` for parsing. External dependencies: Prometheus client and a
   YAML parser. Anything else needs a reason in the pull request.
