# Implementation log

Formal record of implementation progress. Newest entry last. Each entry: date,
what was done or decided, commit(s), open items it creates or closes.
Milestone definitions are in [development.md](development.md).

## Status

| Stage | Scope | Status |
|---|---|---|
| 0 | Skeleton: module, CI, config, `cmd/llm-shape-proxy` | not started |
| M1 | Pass-through proxy, timings, generic records, JSONL sink, fakeupstream | not started |
| M2 | Capture, pipeline events, jsonscan, SSE, `openai` dialect, estimation, `stream_usage` | not started |
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
