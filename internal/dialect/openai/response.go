// SPDX-License-Identifier: Apache-2.0

package openai

import (
	"bytes"
	"strings"

	"github.com/krauncher/krauncher-proxy/internal/dialect"
	"github.com/krauncher/krauncher-proxy/internal/jsonscan"
	"github.com/krauncher/krauncher-proxy/internal/sse"
)

// docVisitor reads one JSON document: a whole response or one stream chunk.
// Usage may sit at the top level or under "response" (responses API events).
type docVisitor struct {
	model    *string
	usage    usage
	hasUsage bool
	finish   string
	errType  string
	errCode  string

	text      int // generated text + tool-call arguments
	reasoning int // reasoning text
	toolCall  bool
	status    string // responses API: response.status
}

type usage struct {
	prompt, completion, cached, reasoning *int
}

// under strips an optional leading "response" segment (responses API events
// wrap the response object).
func under(p jsonscan.Path) jsonscan.Path {
	if len(p) > 0 && p[0].Key != nil && string(p[0].Key) == "response" {
		return p[1:]
	}
	return p
}

func (v *docVisitor) Start(p jsonscan.Path, k jsonscan.Kind, _ int) {
	if k == jsonscan.Object && (p.Match("choices", "*", "delta", "tool_calls", "*") || p.Match("choices", "*", "message", "tool_calls", "*")) {
		v.toolCall = true
	}
}

func (v *docVisitor) End(jsonscan.Path, jsonscan.Kind, int, int, int) {}

func (v *docVisitor) Value(full jsonscan.Path, k jsonscan.Kind, raw []byte, _ int) {
	p := under(full)
	switch k {
	case jsonscan.Number:
		n := intOf(raw)
		switch {
		case p.Match("usage", "prompt_tokens"), p.Match("usage", "input_tokens"):
			v.usage.prompt, v.hasUsage = n, true
		case p.Match("usage", "completion_tokens"), p.Match("usage", "output_tokens"):
			v.usage.completion, v.hasUsage = n, true
		case p.Match("usage", "prompt_tokens_details", "cached_tokens"), p.Match("usage", "input_tokens_details", "cached_tokens"):
			v.usage.cached = n
		case p.Match("usage", "prompt_cache_hit_tokens"): // DeepSeek; same value as cached_tokens when both exist
			if v.usage.cached == nil {
				v.usage.cached = n
			}
		case p.Match("usage", "completion_tokens_details", "reasoning_tokens"), p.Match("usage", "output_tokens_details", "reasoning_tokens"):
			v.usage.reasoning = n
		}
	case jsonscan.String:
		switch {
		case p.Match("model"):
			if s, ok := jsonscan.Unquote(raw); ok {
				s = dialect.SanitizeName(s)
				v.model = &s
			}
		case p.Match("choices", "*", "finish_reason"), p.Match("incomplete_details", "reason"):
			v.finish, _ = jsonscan.Unquote(raw)
		case p.Match("status"):
			v.status, _ = jsonscan.Unquote(raw)
		case p.Match("error", "type"):
			v.errType, _ = jsonscan.Unquote(raw)
		case p.Match("error", "code"):
			v.errCode, _ = jsonscan.Unquote(raw)
		case p.Match("choices", "*", "message", "content"), p.Match("choices", "*", "delta", "content"),
			p.Match("choices", "*", "text"),
			p.Match("choices", "*", "message", "tool_calls", "*", "function", "arguments"),
			p.Match("choices", "*", "delta", "tool_calls", "*", "function", "arguments"),
			p.Match("output", "*", "content", "*", "text"),
			full.Match("delta") && len(full) == 1: // responses API delta events
			v.text += jsonscan.DecodedLen(raw)
		case p.Match("choices", "*", "message", "reasoning_content"), p.Match("choices", "*", "delta", "reasoning_content"),
			p.Match("choices", "*", "message", "reasoning"), p.Match("choices", "*", "delta", "reasoning"): // Ollama: "reasoning"
			v.reasoning += jsonscan.DecodedLen(raw)
		}
	}
}

// carriesOutput reports whether a chunk carries generated tokens.
func (v *docVisitor) carriesOutput() bool { return v.text > 0 || v.reasoning > 0 || v.toolCall }

func (Dialect) ParseResponse(ep dialect.Endpoint, rc dialect.ResponseCapture) dialect.Response {
	r := dialect.Response{UsageSource: dialect.UsageNone, FirstContentEnd: -1}
	if rc.Status >= 400 {
		parseError(&r, rc)
		return r
	}
	if rc.Stream {
		parseStream(&r, rc)
	} else {
		parsePlain(&r, rc)
	}
	return r
}

func setUsage(r *dialect.Response, u usage, source string) {
	r.PromptTokens, r.CompletionTokens = u.prompt, u.completion
	r.CachedPromptTokens, r.ReasoningTokens = u.cached, u.reasoning
	r.UsageSource = source
}

func setFinish(r *dialect.Response, finish, status string) {
	if finish == "" && status == "completed" {
		finish = status
	}
	if finish != "" {
		f := dialect.NormalizeFinish(finish)
		r.FinishReason = &f
	}
}

