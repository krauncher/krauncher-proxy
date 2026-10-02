# Implementation log

Formal record of implementation progress. Newest entry last. Each entry: date,
what was done or decided, commit(s), open items it creates or closes.
Milestone definitions are in [development.md](development.md).

## Status

| Stage | Scope | Status |
|---|---|---|
| 0 | Skeleton: module, CI, config, `cmd/llm-shape-proxy` | done |
| M1 | Pass-through proxy, timings, generic records, JSONL sink, fakeupstream | done |
| M2 | Capture, pipeline events, jsonscan, SSE, `openai` dialect, estimation, `stream_usage` | done |
| M3 | Prometheus metrics, shape cell, dashboard, compose, cheap security measures | done |
| M2b | `anthropic` dialect | not started |
| M4 | Prefix repetition estimator | not started |
| M5 | Measured performance work | done |
| M6 | Release hardening, `CONTRIBUTING.md`, `SECURITY.md` | not started |
| M7 | Gateway log importer | not started |

Order: 0 → M1 → M2 → M3, then the rest. No fixed deadline; scope is delivered
as it becomes ready.

## Entries

### 2026-10-02 — design documentation

- Design documentation in `doc/`, `LICENSE` (Apache-2.0), `NOTICE`
  (copyright Ilya Sergeev). Commit `44c829a`, pushed to `origin/main`.
- Binary name `llm-shape-proxy`; metric prefix `llm_shape_`; environment prefix
  `LLM_SHAPE_`. Commit `aa5f431`.

### 2026-10-02 — decisions before implementation

- Fixtures for the `openai` dialect are captured first from DeepSeek
  (`https://api.deepseek.com/v1`, model `deepseek-chat`). The API key is read
  from an environment variable by the capture tool and is never written to the
  repository or to configuration files.
- To verify on captured responses before writing the parser: the field names
  DeepSeek uses for cached input tokens, and whether it honours
  `stream_options.include_usage`.
- Scope is delivered by readiness, not by date.

### 2026-10-02 — client authentication

- Upstream keys are never held by the proxy: the client sends its upstream key
  with each request, and it passes through.
- The client authenticates to the proxy separately: `header` mode (proxy token
  in `X-Proxy-Key`, SHA-256 hashes in a tokens file) for simple setups, `mtls` (client certificate in the TLS handshake) for strict and
  enterprise setups. A session-based scheme that would keep upstream keys in
  proxy memory was rejected. Default `client_auth.mode` is `off` (loopback
  sidecar); strict mode requires authentication on a non-loopback listener.
- Purpose: statistics integrity, proxy resources, attribution (`client` field).
- During development the DeepSeek key and the proxy tokens live in `.env`,
  which is not tracked by git. A more secure arrangement comes later.

### 2026-10-02 — stage 0: skeleton

- `go.mod`, `internal/config` (structure, defaults, YAML with unknown-key
  errors, `LLM_SHAPE_*` overrides, validation naming every key, strict-mode
  rules), `internal/auth` (token hashing), `cmd/llm-shape-proxy` (`-config`,
  `-check`, `-version`, `-hash-token`, `slog`, signal handling).
- `configs/example.yaml` is generated from `configuration.md`; a test keeps it
  valid and equal to the code defaults.
- `Makefile` (`build`, `test`, `lint`, `check`) and GitHub Actions CI with a DCO
  check on pull requests. Commits are signed off from now on.
- Fixed in `configuration.md`: `listen.read_header_timeout` and
  `listen.idle_timeout` had been placed under `client_auth`.
- `govulncheck` found two standard-library vulnerabilities in Go 1.24.4
  (`net/url`, reachable from config validation). Decision: Go 1.26, toolchain
  pinned to go1.26.8 in `go.mod`; staticcheck v0.8.1 and govulncheck v1.8.0
  pinned in the `Makefile`. `make check` is clean.
- Size parse errors give the line and the value, not the key name: the YAML
  decoder does not expose the key to custom types. Accepted.

### 2026-10-02 — M1: pass-through proxy

