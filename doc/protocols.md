# Protocols and dialects

A route declares its dialect. The dialect determines how stage 2 parses the
captured bytes. The data plane is dialect-agnostic.

> Field names below describe the public API formats as the implementer must
> verify them. Before implementing a parser, check the field names against the
> provider's current API reference and add a fixture captured from a real
> response (with content replaced by placeholders). Tests run on fixtures, not
> on this table.

## Parser interface

```go
type Dialect interface {
    Name() string
    // Endpoint maps a request path to a normalized operation.
    Endpoint(path string) Endpoint
    // ParseRequest extracts request-shape fields. buf may be truncated.
    ParseRequest(ep Endpoint, buf []byte, truncated bool) (RequestShape, error)
    // ParseResponse extracts usage, finish reason, error class and the byte
    // offset of the first content-bearing event (streaming).
    ParseResponse(ep Endpoint, r ResponseCapture) (ResponseShape, error)
    // PromptSequence yields the canonical prompt parts for prefix hashing.
    PromptSequence(ep Endpoint, buf []byte, yield func(part []byte)) error
}
```

Parsers must be allocation-conscious: decode only the fields needed (struct
with only those fields, `json.RawMessage` for skipped parts) and never retain
references to the capture buffers after returning.

## OpenAI-compatible (`openai`)

Covers OpenAI and the many servers that expose the same API (self-hosted
inference servers, gateways).

| Path suffix | Endpoint |
|---|---|
| `/chat/completions` | `chat` |
| `/completions` | `completion` |
| `/responses` | `responses` |
| `/embeddings` | `embeddings` |

**Request**: `model`, `stream`, `messages` (count; `role: system` or
`developer` → `has_system`), `tools` (count), `max_tokens` /
`max_completion_tokens`, `n`; for `responses`: `input` (count when array),
`instructions` (→ `has_system`), `max_output_tokens`; for `embeddings`:
`input` (count when array, else 1).

**Response, non-streaming**: `usage.prompt_tokens`, `usage.completion_tokens`,
`usage.prompt_tokens_details.cached_tokens`,
`usage.completion_tokens_details.reasoning_tokens`, `choices[0].finish_reason`.
`responses` endpoint: `usage.input_tokens`, `usage.output_tokens`,
`usage.input_tokens_details.cached_tokens`,
`usage.output_tokens_details.reasoning_tokens`.

**Response, streaming (SSE)**: lines `data: {json}`, terminated by
`data: [DONE]`.
- First content event: first chunk whose `choices[*].delta` has non-empty
  `content`, `reasoning_content` or `tool_calls`.
- Usage: present only in a final chunk when the client asked for it
  (`stream_options.include_usage`). Most clients don't, so this is the common
  case, not the exception. What happens without it depends on the route option
  `stream_usage` (see Token usage in streams below).
- `responses` streaming uses typed events; first content = first
  `response.output_text.delta` (or other output delta); usage in
  `response.completed`.

**Errors**: body `{"error": {"type": ..., "code": ...}}` → `error_class` from
`type`, falling back to `code`, falling back to the HTTP status class.

## Anthropic-compatible (`anthropic`)

| Path suffix | Endpoint |
|---|---|
| `/messages` | `messages` |

**Request**: `model`, `stream`, `messages` (count), `system` (presence),
`tools` (count), `max_tokens`.

**Response, non-streaming**: `usage.input_tokens`, `usage.output_tokens`,
`usage.cache_read_input_tokens`, `usage.cache_creation_input_tokens`,
`stop_reason`.

Normalization: in this format the input counter excludes cached tokens, so
`prompt_tokens = input_tokens + cache_read_input_tokens + cache_creation_input_tokens`,
`cached_prompt_tokens = cache_read_input_tokens`,
`cache_write_tokens = cache_creation_input_tokens`. Verify against the current
reference before implementing.

**Response, streaming (SSE)**: `event:` + `data:` pairs.
- `message_start` carries input-side usage.
- First content event: first `content_block_delta`.
- `message_delta` carries the cumulative `usage.output_tokens` and `stop_reason`;
  take the last one in the tail buffer.

**Errors**: `{"type": "error", "error": {"type": ...}}`.

## Token usage in streams

Route option `stream_usage`, values:

- `passthrough` (default): the request is not modified. If the stream carries
  no usage, tokens are estimated (`usage_source: estimated`).
