# krauncher-proxy

Open logging proxy for LLM API traffic. It forwards requests and responses
unchanged and records the workload shape of the traffic, not its content.
Statistics are exported in a Grafana-compatible form.

What it records per request: prompt and output tokens (reported by the API, or
estimated), time to first token, latency, decode rate, request and response
sizes, concurrency, cache hits, errors. Never prompts, answers or keys.

## Try it without a model

The demo replays real recordings of a local model (Qwen3.5 on Ollama, with
their original timing) through the proxy into Prometheus and Grafana:

```
docker compose -f deploy/compose.yaml up --build
```

Open Grafana at http://127.0.0.1:3000, dashboard "LLM workload shape".
Stop with `docker compose -f deploy/compose.yaml down`.

## Run it in front of an API

Build:

```
make build            # bin/llm-shape-proxy
```

Minimal configuration (`proxy.yaml`), all other keys default
(`configs/example.yaml` lists every key with its default):

```yaml
routes:
  - name: main
    prefix: /
    upstream: https://api.openai.com
    dialect: openai        # openai | generic
sink:
  jsonl:
    enabled: true          # per-request records in ./data, off by default
```

Check and start:

```
bin/llm-shape-proxy -config proxy.yaml -check
bin/llm-shape-proxy -config proxy.yaml
```

Point the client at the proxy instead of the API; its own API key passes
through unchanged:

```python
client = OpenAI(base_url="http://127.0.0.1:8080/v1", api_key="sk-...")
```

Outputs:

- Prometheus metrics on `http://127.0.0.1:9090/metrics` (loopback by default);
  the Grafana dashboard is `dashboards/shape-overview.json`.
- JSON Lines records in `./data/<instance>/` when the sink is enabled.

For anything beyond loopback, enable TLS and client authentication
(`listen.tls`, `client_auth`); `security.strict: true` enforces them.

## Documentation

`doc/` holds the design: architecture, data model, protocols, outputs,
configuration, privacy, development. Start with `doc/README.md`.

## License

Apache-2.0, see `LICENSE`.
