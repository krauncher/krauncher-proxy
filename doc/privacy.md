# Privacy

The proxy sits in the request path, so it necessarily sees prompts, answers and
credentials in transit. The design keeps everything it sees inside the process
for as short a time as possible and exports only numbers.

## Threat model

The proxy runs on the customer's hardware. Whoever has root on that host can
already read the application's traffic and memory, so the proxy cannot protect
content from them. What it guarantees instead:

- it leaves **nothing content-bearing on disk**;
- content stays in memory **briefly and in few copies**;
- it holds **no secrets of its own** beyond its TLS key;
- it is **not a channel out**: no outbound connections except to configured
  upstreams;
- it is **hard to use as a way in**: minimal image, no shell, no root
  (`development.md`).

## What is seen

Full request and response bytes, all headers including credentials.

## What is kept in memory, and for how long

| Data | Lifetime |
|---|---|
| Request capture buffer | Until the request side is parsed (normally milliseconds after the body is sent); then zeroed and released |
| Response capture buffers | Until the response side is parsed; then zeroed and released |
| Prefix block hashes (optional) | Salted, in an in-memory LRU, at most `prefix.ttl`; salt rotates with TTL |
| Headers | Not captured at all |

Limit, stated plainly: Go does not allow guaranteed erasure of every copy of
the data. Capture buffers are zeroed, but internal buffers of `net/http` and
`crypto/tls` are reused, not wiped. The design minimizes copies and lifetime;
it does not promise erasure.

## What is exported

Only the fields in `data-model.md`: sizes, counts, timings, token numbers,
enums, the sanitized model name, proxy-generated IDs. The model name is the
only string taken from traffic; it is length- and charset-limited, and
anything outside the rules is exported as `invalid`. No content, no hashes of content,
no headers, no client IP, no upstream request IDs, no query strings.

## Rules for contributors

1. No field derived from content may be added to `ShapeRecord` except counts
   and sizes. A proposal for any other derived field needs a privacy review in
   the pull request.
2. Logs and error messages never include bodies, header values or query
   strings. Parser errors are short codes.
3. Capture buffers are never written to disk, never sent over the network,
   never included in panic output.
4. Debug features that would expose content (e.g. dumping a capture) are not
   implemented, even behind a flag.
5. Parsers and `jsonscan` do not copy content strings; they measure them in
   place.
6. The process sets `PR_SET_DUMPABLE=0` and `RLIMIT_CORE=0` at startup: no core
   dumps, no ptrace attach by other processes of the same user.
7. No outbound connections except to route upstreams: no telemetry to the
   project, no update checks, no remote configuration.
8. No new secret may be introduced into the configuration file; secrets are
   referenced by path.

## Deployment requirements (operator)

- No swap, or encrypted swap: otherwise capture buffers may be paged to disk.
- JSONL disabled unless needed; when enabled, the data directory is on an
  encrypted volume and readable only by the proxy user.
- The metrics endpoint stays on loopback, or uses TLS with a bearer token or
  mTLS.
- `security.strict: true` for enterprise installations.
