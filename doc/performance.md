# Performance

## Targets (one instance, 4 vCPU, 8 GiB)

| Scenario | Target | Measured (M5, `benchmarks.md`) |
|---|---|---|
| Added latency to response headers, p99 | ≤ 1 ms | met up to 5,000 req/s; +1.9 ms at 10,000 req/s |
| Added latency per streamed chunk, p99 | ≤ 0.5 ms | ~0.16 µs of proxy code per chunk; the rest is syscalls |
| Non-streaming requests, small bodies (≤ 8 KiB) | ≥ 10,000 req/s with capture on | 27,600 req/s (saturated) |
| Concurrent open streams | ≥ 20,000 | 20,000 held, 0 errors |
| Aggregate streamed chunks | ≥ 200,000 chunks/s | ~45,000 chunks/s per vCPU; 167,000 at 3.8 vCPU (the test upstream was the limit) |
| Memory | Bounded by `capture.budget_bytes` + ~32 KiB per open connection | ~23 KiB per connection beyond capture |
| Stage 2 throughput | ≥ 20,000 records/s with prefix estimator off | ~30,000 records/s per vCPU (short streams) |
| Event drops at targets | 0 | 0 in every run |

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

Done after M5 measurements showed the need (GC took ~40% of CPU at
10,000 req/s, 33 GB allocated in 12 s):

- capture buffers grow with the bytes written, while the budget is still
  reserved in steps (they used to allocate a whole 64 KiB step per request);
- ReverseProxy copies through a pool of 32 KiB buffers.

Result: allocations ÷11, GC ~6% of CPU, throughput 13,500 → 27,600 req/s.

Deferred until a benchmark shows the need:

- `sync.Pool` size classes for capture buffers;
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
