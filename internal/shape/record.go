// SPDX-License-Identifier: Apache-2.0

// Package shape is stage 2: it turns captured requests into exported shape
// records. See doc/data-model.md.
package shape

import (
	"math"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/capture"
	"github.com/krauncher/krauncher-proxy/internal/dialect"
)

// Outcome values.
const (
	OutcomeOK              = capture.OutcomeOK
	OutcomeClientCancelled = capture.OutcomeClientCancelled
	OutcomeUpstreamError   = capture.OutcomeUpstreamError
	OutcomeProxyError      = capture.OutcomeProxyError
	OutcomeUnauthorized    = capture.OutcomeUnauthorized
)

// Capture values.
const (
	CaptureFull      = "full"
	CaptureTruncated = "truncated"
	CaptureSkipped   = "skipped"
)

// Usage sources.
const (
	UsageResponse    = dialect.UsageResponse
	UsageStreamFinal = dialect.UsageStreamFinal
	UsageNone        = dialect.UsageNone
	UsageInjected    = "injected"  // stream_final, after the proxy asked for it
	UsageEstimated   = "estimated" // computed by the proxy
)

// Record is one exported line. Field order and names follow
// doc/data-model.md; nil pointers encode as null ("not known").
type Record struct {
	V        int     `json:"v"`
	TS       string  `json:"ts"`
	ID       string  `json:"id"`
	Instance string  `json:"instance"`
	Route    string  `json:"route"`
	Client   *string `json:"client"`

	Dialect      string  `json:"dialect"`
	Precision    *string `json:"precision"`
	Engine       *string `json:"engine"`
	Endpoint     string  `json:"endpoint"`
	Model        *string `json:"model"`
	Stream       *bool   `json:"stream"`
	Status       int     `json:"status"`
	Outcome      string  `json:"outcome"`
	ErrorClass   *string `json:"error_class"`
	FinishReason *string `json:"finish_reason"`

	ReqBytes                   int64 `json:"req_bytes"`
	RespBytes                  int64 `json:"resp_bytes"`
	MessageCount               *int  `json:"message_count"`
	HasSystem                  *bool `json:"has_system"`
	ToolCount                  *int  `json:"tool_count"`
	MaxTokensRequested         *int  `json:"max_tokens_requested"`
	N                          *int  `json:"n"`
	EmbeddingInputs            *int  `json:"embedding_inputs"`
	ConcurrencyAtArrival       int64 `json:"concurrency_at_arrival"`
	ConcurrencyGlobalAtArrival int64 `json:"concurrency_global_at_arrival"`
	TextBytes                  *int  `json:"text_bytes"`
	ImageInputs                *int  `json:"image_inputs"`

	PromptTokens          *int    `json:"prompt_tokens"`
	CompletionTokens      *int    `json:"completion_tokens"`
	CachedPromptTokens    *int    `json:"cached_prompt_tokens"`
	CacheWriteTokens      *int    `json:"cache_write_tokens"`
	ReasoningTokens       *int    `json:"reasoning_tokens"`
	OutputTextBytes       *int    `json:"output_text_bytes"`
	OutputTextBytesSource *string `json:"output_text_bytes_source"`
	UsageSource           string  `json:"usage_source"`

	UploadMS  *float64 `json:"upload_ms"`
	HeadersMS *float64 `json:"headers_ms"`
	TTFTMS    *float64 `json:"ttft_ms"`
	LatencyMS float64  `json:"latency_ms"`
	DecodeMS  *float64 `json:"decode_ms"`
	DecodeTPS *float64 `json:"decode_tps"`
	Chunks    *int     `json:"chunks"`
	SSEEvents *int     `json:"sse_events"`

	PrefixRepeatBytes *int `json:"prefix_repeat_bytes"`
	PrefixTotalBytes  *int `json:"prefix_total_bytes"`

	Capture    string  `json:"capture"`
	ParseError *string `json:"parse_error"`

	// Time is the arrival wall clock; sinks format TS from it.
	Time time.Time `json:"-"`
}

// Version of the record format, written as "v".
const Version = 1

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func since(t0, t time.Time) *float64 {
	if t.IsZero() {
		return nil
	}
	v := ms(t.Sub(t0))
	return &v
}

// ms converts to milliseconds with microsecond precision.
func ms(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Microsecond)) / 1000
}
