# Performance

## Targets (one instance, 4 vCPU, 8 GiB)

| Scenario | Target |
|---|---|
| Added latency to response headers, p99 | ≤ 1 ms |
| Added latency per streamed chunk, p99 | ≤ 0.5 ms |
| Non-streaming requests, small bodies (≤ 8 KiB) | ≥ 10,000 req/s with capture on |
| Concurrent open streams | ≥ 20,000 |
| Aggregate streamed chunks | ≥ 200,000 chunks/s |
| Memory | Bounded by `capture.budget_bytes` + ~32 KiB per open connection |
| Stage 2 throughput | ≥ 20,000 records/s with prefix estimator off |
| Event drops at targets | 0 |

These numbers are guidance, not an acceptance bar. LLM responses take seconds,
so proxy overhead in microseconds does not change the result for the user. The
first implementation is the simple one; it is measured against these targets,
and only a measured shortfall justifies the optimizations below.

## Hot-path rules

Required from the first implementation (they cost no complexity):

1. No locks shared across requests on the data plane except atomics (in-flight
   counters) and the capture budget semaphore (`TryAcquire` only).
2. No logging per request at `info`. Per-request logs only at `debug`.
3. Flush SSE immediately (`http.ResponseController.Flush` after each upstream
   read), never buffer streams.
4. The tee writes into capture buffers only while they have room, then becomes
   a byte counter.
5. Time reads use the monotonic clock (`time.Now()` once per write).

Deferred until a benchmark shows the need:

- `sync.Pool` buffer size classes and pooled copy buffers (start with plain
  allocation; the capture budget already bounds memory);
- fixed-size timeline arrays (start with a capped slice);
- pre-resolved Prometheus label caches (start with `WithLabelValues`);
- cleartext HTTP/2 (h2c) from clients;
- brotli and zstd decompression of captured responses (start with gzip only).

## Stage 2 rules

- Workers own an event from dequeue to buffer release; no sharing.
- JSON decoding into minimal structs; SSE scanning with `bytes.IndexByte`, not
  regex.
- Prometheus observations via `WithLabelValues`; a label-set cache only if
  profiling shows it matters.

## Operating system

Documented in `deploy/README.md` when written:
- File descriptor limit ≥ 2 × expected connections.
- `net.core.somaxconn`, ephemeral port range and `tcp_tw_reuse` for high
  upstream connection churn.
- `GOMEMLIMIT` set to ~80 % of the container memory limit.

## Measurement

- **Micro-benchmarks** (`go test -bench`): tee copy, timeline append, SSE
  scanner, each dialect parser on fixtures, record encoding. Each reports
  `ns/op`, `B/op`, `allocs/op`; CI fails on allocation regressions in data-plane
  benchmarks.
- **Load generator** (`tools/loadgen`): Go program driving the proxy against
  `tools/fakeupstream`, which simulates an LLM API: configurable header delay,
  token rate, answer length distribution, SSE or JSON, usage on/off. Reports
  added latency by comparing direct vs proxied runs.
- **Soak test**: 1 hour at target load; heap and goroutine count must be flat.
- **Profiles**: `pprof` endpoints on the metrics listener, behind
  `metrics.pprof: true` (off by default).
