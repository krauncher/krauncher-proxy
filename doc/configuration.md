# Configuration

One YAML file, path given by `-config`. Every key can be overridden by an
environment variable `LLM_SHAPE_<SECTION>_<KEY>` (upper case, dots → `_`). Flags
exist only for `-config`, `-version`, `-check` (validate config and exit) and
`-hash-token` (read a token from stdin, print its SHA-256 for the tokens file).

Invalid configuration fails at startup with a message naming the key. There is
no hot reload; restart to apply changes.

## Full example with defaults

```yaml
instance:
  name: ""                      # default: hostname

listen:
  addr: ":8080"
  tls:
    cert_file: ""               # PEM; empty = plain HTTP
    key_file: ""                # PEM, mode 0600
    client_ca_file: ""          # PEM; required for client_auth.mode: mtls

client_auth:
  mode: off                     # off | header | mtls
  header: X-Proxy-Key           # mode header
  tokens_file: ""               # mode header; lines "name:sha256hex", mode 0600
  mtls:
    name_from: cn               # cn | san_dns
    allowed_names: []           # empty = any certificate signed by the CA
  read_header_timeout: 10s
  idle_timeout: 120s
  # no read/write timeouts: streams may last minutes

routes:                         # matched by longest prefix
  - name: main
    prefix: /                   # incoming path prefix
    upstream: https://api.example.com
    strip_prefix: false
    dialect: openai             # openai | anthropic | generic
    precision: ""               # optional, operator-declared, e.g. bf16, fp8, int4
    engine: ""                  # optional, operator-declared inference engine name
    stream_usage: passthrough   # passthrough | inject | off  (see protocols.md)

upstream:
  max_idle_conns: 4096
  max_idle_conns_per_host: 1024
  max_conns_per_host: 0         # 0 = unlimited
  idle_conn_timeout: 90s
  dial_timeout: 5s
  tls_handshake_timeout: 5s
  response_header_timeout: 0s   # 0 = none; long prompts can take minutes before headers
  force_http2: true

limits:
  max_inflight: 0               # 0 = unlimited; above → 503 from proxy

capture:
  enabled: true
  request_max_bytes: 1MiB
  response_max_bytes: 1MiB      # non-streaming full body
  response_head_bytes: 16KiB
  response_tail_bytes: 64KiB
  timeline_max_points: 256
  budget_bytes: 2GiB            # global cap on capture memory
  budget_step: 64KiB            # reservation step when Content-Length is unknown

pipeline:
  queue_size: 65536
  workers: 0                    # 0 = GOMAXPROCS
  request_wait: 100ms           # response side waits this long for the request side

estimate:
  bytes_per_token: 4.0
  tokens_per_message: 4

prefix:
  enabled: false
  block_bytes: 1024
  max_entries: 1000000
  ttl: 1h

metrics:
  listen: "127.0.0.1:9090"      # loopback by default
  tls:
    cert_file: ""
    key_file: ""
    client_ca_file: ""          # set to require mTLS
  bearer_token_file: ""         # set to require Authorization: Bearer
  pprof: false                  # rejected in strict mode
  models: []                    # allowlist of model label values; others → other
  max_models: 50                # used only when models is empty
  native_histograms: false
  client_label: false           # add client label to requests_total and shape cell
  shape_cell:
    prompt_bounds: [512, 1024, 2048, 4096, 8192, 16384, 32768]   # + inf implied
    output_bounds: [32, 64, 128, 256, 512, 1024, 2048]           # + inf implied

sink:
  jsonl:
    enabled: false              # per-request records are opt-in
    dir: ./data                 # created 0700, files 0600
    retention: 168h             # delete files older than this
    max_total_bytes: 10GiB      # delete oldest beyond this
    time_resolution: 1ms        # truncate ts, e.g. 1s or 1m
    max_bytes: 256MiB
    max_age: 1h
    gzip: true
    batch: 1024
    flush_interval: 1s
    queue_size: 65536

security:
  strict: false                 # see Rules

log:
  level: info                   # debug | info | warn | error
  format: json                  # json | text

shutdown:
  grace: 60s
```

## Rules

- There is no option to disable TLS certificate verification towards
  upstreams, and none will be added. Private CAs are supported via the system
  trust store.
- The configuration file contains no secrets. TLS keys, the client tokens
  file and the metrics bearer token are referenced by file path. The tokens
  file holds only hashes.
- There is no option to store upstream keys in the proxy. They come from the
  client with each request.
- `security.strict: true` refuses to start unless: `listen.tls` is set,
  `metrics.pprof` is false, no route has
  `stream_usage: inject`, `metrics.listen` is loopback or has TLS, and
  `client_auth.mode` is not `off` when `listen.addr` is not loopback.
  Intended for enterprise deployments.

- Sizes accept `B`, `KiB`, `MiB`, `GiB`; durations use Go syntax.
- Route prefixes must be unique. A path that matches no route gets 404 from
  the proxy and is counted, not forwarded.
- `listen.addr` and `metrics.listen` must differ.
- `precision` and `engine` are free-form labels describing what serves the
  upstream. The proxy cannot observe them and does not verify them. Allowed
  characters: `[a-z0-9._-]`, up to 32 characters. Empty means unknown.
- `metrics.shape_cell` bounds must be positive and strictly increasing.
- `metrics.models`: when set, only these values appear in the `model` label,
  everything else is `other`, identically on every instance. When empty, the
  first `max_models` distinct values seen by each instance are used; this
  depends on traffic order and differs between instances, so sums across
  instances may put the same model in `other` on one instance and not on
  another. Set the allowlist for any multi-instance deployment.
  JSONL records always carry the sanitized model name, not the label.
- Logs never contain request or response bodies, headers' values, or query
  strings. Paths are logged without query strings.