func parsePlain(r *dialect.Response, rc dialect.ResponseCapture) {
	if body, ok := rc.Complete(); ok {
		var v docVisitor
		if _, err := jsonscan.Scan(body, &v); err != nil {
			r.ParseError = "response_syntax"
			return
		}
		r.Model = v.model
		if v.hasUsage {
			setUsage(r, v.usage, dialect.UsageResponse)
		}
		setFinish(r, v.finish, v.status)
		out := v.text
		r.OutputTextBytes, r.OutputTextBytesSource = &out, "exact"
		return
	}
	// Oversized body: the usage object is near the end; scan it from the tail.
	r.ParseError = "response_truncated"
	if u, ok := usageFromTail(rc.Tail); ok {
		setUsage(r, u, dialect.UsageResponse)
		r.ParseError = ""
	}
}

// usageFromTail finds the last "usage" object in a tail buffer.
func usageFromTail(tail []byte) (usage, bool) {
	i := bytes.LastIndex(tail, []byte(`"usage"`))
	if i < 0 {
		return usage{}, false
	}
	j := bytes.IndexByte(tail[i:], '{')
	if j < 0 {
		return usage{}, false
	}
	var v docVisitor
	obj := tail[i+j:]
	// Wrap as {"usage": ...} so the visitor's paths apply.
	doc := append(append([]byte(`{"usage":`), obj...), '}')
	jsonscan.Scan(doc, &v)
	return v.usage, v.hasUsage
}

func scanEvents(buf []byte, partial bool, fn func(ev sse.Event, v *docVisitor)) {
	sse.Each(buf, partial, func(ev sse.Event) bool {
		var v docVisitor
		if !bytes.Equal(bytes.TrimSpace(ev.Data), []byte("[DONE]")) {
			jsonscan.Scan(ev.Data, &v)
		}
		fn(ev, &v)
		return true
	})
}

func parseStream(r *dialect.Response, rc dialect.ResponseCapture) {
	var (
		u        usage
		hasUsage bool
		finish   string
		status   string
		model    *string
	)
	take := func(v *docVisitor) {
		if model == nil {
			model = v.model
		}
		if v.hasUsage {
			u, hasUsage = v.usage, true
		}
		if v.finish != "" {
			finish = v.finish
		}
		if v.status != "" {
			status = v.status
		}
	}

	if all, ok := rc.Complete(); ok {
		// The whole stream was captured: everything is exact.
		text := 0
		scanEvents(all, false, func(ev sse.Event, v *docVisitor) {
			take(v)
			if v.carriesOutput() {
				if r.FirstContentEnd < 0 {
					r.FirstContentEnd = int64(ev.End)
				}
			} else {
				r.Overhead++
			}
			text += v.text
		})
		r.OutputTextBytes, r.OutputTextBytesSource = &text, "exact"
	} else {
		// Head: first content event. Tail: final usage, finish reason.
		// Both: the mean per-event envelope of content events (bytes that are
		// not generated text) and the non-content events. The gap is assumed to
		// hold content events with that mean envelope.
		var envelope, contentEvents, fixed int
		scanEvents(rc.Body, false, func(ev sse.Event, v *docVisitor) {
			take(v)
			span := ev.End - ev.Start
			if v.carriesOutput() {
				if r.FirstContentEnd < 0 {
					r.FirstContentEnd = int64(ev.End)
				}
				envelope += span - v.text - v.reasoning
				contentEvents++
			} else {
				r.Overhead++
				fixed += span
			}
		})
		scanEvents(rc.Tail, true, func(ev sse.Event, v *docVisitor) {
			take(v)
			span := ev.End - ev.Start
			if v.carriesOutput() {
				envelope += span - v.text - v.reasoning
				contentEvents++
			} else {
				r.Overhead++
				fixed += span
			}
		})
		if contentEvents > 0 {
			mean := float64(envelope) / float64(contentEvents)
			n := rc.SSEEvents - r.Overhead
			derived := int(float64(rc.Total) - float64(n)*mean - float64(fixed))
			derived = max(derived, 0)
			r.OutputTextBytes, r.OutputTextBytesSource = &derived, "derived"
		}
		if r.FirstContentEnd < 0 {
			r.ParseError = "head_overflow"
		}
	}
	r.Model = model
	if hasUsage {
		setUsage(r, u, dialect.UsageStreamFinal)
	}
	setFinish(r, finish, status)
}

func parseError(r *dialect.Response, rc dialect.ResponseCapture) {
	class := dialect.ClassifyStatus(rc.Status)
	body := rc.Body
	if b, ok := rc.Complete(); ok {
		body = b
	}
	var v docVisitor
	jsonscan.Scan(body, &v)
	if c := classify(v.errType, v.errCode); c != "" {
		class = c
	}
	r.ErrorClass = &class
	r.Model = v.model
}

// classify maps OpenAI-style error type/code to the closed set; "" if unknown.
func classify(typ, code string) string {
	for _, s := range []string{code, typ} {
		switch {
		case s == "":
		case strings.Contains(s, "context_length"):
			return dialect.ErrContextLength
		case strings.Contains(s, "rate_limit"), s == "insufficient_quota":
			return dialect.ErrRateLimit
		case strings.Contains(s, "authentication"), s == "invalid_api_key":
			return dialect.ErrAuthentication
		case strings.Contains(s, "permission"):
			return dialect.ErrPermission
		case strings.Contains(s, "not_found"), s == "model_not_found":
			return dialect.ErrNotFound
		case strings.Contains(s, "overloaded"):
			return dialect.ErrOverloaded
		case s == "server_error", s == "api_error", s == "internal_error":
			return dialect.ErrServer
		case s == "timeout":
			return dialect.ErrTimeout
		}
	}
	// "invalid_request_error" covers many causes; the HTTP status is more
	// specific, so it is left to ClassifyStatus.
	return ""
}