- `internal/proxy`: routes by longest prefix on segment boundaries,
  `strip_prefix`, `httputil.ReverseProxy` with immediate flushing, shared
  upstream transport (TLS verification always on, no added compression),
  in-flight counters, `max_inflight` → 503, unrouted → 404 (counted).
  Timings: arrival, request body consumed, upstream headers, end. Outcomes:
  ok, client_cancelled, upstream_error (before headers → 502; mid-stream →
  connection aborted), proxy_error.
- `internal/pipeline`: generic bounded queue `Queue[T]`, non-blocking submit,
  drop counter, drain on close; reused by the JSONL sink.
- `internal/shape`: `Event` and the full `Record` (all fields of
  `data-model.md`, null until a parser fills them).
- `internal/sink/jsonl`: batching, rotation by size and age, gzip of finished
  files, retention by age and total size, modes 0700/0600, `ts` truncation.
- `internal/fakeupstream` + `tools/fakeupstream`: simulated chat completions,
  JSON and SSE, header delay, token interval, `include_usage`, error statuses.
- `main`: wiring and ordered shutdown (stop accepting, wait in-flight up to
  `shutdown.grace`, drain events, close sinks).
- Tests: bytes identical in both directions up to 1 MiB, end-to-end headers and
  client `X-Forwarded-For` untouched, hop-by-hop removed, no `Accept-Encoding`
  added, first SSE chunk delivered before the upstream finishes, client cancel
  reaches the upstream, upstream down → 502, upstream breaks mid-stream,
  routing, 404, 503, timings against fakeupstream; sink format, permissions,
  rotation without loss, retention. `make check` clean, tests stable over 5
  runs with `-race`. Manual end-to-end run with fakeupstream and JSONL done.
- Documentation: `resp_bytes` added to the record (was in the event only);
  forwarding headers, compression and environment proxy behaviour stated in
  `protocols.md`; `internal/fakeupstream` added to the layout.
- Not verified against a real provider yet; that comes with M2 fixtures.

### 2026-10-02 — M1 test coverage review

- Added: concurrency values 1…N and counters back to 0; a stalled pipeline does
  not slow traffic (events dropped and counted); TLS upstream over HTTP/2 with
  a trusted certificate, 502 with an untrusted one; upstream answering before
  reading the body (partial `req_bytes`, no upload time; exercises the body
  counter atomics under `-race`); client cancel before headers; `strip_prefix`
  with escaped paths and query; debug logs free of credentials, bodies and
  query strings; `serve` shutdown (a running stream completes and is recorded,
  grace expiry cuts and returns, listen failure cleans up).
- Strengthened: queue invariant processed + dropped = submitted under a
  Close/Submit race; exact JSONL retention bound; timing test reduced to order
  plus the header-delay lower bound.
- Fixed: `jsonl.Writer.Close` is idempotent; `serve` closes the queue and the
  sink on every exit path, including a failed listen.
- Observed while testing: Go's HTTP server notices a closed client connection
  only after the request body is consumed. An upstream that never reads the
  body does not see a cancellation from the proxy; real APIs read the body
  first, so this affects test upstreams only.
- Deferred to M2: randomized chunking property test, JSONL age-based rotation
  and retention, write-error path, trailers / 1xx / upgrades, goroutine leak
  checks, TLS listener.

### 2026-10-02 — M2: capture and the `openai` dialect

- Fixtures: `tools/capture` recorded 12 DeepSeek scenarios into
  `testdata/openai/deepseek/` (plain, length-limited, streams with and without
  `include_usage`, tool calls plain and streamed, cache miss/hit pair, three
  error types, a reasoning stream). Generated text, ids and tool arguments are
  replaced by `x` of the same escaped length; the key came from `.env`.
- Verified on fixtures: DeepSeek sends usage in the finish chunk of every
  stream, also without `include_usage`; it reports cached input both as
  `prompt_tokens_details.cached_tokens` and `prompt_cache_hit_tokens`; it
  answers `deepseek-chat` as `deepseek-flash`; stream overhead is 3 events
  (role chunk, finish chunk, `[DONE]`). Documented in `protocols.md`.
- `internal/jsonscan`: one-pass, allocation-free path scanner for possibly
  truncated JSON. Truncation at every byte of every fixture tested; fuzzed for
  2 minutes (21 M inputs) after fixing three findings (invalid UTF-8 length,
  raw control characters, top-level number at end of input).
