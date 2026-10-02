# AGENTS.md

Guidance for coding agents working on this repository.

## What this is

`llm-shape-proxy`: a transparent reverse proxy for LLM APIs that records the
workload shape (sizes, tokens, timings, concurrency), never content. Go 1.26,
module `github.com/krauncher/krauncher-proxy`. Design in `doc/`; read
`doc/architecture.md` and `doc/privacy.md` before changing behaviour.

## Commands

```
make check     # gofmt, go vet, staticcheck, govulncheck, go test -race — must pass
make build     # bin/llm-shape-proxy
go test -race -count=3 ./...                      # flakiness check before committing
go run ./tools/fakeupstream -replay testdata/replay/ollama-qwen35   # model-free upstream
docker compose -f deploy/compose.yaml up --build  # demo stack with Grafana
```

## Layout

- `cmd/llm-shape-proxy` — wiring (`serve.go`), CLI.
- `internal/proxy` — stage 1, the data plane: forwards and measures, never
  parses bodies, never blocks on stage 2.
- `internal/capture` — bounded buffers, memory budget, `Pending`.
- `internal/shape` — stage 2: assembles records. `internal/dialect/*` — API
  formats. `internal/jsonscan`, `internal/sse` — parsers for truncated input.
- `internal/metrics`, `internal/sink/jsonl` — outputs. `internal/auth`,
  `internal/harden` — security.
- `testdata/openai/*` — fixtures for parser tests; `testdata/replay/*` —
  recordings with timing for replay.
- `dashboards/gen/shape_overview.py` generates `dashboards/shape-overview.json`.

## Rules

- **Privacy first.** No prompt, answer, header value, key or query string in
  records, metrics, logs or error messages. Records carry numbers, enums,
  proxy-made IDs and sanitized names only. A new record field derived from
  content other than a count or size needs a privacy review.
- **Stage 1 stays cheap.** No body parsing, no blocking on stage 2, no
  per-request logging at info level.
- **Traffic is never changed** except where documented (`stream_usage:
  inject`, removal of the proxy's own token header).
- **No feature without its tests.** Configuration options whose behaviour does
  not exist must be refused at startup (`rejectUnimplemented`).
- **Dependencies:** standard library, `prometheus/client_golang`, `yaml.v3`.
  Anything else needs a stated reason.
- **Docs move with code.** Update `doc/` (field tables, config keys, metric
  names) in the same change, and add a dated entry to
  `doc/implementation-log.md`. Measured numbers go to `doc/benchmarks.md`.
- **Dashboard:** edit the generator, not the JSON; `TestDashboardMetricsExist`
  checks every metric a panel uses.
- **Fixtures:** record with `tools/capture` (it redacts generated text, ids and
  arguments to `x` of the same length); inspect the files before committing.
  Never commit real content or keys; keys live in `.env` (git-ignored).
- **Commits** are signed off (`git commit -s`, DCO). English for code,
  comments and docs.
