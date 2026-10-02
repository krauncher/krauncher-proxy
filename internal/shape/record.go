// SPDX-License-Identifier: Apache-2.0

// Package shape defines the exported per-request shape record and the event
// the data plane hands to the pipeline. See doc/data-model.md.
package shape

import (
	"math"
	"time"
)

// Outcome values.
const (
	OutcomeOK              = "ok"
	OutcomeClientCancelled = "client_cancelled"
	OutcomeUpstreamError   = "upstream_error"
	OutcomeProxyError      = "proxy_error"
	OutcomeUnauthorized    = "unauthorized"
)

// Capture values.
const (
	CaptureFull      = "full"
	CaptureTruncated = "truncated"
	CaptureSkipped   = "skipped"
)

// Usage sources.
const (
	UsageResponse    = "response"
	UsageStreamFinal = "stream_final"
	UsageInjected    = "injected"
	UsageEstimated   = "estimated"
	UsageNone        = "none"
)

// Endpoint values.
const EndpointOther = "other"

// Event is what the data plane knows about one request when it ends. Times
// carry Go's monotonic reading, so differences are immune to clock steps.
// A zero time means "did not happen".
type Event struct {
	ID        string
	Route     string
	Dialect   string
	Precision string
	Engine    string
	Client    string

	T0      time.Time // arrival
	ReqEnd  time.Time // request body fully read by the upstream transport
	Headers time.Time // upstream response headers received
	End     time.Time // last byte written to the client, or abort

	InflightRoute  int64
	InflightGlobal int64
	ReqBytes       int64
	RespBytes      int64
	Status         int
	Outcome        string
}

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

// Build turns an event into a record with everything the data plane alone can
// tell; body-derived fields stay null until a dialect parser fills them.
func Build(e Event, instance string) Record {
	return Record{
		V:                          Version,
		ID:                         e.ID,
		Instance:                   instance,
		Route:                      e.Route,
		Client:                     optional(e.Client),
		Dialect:                    e.Dialect,
		Precision:                  optional(e.Precision),
		Engine:                     optional(e.Engine),
		Endpoint:                   EndpointOther,
		Status:                     e.Status,
		Outcome:                    e.Outcome,
		ReqBytes:                   e.ReqBytes,
		RespBytes:                  e.RespBytes,
		ConcurrencyAtArrival:       e.InflightRoute,
		ConcurrencyGlobalAtArrival: e.InflightGlobal,
		UsageSource:                UsageNone,
		UploadMS:                   since(e.T0, e.ReqEnd),
		HeadersMS:                  since(e.T0, e.Headers),
		LatencyMS:                  ms(e.End.Sub(e.T0)),
		Capture:                    CaptureSkipped,
		Time:                       e.T0,
	}
}

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