- `internal/sse`: event scanner (tail may start mid-event) and the data-plane
  event counter; counter agrees with the scanner on all fixtures at any write
  boundaries.
- `internal/capture`: memory budget (non-blocking CAS), step-reserved buffers
  that stay contiguous, tail ring, timeline, and `Pending` with the
  request-side handoff. Replaces the M1 `shape.Event`.
- Data plane: request capture with exactly-once handoff at EOF or handler end;
  response head (SSE) or body (JSON) plus tail; SSE counter; timeline;
  `stream_usage: inject` by byte insertion (only when the body fits the cap).
  Capture only on non-generic routes.
- `internal/dialect` + `openai`: endpoints, request fields (messages,
  multimodal parts, tools, responses `input`/`instructions`, embeddings),
  response usage / finish / errors, first content event, exact output bytes
  when the stream is fully captured, derived otherwise; sanitization of model
  names; closed error-class and finish-reason sets.
- `internal/shape`: `Assembler` (request/response jobs, gzip bodies,
  estimation including tool definition bytes, TTFT from the timeline, decode
  rate, capture state, parse errors).
- Tests: dialect against independent `encoding/json` ground truth on all
  fixtures; end-to-end proxy → pipeline → record on replayed fixtures
  (complete, truncated with a gap, gzip, 401); estimation and injection against
  fakeupstream; bytes unchanged under random chunking with capture on and off
  (the M1 deferred property test); capture budget returns to 0 after every
  scenario. Live run through the proxy to `api.deepseek.com`: records complete.
- Measured estimation errors in `benchmarks.md`. Known limits: tool-calling and
  reasoning prompts carry hidden template tokens (input estimate −70…−78%);
  tool-call streams emit several tokens per event (output estimate −56%);
  derived output bytes are rough for DeepSeek's large per-event envelopes.
- Documentation: budget reservation is stepwise for all buffers (simpler than
  the earlier Content-Length rule, same memory bound); estimation and derived
  output limits; `benchmarks.md` added.
- Still open from the M1 review, moved to M3: JSONL age-based rotation and
  retention tests, write-error path, trailers / 1xx / upgrades, goroutine leak
  checks, TLS listener test. Anthropic dialect is M2b.

### 2026-10-02 — fixtures from a local open model (Ollama)

- Captured `testdata/openai/ollama-qwen35/` from Ollama 0.33.2 with
  `qwen3.5:9b-q8_0` on a rented GPU. `err_auth` dropped: Ollama does not check
  keys, the "error" was a normal answer.
- Incident: Ollama names reasoning text `reasoning`, which `tools/capture` did
  not redact, so real model reasoning text was written to the fixture files.
  Found on inspection before any commit; the redaction list now includes
  `reasoning`, all fixture files were re-redacted, and a scan for long
  non-placeholder strings shows only provider error messages and fixed fields.
- Parser: `message.reasoning` / `delta.reasoning` counted as reasoning output.
- Findings (in `protocols.md`): usage in streams only with `include_usage`
  (separate chunk), so estimation is the normal path for local Ollama streams
  and `stream_usage: inject` is the fix; no cached-token field; 404 for unknown
  models.
- Dialect tests now run over both providers with ground truth from
  `encoding/json`, including per-provider stream overhead (DeepSeek 3 events,
  Ollama 2–3). Measured errors added to `benchmarks.md`: output estimate −2…−19%,
  input estimate −23…+24% for text, −78% with tools; derived output bytes
  −7…−22%.

### 2026-10-02 — replay sets with timing

- `tools/capture`: suites (`basic`, `shapes`), timing per response
  (`.timing.json`: headers time, arrival of every body piece), parallel batches;
  redaction now also covers `reasoning`.
- Recorded `testdata/replay/ollama-qwen35/` (17 responses, 2.8 MB): prompt ×
  output grid, long answer (4 771 tokens, 1 MB stream), real tool calls plain
  and streamed, non-thinking plain answer, parallel batch of 4.
- `fakeupstream`: `LoadRecordings` + `Replay` (by `X-Fixture` or round-robin,
  timing scaled by a speed factor); binary flags `-replay`, `-speed`.