- `inject`: for `openai` `chat` / `completion` streaming requests that don't set
  `stream_options.include_usage`, the proxy sets it before forwarding. This is
  the one case where the proxy modifies a request, and it is opt-in per route.
  Consequence for the client: one extra final chunk with an empty `choices`
  array and a `usage` object. Current official SDKs handle it; hand-written
  clients that read `choices[0]` unconditionally break. The operator enables it
  only after checking the clients behind that route. The rewrite happens on the
  data plane, so it needs the whole request body: it applies only when
  `Content-Length ≤ request_max_bytes`, otherwise the request passes unmodified
  and falls back to estimation.
- `off`: no injection, no estimation; tokens null (`usage_source: none`).

Anthropic streams always carry usage, so the option has no effect there.

## Token estimation

Used when no usage is reported and `stream_usage` is not `off`. Every estimated
record has `usage_source: estimated`; metrics keep estimated and reported values
apart (`outputs.md`).

- **Output tokens** ≈ `sse_events − overhead`, where `overhead` is the number of
  non-content events the dialect emits per response (for `openai` chat: the
  role-only first chunk, the finish chunk and the `[DONE]` marker; measured per
  dialect from fixtures and fixed as a constant in the dialect package).
  Valid because servers of this kind emit about one content event per generated
  token; servers that batch several tokens per event make this an
  underestimate. Non-streaming responses without usage: output text bytes ÷
  `estimate.bytes_per_token`.
- **Input tokens** ≈ `text_bytes ÷ estimate.bytes_per_token` (default 4.0),
  plus a fixed per-message overhead (`estimate.tokens_per_message`, default 4).
  Non-text inputs (`image_inputs`) are not estimated; records with
  `image_inputs > 0` and no reported usage get `prompt_tokens: null`.
- The estimate is for distributions, not billing. Its error is measured in the
  test suite against fixtures that carry real usage, and recorded in
  `doc/benchmarks.md` per dialect.

## Parsing truncated request bodies

A request capture may end mid-document (`capture: truncated`). Parsers do not
use `json.Unmarshal` on request bodies. They use the shared scanner in
`internal/jsonscan`:

- walks the bytes once, tracks the path of the current value, and calls back
  only for whitelisted paths (`model`, `stream`, `max_tokens`, …, the elements of
  `messages`);
- does not allocate for values it skips, including large strings (base64
  images); string values are measured (decoded length) without being copied,
  except whitelisted short scalars;
- on reaching the end of the buffer inside a value, stops and reports which
  paths were completed.

Rules for fields when the capture is truncated:

- A scalar field is reported only if its value was read completely.
- `message_count`, `tool_count`, `text_bytes`, `image_inputs` are reported only
  if the enclosing array was closed within the capture; otherwise null. A
  partial count is never reported as a count.
- `stream` missing from a truncated capture: taken from the response
  `Content-Type` (`text/event-stream` → true).
- `model` missing: taken from the response, as for non-truncated requests.

Response bodies within `response_max_bytes` are complete and may use
`encoding/json` with minimal structs; oversized non-streaming responses use the
scanner on the tail buffer to find the usage object.

## Generic (`generic`)

Any route without a known dialect, and any unknown path on a known dialect.
No body parsing. The record has timings, sizes, status, outcome; for
`text/event-stream` responses also `ttft_ms` approximated as the first
timeline point, and `chunks`. `endpoint: other`.

## SSE parsing rules

- Parse the head buffer for the first content event and the tail buffer for
  final usage. The tail may start mid-event: skip to the first line boundary
  after `\n\n`.
- Accept `\n` and `\r\n` line endings.
- Multi-line `data:` fields are joined with `\n` per the SSE specification.
- If the first content event is not within the head buffer, `ttft_ms` is null
  and `parse_error: head_overflow`.
- First content event offset → timestamp: the first timeline point whose
  cumulative offset is ≥ the end offset of that event.

## Transport

- HTTP/1.1 and HTTP/2 to the upstream; HTTP/1.1 from clients, and HTTP/2 when
  the listener has TLS. Cleartext HTTP/2 (h2c) is deferred.
- Hop-by-hop headers removed per RFC 9110 (this includes
  `Proxy-Authorization`). The proxy token header (`client_auth.header`) is
  removed. Everything else passes as is, including `Authorization` and
  provider API key headers carrying the client's upstream key.
- `Accept-Encoding` passes through. Compressed response bodies are captured
  compressed; stage 2 decompresses gzip within the capture cap. Other encodings
  (br, zstd) are recorded with `parse_error: unsupported_encoding` until a need
  is shown.
  Streaming responses from LLM APIs are normally uncompressed.
- WebSocket and other upgrades: passed through as a tunnel, recorded as
  `generic` with timings only.
