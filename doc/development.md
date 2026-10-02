# Development

## Toolchain

- Go 1.24 or newer. Module path `github.com/krauncher/krauncher-proxy`. Binary
  `llm-shape-proxy`; metric prefix `llm_shape_`, environment prefix
  `LLM_SHAPE_`. Names describe the function, not the vendor.
- Dependencies allowed without discussion: `github.com/prometheus/client_golang`,
  `gopkg.in/yaml.v3`, `golang.org/x/sync` (semaphore). Anything else needs a
  reason in the pull request.

## License and contributions

- License: Apache-2.0. `LICENSE` at the repository root holds the full text;
  `NOTICE` lists the copyright holder (Ilya Sergeev). Source files carry an SPDX header:
  `// SPDX-License-Identifier: Apache-2.0`.
- Contributions under the Developer Certificate of Origin: every commit is
  signed off (`git commit -s`), checked in CI. No CLA.
- `CONTRIBUTING.md`: how to build and test, the DCO, the rules from
  `privacy.md` that every change must respect, the dependency policy.
- `SECURITY.md`: a private channel for vulnerability reports, the supported
  versions, and the expected response time. Security fixes are released with
  an advisory.
- Forks are expected and welcome. Nothing in the code may assume a specific
  vendor, endpoint or account.
- `gofmt`, `go vet`, `staticcheck` clean. Tests run with `-race` in CI.

## Build and release

- Static binary (`CGO_ENABLED=0`), reproducible build (`-trimpath`, pinned
  toolchain, `-buildvcs`), verified in CI by building twice and comparing.
- Container image: distroless/static base, no shell, runs as a fixed non-root
  UID, read-only root filesystem, writable only the JSONL directory, all Linux
  capabilities dropped. Kubernetes and compose examples set these explicitly.
- Releases and images signed; SBOM published with every release.
- Dependencies limited to the list above; `govulncheck` in CI.

## Repository layout

```
cmd/llm-shape-proxy/      main: config load, wiring, signals, shutdown
internal/config/          YAML + env parsing, defaults, validation
internal/proxy/           stage 1: handler, route match, transport, tee, timeline
internal/capture/         capture buffers, head/tail ring, capture budget
internal/pipeline/        event queue, worker pool, record queue
internal/dialect/         Dialect interface, registry
internal/dialect/openai/
internal/dialect/anthropic/
internal/dialect/generic/
internal/sse/             SSE scanner shared by dialects
internal/jsonscan/        allocation-free JSON path scanner for truncated bodies
internal/shape/           ShapeRecord, Pending/events → record assembly, prefix estimator
internal/sink/jsonl/      batching writer with rotation
internal/metrics/         Prometheus collectors, label cache, self metrics, HTTP endpoint
dashboards/               Grafana dashboard JSON
deploy/                   compose stack, Prometheus config, Grafana provisioning
tools/fakeupstream/       simulated LLM API for tests and load
tools/loadgen/            load generator
testdata/                 dialect fixtures (content replaced by placeholders)
doc/                      this documentation
```

`internal/` throughout: the project is a binary, not a library. Packages depend
downward only: `proxy` → `capture`, `pipeline`; `pipeline` → `shape`,
`dialect`, `sink`, `metrics`. `proxy` must not import `dialect`.

## Conventions

- Comments and docs in English.
- Errors wrapped with `%w`; no panics outside `main` and tests.
- Every goroutine has an owner that stops it via context on shutdown.
- Time: `time.Now()` for wall clock, monotonic durations from the same reading;
  never mix with `time.Unix` arithmetic.
- New record fields: update `data-model.md`, the JSONL example in `outputs.md`,
  and fixtures in the same pull request.

## Testing

| Level | What |
|---|---|
| Unit | Each dialect parser on fixtures: non-streaming, streaming, truncated head, truncated tail, error bodies, missing usage |
| Unit | SSE scanner: line endings, multi-line data, tail starting mid-event |
| Unit | jsonscan: truncation at every byte offset of each fixture never panics, never reports a partial count, reports scalars only when complete |
| Unit | Token estimation error against fixtures with real usage, per dialect; result recorded in `doc/benchmarks.md` |
| Unit | Sanitization: invalid model names, unknown error types, unknown finish reasons |
| Integration | `stream_usage: inject` adds the option only when absent, leaves bodies above the cap unmodified |
| Integration | Client auth: `header` accepts valid, rejects missing/invalid with 401 and never forwards; the token header never reaches the upstream; the upstream key does; `mtls` rejects unknown CA and names outside the allowlist |
| Unit | Capture: caps, head/tail ring correctness, budget acquire/release, released buffers are zeroed |
| Integration | Proxy + `fakeupstream` via `httptest`: bytes forwarded identically (hash compare), headers preserved, streaming flush timing, client cancel, upstream error, records match expectations |
| Property | Forwarded body equals upstream body for random sizes and chunkings, capture on and off |
| Load | `tools/loadgen` scenarios from `performance.md` |

Fixtures: real responses captured from each supported API with all text
replaced by placeholders of the same length class. Never commit real content.

## Milestones

| | Scope | Done when |
|---|---|---|
| M1 | Pass-through proxy, routes, transport tuning, timings, in-flight counters, generic records to JSONL | Integration tests pass; bytes identical; streams flush |
| M2 | Capture (caps, budget, timeline, SSE event counter), pipeline (request/response events, workers, drop counters), jsonscan, OpenAI and Anthropic dialects, token estimation, `stream_usage` | Fixture tests pass for all endpoints in `protocols.md`; estimation error recorded |
| M3 | Prometheus metrics, label cardinality control, Grafana dashboard, compose stack | Dashboard shows all rows against `fakeupstream` |
| M4 | Prefix repetition estimator | Correct on synthetic shared-prefix workloads; CPU cost measured |
| M5 | Measured performance work | Simple implementation measured against `performance.md`, results in `doc/benchmarks.md`; deferred optimizations added only where the measurement falls short |
| M6 | Release hardening: image, signing, SBOM, reproducible build, strict mode | A release passes the checks in Build and release |
| M7 | Gateway log importer (first adapter) | Records from fixtures match the proxy's records for the same traffic |

## Open questions (for the project owner)

- Agent sessions: multi-step agent loops grow the context request by request.
  Without a session identifier this is visible only through prefix repetition.
  An optional, operator-configured header carrying a session ID is a
  candidate: the ID is used in process only and exported as a per-process
  sequential number, never raw and never hashed. Needs a privacy review first.
- Whether a container image is published, and where.