- Tests: replay reproduces bytes and pace; end-to-end replay of every recording
  through the proxy: usage matches, TTFT not before the recorded headers,
  output bytes exact on short streams and within 1% derived on long ones
  (`benchmarks.md`).
- Observed (in `benchmarks.md`): grid TTFTs after the first cell per row are
  warm (Ollama prefix cache); the parallel batch was served sequentially.

### 2026-10-02 — review pass, items 1–6

Code review of all packages; findings ranked by impact. Fixed:

1. A panic in stage 2 no longer kills the process. `pipeline.Queue` recovers
   per item and counts (`Panics`); `shape.Assembler.Handle` recovers, releases
   the capture buffers, unblocks a waiting response side and logs the request
   ID only. Test: a dialect that panics on every input — traffic unaffected,
   budget back to 0, no records.
2. Configuration refuses options whose feature does not exist yet
   (client authentication, mTLS CA, metrics TLS / bearer / pprof, prefix
   estimator, strict mode) instead of accepting them silently. The strict-mode
   rules stay and are tested; strict mode itself is refused until M3.
3. `jsonscan` limits nesting to 256 (`ErrTooDeep`); a hostile 1 MiB body of
   `[` no longer drives unbounded recursion.
4. `TestEndToEndReplay` waits for record i before sending request i+1, so
   record order cannot drift with several workers.
5. With `stream_usage: inject`, `req_bytes` excludes the inserted bytes and
   `upload_ms` ends when the client's body was read (it was ~0 before).
6. `capture.NewRing` / `NewBuffer` guard non-positive sizes.

Still open from the review (items 7–10): single source for duplicated
constants, `ServeHTTP` decomposition, naming (`Pending.Capture`), listener
collision by port. Also open: `PR_SET_DUMPABLE` / `RLIMIT_CORE` from
`privacy.md` are not set yet (M3).

### 2026-10-02 — review pass, items 7–10

7. One source per constant set: dialect-level usage sources live in
   `dialect` and `shape` aliases them, adding only `injected` / `estimated`;
   `dialect.OutputExact` / `OutputDerived` replace the string literals.
8. `proxy.Handler.ServeHTTP` split into `newPending`, `admit` / `leave` /
   `reject`, `prepareRequest` and `finish`; behaviour unchanged.
9. `Pending.Capture` renamed to `Captured`; `injectVisitor` fields grouped by
   meaning.
10. `metrics.listen` and `listen.addr` are compared by port, with an empty or
    wildcard host matching any host (`:8080` and `0.0.0.0:8080` now collide).

No behaviour change beyond item 10; all tests unchanged and passing.

### 2026-10-02 — fix: responses cut under load (request body EOF race)

- Symptom: under parallel load (`go test -race -count=4 ./...`), about one run
  in four to six had a test whose upstream stream was cut after a few
  milliseconds (`TestServeShutdownLetsStreamsFinish`,
  `TestEndToEndDeepSeekFixtures`), recorded as `upstream_error` with no usage.
- Diagnosis, from an instrumented transport and the Go sources: the upstream
  response read failed with "use of closed network connection" while the
  request context was alive; just before it, the transport's read of the
  request body failed with "http: invalid Read on closed Body". net/http
  drains and closes an unread request body when the handler writes response
  headers (`server.go`, the `fullDuplex` check). If the transport has sent all
  bytes but not yet made its final read to see EOF, that read hits the closed
  body, the transport treats it as a write error and drops the upstream
  connection.
- A production defect, not a test artefact: any streaming response could be
  cut this way under load.
- Tried `ResponseController.EnableFullDuplex`: it stops the drain but lets the
  transport read the body after the handler returned, which made net/http
  panic with "invalid concurrent Body.Read call" in another test. Reverted.
- Fix: the body wrapper knows `Content-Length`; it returns EOF together with
  the last bytes and never reads the server's body again after that. Test:
  a body that fails every read after close is never read past its length.
- Result: 0 failures in 8 × 4 full parallel runs (was about 1 in 4–6).
- Remaining exposure: request bodies without `Content-Length` (chunked), and
  an upstream that answers before reading the whole body. LLM API clients send
  a length and LLM APIs read the body first; noted for M5 load tests.
