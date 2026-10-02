// SPDX-License-Identifier: Apache-2.0

package shape

import (
	"bytes"
	"compress/gzip"
	"io"
	"math"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/capture"
	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/dialect"
	"github.com/krauncher/krauncher-proxy/internal/dialect/openai"
)

// Dialects by configuration name.
var Dialects = map[string]dialect.Dialect{
	config.DialectOpenAI:  openai.Dialect{},
	config.DialectGeneric: dialect.Generic{},
}

// Job is one unit of stage-2 work: the request side or the response side of
// a Pending request.
type Job struct {
	P        *capture.Pending
	Response bool
}

// Assembler parses captured requests and emits records.
type Assembler struct {
	Instance    string
	Estimate    config.Estimate
	RequestWait time.Duration
	MaxBody     int // decompression cap for compressed bodies
	Out         func(Record)
}

func dialectOf(p *capture.Pending) dialect.Dialect {
	if d, ok := Dialects[p.Dialect]; ok {
		return d
	}
	return dialect.Generic{}
}

// Handle runs one job. It never panics on input; buffers are released here.
func (a *Assembler) Handle(j Job) {
	if j.Response {
		a.response(j.P)
	} else {
		a.request(j.P)
	}
}

func (a *Assembler) request(p *capture.Pending) {
	if p.Req == nil {
		p.ResolveRequest(nil)
		return
	}
	d := dialectOf(p)
	r := d.ParseRequest(d.Endpoint(p.Path), p.Req.Bytes(), p.Req.Truncated() || p.ReqIncomplete)
	p.Req.Release()
	p.ResolveRequest(&r)
}

func (a *Assembler) response(p *capture.Pending) {
	d := dialectOf(p)
	ep := d.Endpoint(p.Path)
	var req dialect.Request
	late := false
	if v, ok := p.WaitRequest(a.RequestWait); !ok {
		late = true
	} else if r, _ := v.(*dialect.Request); r != nil {
		req = *r
	}

	resp := dialect.Response{UsageSource: dialect.UsageNone, FirstContentEnd: -1}
	if p.Capture && p.Head != nil {
		rc := dialect.ResponseCapture{
			Status: p.Status, Stream: p.Stream,
			Body: p.Head.Bytes(), Truncated: p.Head.Truncated() || p.TailFailed,
			Total: p.RespBytes, SSEEvents: p.SSE.Events(),
		}
		if p.Tail != nil {
			rc.Tail, rc.TailSkipped = p.Tail.Bytes()
		}
		switch enc := p.ContentEncoding; {
		case enc == "" || enc == "identity":
			resp = d.ParseResponse(ep, rc)
		case enc == "gzip" && !p.Stream:
			if body, ok := rc.Complete(); ok {
				if plain, err := gunzip(body, a.MaxBody); err == nil {
					rc.Body, rc.Tail, rc.Truncated = plain, nil, false
					resp = d.ParseResponse(ep, rc)
				} else {
					resp.ParseError = "decompress"
				}
			} else {
				resp.ParseError = "response_truncated"
			}
		default:
			resp.ParseError = "unsupported_encoding"
		}
	}
	rec := a.build(p, ep, req, resp)
	p.ReleaseResponse()
	if late {
		rec.ParseError = optional("request_late")
	}
	a.Out(rec)
}

func gunzip(b []byte, limit int) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(zr, int64(limit)))
}

