// SPDX-License-Identifier: Apache-2.0

// Package dialect defines how captured bytes of one API format become shape
// fields, and the normalizations shared by all formats. See doc/protocols.md.
package dialect

import (
	"regexp"
	"strings"
)

// Endpoint is the normalized operation.
type Endpoint string

const (
	Chat       Endpoint = "chat"
	Completion Endpoint = "completion"
	Responses  Endpoint = "responses"
	Messages   Endpoint = "messages"
	Embeddings Endpoint = "embeddings"
	Other      Endpoint = "other"
)

// Request is what the request body tells. Nil means not known.
type Request struct {
	Model           *string
	Stream          *bool
	MessageCount    *int
	HasSystem       *bool
	ToolCount       *int
	MaxTokens       *int
	N               *int
	EmbeddingInputs *int
	TextBytes       *int
	ImageInputs     *int
	ToolBytes       int  // size of the tool definitions as sent; for estimation only
	Truncated       bool // the capture was cut
	ParseError      string
}

// ResponseCapture is the captured response as stage 2 sees it.
type ResponseCapture struct {
	Status      int
	Stream      bool
	Body        []byte // non-streaming: the body up to its cap; streaming: the head
	Truncated   bool   // Body does not hold everything before Tail
	Tail        []byte // bytes after Body, last ones kept (may start mid-event)
	TailSkipped int64  // bytes between Body and Tail that were not kept
	Total       int64  // full response size
	SSEEvents   int
}

// Complete returns the whole response when Body and Tail cover it.
func (r ResponseCapture) Complete() ([]byte, bool) {
	if r.TailSkipped > 0 || (r.Truncated && r.Tail == nil) {
		return nil, false
	}
	if len(r.Tail) == 0 {
		return r.Body, true
	}
	return append(append([]byte(nil), r.Body...), r.Tail...), true
}

// How OutputTextBytes was obtained.
const (
	OutputExact   = "exact"   // every event (or the whole body) was parsed
	OutputDerived = "derived" // computed across a gap, see doc/data-model.md
)

// Usage sources a dialect can report; the record adds "injected" and
// "estimated" (package shape).
const (
	UsageResponse    = "response"
	UsageStreamFinal = "stream_final"
	UsageNone        = "none"
)

// Response is what the response tells. Nil means not known.
type Response struct {
	Model              *string
	PromptTokens       *int
	CompletionTokens   *int
	CachedPromptTokens *int
	CacheWriteTokens   *int
	ReasoningTokens    *int
	UsageSource        string
	FinishReason       *string
	ErrorClass         *string

	// FirstContentEnd is the offset just after the first event carrying
	// generated output (streaming), or -1.
	FirstContentEnd int64
	// OutputTextBytes and how they were obtained (OutputExact, OutputDerived).
	OutputTextBytes       *int
	OutputTextBytesSource string
	// Overhead is the number of SSE events that carry no generated tokens
	// (role, finish, usage-only, terminator), for output-token estimation.
	Overhead   int
	ParseError string
}

// Dialect parses one API format. Parsers must not keep references to the
// buffers after returning.
type Dialect interface {
	Name() string
	Endpoint(path string) Endpoint
	ParseRequest(ep Endpoint, buf []byte, truncated bool) Request
	ParseResponse(ep Endpoint, rc ResponseCapture) Response
}

// Generic parses nothing: timings and sizes only.
type Generic struct{}

func (Generic) Name() string                                { return "generic" }
func (Generic) Endpoint(string) Endpoint                    { return Other }
func (Generic) ParseRequest(Endpoint, []byte, bool) Request { return Request{} }
func (Generic) ParseResponse(Endpoint, ResponseCapture) Response {
	return Response{UsageSource: UsageNone, FirstContentEnd: -1}
}

// Error classes (closed set).
const (
	ErrRateLimit      = "rate_limit"
	ErrOverloaded     = "overloaded"
	ErrInvalidRequest = "invalid_request"
	ErrContextLength  = "context_length"
	ErrAuthentication = "authentication"
	ErrPermission     = "permission"
	ErrNotFound       = "not_found"
	ErrTimeout        = "timeout"
	ErrServer         = "server_error"
	ErrOther          = "other"
)

// ClassifyStatus maps an HTTP status to an error class when the body says
// nothing usable.
func ClassifyStatus(status int) string {
	switch {
	case status == 401:
		return ErrAuthentication
	case status == 403:
		return ErrPermission
	case status == 404:
		return ErrNotFound
	case status == 408 || status == 504:
		return ErrTimeout
	case status == 413:
		return ErrContextLength
	case status == 429:
		return ErrRateLimit
	case status == 503 || status == 529:
		return ErrOverloaded
	case status >= 500:
		return ErrServer
	case status >= 400:
		return ErrInvalidRequest
	}
	return ErrOther
}

// Finish reasons (closed set).
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolUse       = "tool_use"
	FinishContentFilter = "content_filter"
	FinishOther         = "other"
)

// NormalizeFinish maps provider finish reasons to the closed set.
func NormalizeFinish(s string) string {
	switch s {
	case "stop", "end_turn", "stop_sequence", "completed":
		return FinishStop
	case "length", "max_tokens", "max_output_tokens":
		return FinishLength
	case "tool_calls", "function_call", "tool_use":
		return FinishToolUse
	case "content_filter", "refusal":
		return FinishContentFilter
	}
	return FinishOther
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{1,128}$`)

// SanitizeName applies the export rule for strings taken from traffic
// (model names, client names): length and charset limited, else "invalid".
func SanitizeName(s string) string {
	if nameRe.MatchString(s) {
		return s
	}
	return "invalid"
}

// Suffix maps a path suffix to an endpoint.
type Suffix struct {
	Suffix   string
	Endpoint Endpoint
}

// MatchSuffix returns the endpoint of the first matching suffix; list longer
// suffixes first ("/chat/completions" before "/completions").
func MatchSuffix(path string, table []Suffix) Endpoint {
	path = strings.TrimSuffix(path, "/")
	for _, s := range table {
		if strings.HasSuffix(path, s.Suffix) {
			return s.Endpoint
		}
	}
	return Other
}
