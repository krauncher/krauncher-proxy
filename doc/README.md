# krauncher-proxy — design documentation

krauncher-proxy is a transparent reverse proxy placed between an application and
an LLM HTTP API. It forwards traffic unchanged and records the **workload
shape**: how large requests and answers are, how fast they arrive, how many run
at once, how long they take, how much of the prompt repeats. It never stores
prompt or answer content.

The output is meant for Grafana (Prometheus metrics) and for offline analysis
(per-request records in JSON Lines).

The project is a free, neutral collector licensed under Apache-2.0. It is meant
to be trusted by third parties and used as a base for their forks; it is not
tied to any vendor's service. See Project principles in
[architecture.md](architecture.md).

## Documents

| File | Contents |
|---|---|
| [architecture.md](architecture.md) | Goals, non-goals, components, the two stages, failure behaviour, design decisions |
| [data-model.md](data-model.md) | The per-request shape record: every field, its unit and how it is derived |
| [protocols.md](protocols.md) | Supported API dialects and how each is parsed (plain JSON and streaming) |
| [outputs.md](outputs.md) | Prometheus metrics, JSONL records, Grafana dashboard |
| [configuration.md](configuration.md) | Configuration file, flags, defaults |
| [performance.md](performance.md) | Load targets, memory bounds, benchmarks and load tests |
| [privacy.md](privacy.md) | What is seen, what is kept, what never leaves the process |
| [development.md](development.md) | Repository layout, conventions, testing, milestones, open questions |
| [benchmarks.md](benchmarks.md) | Measured estimation errors and performance numbers |
| [implementation-log.md](implementation-log.md) | Implementation status and dated log of progress and decisions |

## Glossary

- **Upstream** — the LLM API the proxy forwards to.
- **Route** — a mapping from an incoming path prefix to one upstream.
- **Dialect** — the API format of a route (OpenAI-compatible, Anthropic-compatible, generic).
- **Data plane (stage 1)** — accepting, forwarding and streaming requests and responses.
- **Shape pipeline (stage 2)** — parsing captured bytes into shape records and writing statistics.
- **Capture** — the bounded copy of request/response bytes taken on the data plane for stage 2.
- **Shape record** — one row per proxied request; numbers only, no content.
- **TTFT** — time to first token: from request arrival to the first content-bearing response event.