- Also fixed: `TestStalledPipelineDoesNotBlockTraffic` read the drop counter
  before the last handler had emitted; it now waits for the exact count.

### 2026-10-02 — M3: metrics, security, demo stack

- `internal/metrics`: every shape metric from `outputs.md` (requests, token
  histograms and totals with the `usage` label, TTFT, latency, decode rate,
  sizes, concurrency, shape cell, cached tokens, usage missing, capture state,
  parse errors), model label bounded by allowlist or first N, optional
  `client` label bounded by the authenticator's names. Self metrics read on
  scrape (queue, drops, stage-2 panics, worker busy time, capture memory,
  unrouted, in-flight per route, sink drops and errors, auth failures).
  Failed requests are counted but kept out of the shape histograms.
- Metrics listener: separate from the proxy, `/metrics`, `/healthz`,
  `/readyz`, optional pprof, TLS, client certificates, bearer token (compared
  in constant time).
- `internal/auth`: header tokens (SHA-256 hashes in a file, constant-time
  comparison against every entry, header removed before forwarding) and mTLS
  (chain verified by TLS, name from CN or DNS SAN, optional allowlist).
  Unauthenticated requests get 401, are not forwarded, and are recorded as
  `unauthorized`.
- Proxy listener TLS with optional client certificate verification.
- `internal/harden`: `RLIMIT_CORE` 0 and `PR_SET_DUMPABLE` 0 at startup
  (Linux; a warning elsewhere).
- Options refused at startup now: only `prefix.enabled` (M4). Strict mode
  works.
- JSONL: an active file with no records is removed on rotation and close
  instead of being kept empty.
- Tests: metrics observation, label bounds, cell buckets, metrics listener
  bearer; TLS listener with header auth, strict mode and metrics end to end;
  mTLS (allowed, outside the allowlist, no certificate); tests deferred from
  M1 — JSONL age rotation, age retention, write errors, trailers, 103 Early
  Hints, upgrade tunnel, goroutine leaks. A data race in `serve` (reading
  `TLSConfig` while `Serve` set up HTTP/2) found by `-race` and fixed. Full
  suite stable: 0 failures in 6 × 3 parallel runs.
- Demo: `tools/loadgen` (sends replay-set requests with `X-Fixture`),
  `Dockerfile` (distroless, non-root, proxy + demo tools + replay set),
  `deploy/compose.yaml` (replaying upstream, proxy, loadgen, Prometheus,
  Grafana), `dashboards/shape-overview.json` from
  `dashboards/gen/shape_overview.py`.
- Not yet verified: the compose stack has not been run (needs the base images
  pulled), so dashboard queries are untested against a live Prometheus.

### 2026-10-02 — M3 test coverage review

- Fixed (design): the metrics bearer token also guarded `/healthz` and
  `/readyz`, which would fail Kubernetes probes; it now guards `/metrics` and
  pprof only. `/readyz` answered "ok" unconditionally; it now answers 503 once
  shutdown starts.
- Fixed: upgraded connections were recorded with status 0 (ReverseProxy writes
  the 101 on the hijacked connection, bypassing the writer). Now status 101,
  outcome ok, latency = tunnel lifetime; documented in `protocols.md`.
- Fixed: repeated names or tokens in the tokens file are refused (they made
  clients indistinguishable).
- Added: dashboard ↔ code test (every `llm_shape_*` name in a panel query must
  be exported; 20 names checked); self metrics checked against induced
  conditions (drops, queue length, unrouted, auth failures, stage-2 panic,
  sink write errors, worker busy time); metrics listener TLS and client
  certificates; pprof behind the token; client name sanitization through mTLS
  end to end; upgrade record.
- `testpki`: throwaway CA and certificates shared by tests.
- Not covered by tests: PromQL syntax of the dashboard (needs a live
  Prometheus: the compose run), `native_histograms`.

### 2026-10-02 — M3 review pass (A–E)

- A. Metrics listener with client certificates: verification moved from
  "required at handshake" to "verified if presented, required per endpoint",
  so `/healthz` and `/readyz` stay reachable for probes; `/metrics` and pprof
  answer 401 without a verified certificate. Tests: probes without a
  certificate, scrape without one, certificate from a foreign CA.
