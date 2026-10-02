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
| M3 | Prometheus metrics, shape cell, dashboard, compose, cheap security measures | not started |
| M2b | `anthropic` dialect | not started |
| M4 | Prefix repetition estimator | not started |
| M5 | Measured performance work | not started |
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
