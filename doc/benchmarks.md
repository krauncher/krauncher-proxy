# Benchmarks and measured errors

Measured values, with the date and the data they come from. Update when the
code or the fixtures change.

## Token estimation, `openai` dialect (2026-10-02)

Source: `testdata/openai/deepseek/` (DeepSeek, `deepseek-chat` and
`deepseek-reasoner`), compared with the usage the provider reported.
Estimation is only used when a response carries no usage; DeepSeek always
reports it, so these numbers describe the estimator, not DeepSeek records.

Output tokens, streams (`sse_events − overhead`):

| Fixture | Estimated | Reported | Error |
|---|---|---|---|
| chat_stream | 6 | 6 | 0% |
| chat_stream_usage | 6 | 6 | 0% |
| reasoner_stream_usage | 33 | 34 | −3% |
| chat_tools_stream_usage | 21 | 48 | −56% |

Input tokens (`(text_bytes + tool bytes) / 4 + 4 × messages`):

| Fixture | Estimated | Reported | Error |
|---|---|---|---|
| chat_length | 12 | 12 | 0% |
| chat_basic | 20 | 17 | +18% |
| chat_stream | 19 | 16 | +19% |
| chat_cache_first | 1960 | 1569 | +25% |
| chat_tools | 59 | 271 | −78% |
| reasoner_stream_usage | 12 | 40 | −70% |

Reading: plain text is within about ±25%; tool calling and reasoning add
hidden template tokens the proxy cannot see, so estimates there are lower
bounds.

## Token estimation, `openai` dialect — Ollama, Qwen3.5 9B Q8 (2026-10-02)

Source: `testdata/openai/ollama-qwen35/` (Ollama 0.33.2, `qwen3.5:9b-q8_0`, a
thinking model: with small `max_tokens` the whole output is reasoning). Here
estimation is the normal case for streams: Ollama sends usage only with
`include_usage`.

Output tokens, streams:

| Fixture | Estimated | Reported | Error |
|---|---|---|---|
| chat_stream_usage | 36 | 40 | −10% |
| chat_tools_stream_usage | 59 | 60 | −2% |
| reasoner_stream_usage | 286 | 353 | −19% |

Input tokens:

| Fixture | Estimated | Reported | Error |
|---|---|---|---|
| chat_basic | 20 | 26 | −23% |
| chat_length | 12 | 18 | −33% |
| chat_stream_usage | 19 | 25 | −24% |
| chat_cache_first | 1960 | 1578 | +24% |
| chat_tools | 59 | 273 | −78% |
| reasoner_stream_usage | 12 | 21 | −43% |

Reading: the chat template of an open model adds a fixed number of tokens per
request (short prompts are underestimated, long ones overestimated by the
4-bytes-per-token rule); tool definitions are expanded by the template far
beyond their JSON size.

## Output text bytes, derived (2026-10-02)

Head and tail cut to 2 KiB each so that the stream has a gap:

| Fixture | Derived | Actual (text + reasoning) | Error |
|---|---|---|---|
| DeepSeek reasoner_stream_usage | 111 | 111 | 0 |
| DeepSeek chat_tools_stream_usage | 215 | 69 | +146 bytes over 21 events |
| Ollama chat_stream | 157 | 187 | −16% |
| Ollama chat_tools_stream_usage | 219 | 281 | −22% |
| Ollama reasoner_stream_usage | 1012 | 1085 | −7% |

DeepSeek wraps a few bytes of text in about 300 bytes of envelope per event,
and text and tool-call events have different envelopes, so derived values are
rough for long mixed streams. With the default caps (16 KiB head, 64 KiB tail)
streams up to about 80 KiB, roughly 270 DeepSeek events, are parsed exactly.

## Output text bytes on real long streams (2026-10-02)

Source: `testdata/replay/ollama-qwen35/`, replayed through the proxy with the
default caps (16 KiB head, 64 KiB tail):

| Recording | Stream bytes | Events | Derived | Actual | Error |
|---|---|---|---|---|---|
| grid_in512_out1024 | 215 307 | 1 002 | 5 020 | 5 052 | −0.6% |
| grid_in2048_out1024 | 213 368 | 998 | 4 918 | 4 949 | −0.6% |
| grid_in8192_out1024 | 214 969 | 1 001 | 4 891 | 4 923 | −0.7% |
| long_answer | 1 005 971 | 4 688 | 21 443 | 21 659 | −1.0% |

On homogeneous text streams the derived value is within about 1%; the large
errors in the section above come from short streams cut artificially and from
mixing text and tool-call events.

## Replay set notes: Ollama, Qwen3.5 9B Q8 (2026-10-02)

- Thinking disabled with `reasoning_effort: "none"`.
- Prompt × output grid: requested ~512 / 2048 / 8192 tokens of prompt (actual
  427 / 1647 / 6507: the filler text has ~5 bytes per token) × 32 / 256 / 1024
  output tokens. Decode pace ≈ 37–39 tokens/s on the rented card.
- Each prompt size is sent three times with the same text, so Ollama's prefix
  cache serves the second and third: only the `out32` cell of each row shows a
  cold prefill (0.8 / 1.2 / 1.8 s to headers).
- Parallel batch of 4: Ollama answered them one after another (headers at
  1.2 / 7.9 / 15.9 / 23.5 s), i.e. the server ran one request at a time. The
  recording shows queueing, not concurrent decoding.

## Live check (2026-10-02)

Proxy → `api.deepseek.com`, one streaming and one plain request: both records
complete (`capture: full`, no parse errors); stream with 55 events, 52
completion tokens reported, output bytes exact, TTFT and decode rate present.