- B. `client_auth.header` may not be an upstream key header (`Authorization`,
  `X-Api-Key`, `Api-Key`, `X-Goog-Api-Key`); the proxy would strip the
  client's upstream key.
- C. Shutdown order: the metrics listener now closes after the event queue is
  drained and the sink flushed, as documented; test: during drain `/readyz` is
  503 and `/metrics` still answers.
- D. Empty `precision` / `engine` labels are exported as `unknown`, as
  `outputs.md` says.
- E. Refused requests (401) are counted in metrics only; no JSONL record, so
  unauthenticated traffic cannot drive disk writes. Test: one record for three
  requests of which two were refused.
- Open: pin the Prometheus and Grafana image versions at the first compose
  run; tokens-file permission warning; loadgen pacing goroutine after cancel;
  document that token files are read at startup only.

### 2026-10-02 — open items closed

- Startup warning when a secret file (tokens file, metrics bearer token, TLS
  keys) is readable by group or others (`harden.LooseSecretFile`).
- `loadgen`: the pacing goroutine exits on cancel instead of blocking.
- `configuration.md`: secret files are read at startup only; rotation takes a
  restart.
- Still open: pin the Prometheus and Grafana image versions at the first
  compose run (needs the images pulled).

### 2026-10-02 — demo stack verified

- Built and ran `deploy/compose.yaml`: image 43.5 MB, distroless, user
  `nonroot`; process hardening applied in the container (no warning).
- Every dashboard query (26 expressions) run against the live Prometheus via
  its HTTP API: all parse; data present wherever the demo produces it (no
  cached tokens, usage-missing or auth failures by design of the demo).
  Grafana 13.2.3: datasource healthy, dashboard provisioned (22 panels).
  Visual check in a browser not done by me.
- Found and fixed: the shape-cell table used `increase()`, which cannot see the
  first increment of a new series; sparse cells read 0 and the share was NaN.
  The table now shows cumulative shares since proxy start (explained in the
  panel description).
- Found and fixed: Docker's default 10 s stop timeout would SIGKILL the proxy
  before `shutdown.grace`; `stop_grace_period: 70s`. Verified: stop with four
  streams in flight took 60 s, the longest stream (137 s recording) was cut at
  the grace period, clean exit.
- Found and fixed: records of requests cut at the grace period were dropped
  (`Server.Close` does not wait for handlers, the queue closed first). serve
  now waits up to 5 s for in-flight handlers before closing the queue; the
  grace-expiry test checks the record (failed 2 of 5 runs without the fix).
- Compose builds one image (`llm-shape-proxy:dev`) for the three services
  instead of three copies; Prometheus `v3.15.0` and Grafana `13.2.3` pinned by
  tag and digest.

### 2026-10-02 — M5: measured performance

- `tools/loadtest`: closed-loop throughput, open-loop fixed-rate latency (a
  fixed schedule; a ticker-based first version dropped ticks and under-offered
  load, caught by comparing offered and achieved rates), long-lived streams,
  chunked request bodies. Micro-benchmarks for the writer, the capturing
  reader, stage 2 and JSONL encoding.
- Measured shortfall: at 10,000 req/s the proxy added 4.9 ms p99; the profile
  showed GC at ~40% of CPU, 74% of allocations from capture buffers growing to
  a full 64 KiB step per request and 18% from ReverseProxy's per-response copy
  buffer. Fixed both (buffers grow with the data, pooled copy buffers):
  allocations ÷11, throughput 13,500 → 27,600 req/s, p99 added latency at
  10,000 req/s 4.9 → 1.9 ms.
- Streams: 20,000 concurrent streams held with 0 errors and 0 event drops;
  capacity ~45,000 chunks/s per vCPU, bound by one read + one write/flush
  syscall per chunk, which streaming requires; proxy code is ~3% of that.
- Chunked request bodies under load: no cut responses (closes the open item
  from the EOF-race fix).
- Targets in `performance.md` now show the measured values next to them; two
  are not met as written (p99 ≤ 1 ms at 10,000 req/s; 200,000 chunks/s on
  4 vCPU) and are explained there and in `benchmarks.md`.