func (a *Assembler) build(p *capture.Pending, ep dialect.Endpoint, req dialect.Request, resp dialect.Response) Record {
	r := Record{
		V:                          Version,
		ID:                         p.ID,
		Instance:                   a.Instance,
		Route:                      p.Route,
		Client:                     optional(p.Client),
		Dialect:                    p.Dialect,
		Precision:                  optional(p.Precision),
		Engine:                     optional(p.Engine),
		Endpoint:                   string(ep),
		Status:                     p.Status,
		Outcome:                    p.Outcome,
		ErrorClass:                 resp.ErrorClass,
		FinishReason:               resp.FinishReason,
		ReqBytes:                   p.ReqBytes,
		RespBytes:                  p.RespBytes,
		MessageCount:               req.MessageCount,
		HasSystem:                  req.HasSystem,
		ToolCount:                  req.ToolCount,
		MaxTokensRequested:         req.MaxTokens,
		N:                          req.N,
		EmbeddingInputs:            req.EmbeddingInputs,
		ConcurrencyAtArrival:       p.InflightRoute,
		ConcurrencyGlobalAtArrival: p.InflightGlobal,
		TextBytes:                  req.TextBytes,
		ImageInputs:                req.ImageInputs,
		PromptTokens:               resp.PromptTokens,
		CompletionTokens:           resp.CompletionTokens,
		CachedPromptTokens:         resp.CachedPromptTokens,
		CacheWriteTokens:           resp.CacheWriteTokens,
		ReasoningTokens:            resp.ReasoningTokens,
		OutputTextBytes:            resp.OutputTextBytes,
		OutputTextBytesSource:      optional(resp.OutputTextBytesSource),
		UsageSource:                resp.UsageSource,
		UploadMS:                   since(p.T0, p.ReqEnd),
		HeadersMS:                  since(p.T0, p.Headers),
		LatencyMS:                  ms(p.End.Sub(p.T0)),
		Capture:                    captureState(p, req),
		Time:                       p.T0,
	}
	r.Model = req.Model
	if r.Model == nil {
		r.Model = resp.Model
	}
	r.Stream = req.Stream
	if r.Stream == nil && p.Stream {
		t := true
		r.Stream = &t
	}
	if r.UsageSource == UsageStreamFinal && p.Injected {
		r.UsageSource = UsageInjected
	}
	if p.Stream {
		chunks, events := p.TimelineTotal, p.SSE.Events()
		r.Chunks, r.SSEEvents = &chunks, &events
	}
	if r.UsageSource == UsageNone && p.StreamUsage != config.StreamUsageOff && p.Capture && p.Status < 400 {
		a.estimate(&r, p, req, resp)
	}
	a.timing(&r, p, resp)
	if pe := firstNonEmpty(req.ParseError, resp.ParseError); pe != "" {
		r.ParseError = &pe
	}
	return r
}

// estimate fills token counts when the upstream reported none
// (doc/protocols.md, Token estimation).
func (a *Assembler) estimate(r *Record, p *capture.Pending, req dialect.Request, resp dialect.Response) {
	var completion, prompt *int
	switch {
	case p.Stream && resp.FirstContentEnd >= 0:
		n := max(p.SSE.Events()-resp.Overhead, 0)
		completion = &n
	case !p.Stream && resp.OutputTextBytes != nil:
		n := int(math.Round(float64(*resp.OutputTextBytes) / a.Estimate.BytesPerToken))
		completion = &n
	}
	if req.TextBytes != nil && (req.ImageInputs == nil || *req.ImageInputs == 0) {
		msgs := 0
		if req.MessageCount != nil {
			msgs = *req.MessageCount
		}
		// Tool definitions are part of the prompt the model reads.
		n := int(math.Round(float64(*req.TextBytes+req.ToolBytes)/a.Estimate.BytesPerToken)) + msgs*a.Estimate.TokensPerMessage
		prompt = &n
	}
	if completion == nil && prompt == nil {
		return
	}
	r.PromptTokens, r.CompletionTokens = prompt, completion
	r.CachedPromptTokens, r.CacheWriteTokens, r.ReasoningTokens = nil, nil, nil
	r.UsageSource = UsageEstimated
}

func (a *Assembler) timing(r *Record, p *capture.Pending, resp dialect.Response) {
	if !p.Stream {
		return
	}
	var first time.Time
	switch {
	case resp.FirstContentEnd > 0:
		first, _ = p.TimeAt(resp.FirstContentEnd)
	case p.Dialect == config.DialectGeneric && len(p.Timeline) > 0:
		first = p.Timeline[0].T // generic: the first chunk approximates TTFT
	}
	if first.IsZero() {
		return
	}
	ttft := ms(first.Sub(p.T0))
	decode := ms(p.End.Sub(first))
	r.TTFTMS, r.DecodeMS = &ttft, &decode
	if r.CompletionTokens != nil && decode > 0 {
		tps := math.Round(float64(*r.CompletionTokens)/decode*1000*10) / 10
		r.DecodeTPS = &tps
	}
}

func captureState(p *capture.Pending, req dialect.Request) string {
	switch {
	case !p.Capture:
		return CaptureSkipped
	case req.Truncated || p.TailFailed || (p.Head != nil && p.Head.Truncated() && p.Tail == nil):
		return CaptureTruncated
	case p.Tail != nil:
		if _, skipped := p.Tail.Bytes(); skipped > 0 {
			return CaptureTruncated
		}
	}
	return CaptureFull
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
