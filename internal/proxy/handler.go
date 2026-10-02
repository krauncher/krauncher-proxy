// SPDX-License-Identifier: Apache-2.0

// Package proxy is the data plane (stage 1): it matches routes, forwards
// requests and responses unchanged, measures sizes and timings, and copies
// bounded parts of bodies for stage 2. It never parses bodies and never blocks
// on the shape pipeline.
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/capture"
	"github.com/krauncher/krauncher-proxy/internal/config"
)

// Emit hands one side of a request to stage 2: the request side when its
// body has been read, the response side when the response has ended. It must
// not block; false means the work was dropped.
type Emit func(p *capture.Pending, response bool) bool

// Options configure the handler.
type Options struct {
	Routes    []config.Route
	Limits    config.Limits
	Capture   config.Capture
	Transport http.RoundTripper
	Emit      Emit
	Log       *slog.Logger
}

// Handler is the proxy's http.Handler.
type Handler struct {
	routes   []*route // longest prefix first
	inflight atomic.Int64
	sem      chan struct{} // nil = unlimited
	capture  config.Capture
	budget   *capture.Budget
	emit     Emit
	log      *slog.Logger

	notFound atomic.Uint64
}

type route struct {
	cfg      config.Route
	inflight atomic.Int64
	proxy    *httputil.ReverseProxy
}

// forwardingHeaders are restored after ReverseProxy strips them, so the
// upstream sees the client's request as sent.
var forwardingHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

// New builds the handler. Routes must already be validated.
func New(o Options) (*Handler, error) {
	h := &Handler{emit: o.Emit, log: o.Log, capture: o.Capture, budget: capture.NewBudget(int64(o.Capture.BudgetBytes))}
	if o.Limits.MaxInflight > 0 {
		h.sem = make(chan struct{}, o.Limits.MaxInflight)
	}
	for _, rc := range o.Routes {
		target, err := url.Parse(rc.Upstream)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", rc.Name, err)
		}
		r := &route{cfg: rc}
		r.proxy = &httputil.ReverseProxy{
			Rewrite:        rewrite(rc, target),
			Transport:      o.Transport,
			FlushInterval:  -1, // never buffer: LLM responses are streams
			ModifyResponse: onHeaders,
			ErrorHandler:   h.onError,
			ErrorLog:       slog.NewLogLogger(o.Log.Handler(), slog.LevelDebug),
		}
		h.routes = append(h.routes, r)
	}
	slices.SortStableFunc(h.routes, func(a, b *route) int { return len(b.cfg.Prefix) - len(a.cfg.Prefix) })
	return h, nil
}

func rewrite(rc config.Route, target *url.URL) func(*httputil.ProxyRequest) {
	return func(pr *httputil.ProxyRequest) {
		if rc.StripPrefix && rc.Prefix != "/" {
			p := strings.TrimPrefix(pr.Out.URL.EscapedPath(), strings.TrimSuffix(rc.Prefix, "/"))
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			pr.Out.URL.RawPath = p
			pr.Out.URL.Path, _ = url.PathUnescape(p)
		}
		pr.SetURL(target)
		for _, k := range forwardingHeaders {
			if v, ok := pr.In.Header[k]; ok {
				pr.Out.Header[k] = v
			}
		}
	}
}

// NotFound returns the number of requests that matched no route.
func (h *Handler) NotFound() uint64 { return h.notFound.Load() }

// BudgetUsed returns the capture memory currently reserved.
func (h *Handler) BudgetUsed() int64 { return h.budget.Used() }

func (h *Handler) match(path string) *route {
	for _, r := range h.routes {
		p := r.cfg.Prefix
		if strings.HasPrefix(path, p) && (len(path) == len(p) || strings.HasSuffix(p, "/") || path[len(p)] == '/') {
			return r
		}
	}
	return nil
}

// state travels in the request context to the ReverseProxy callbacks.
type state struct {
	p             *capture.Pending
	upstreamError bool
}

type stateKey struct{}

func stateOf(ctx context.Context) *state {
	s, _ := ctx.Value(stateKey{}).(*state)
	return s
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	rt := h.match(r.URL.Path)
	if rt == nil {
		h.notFound.Add(1)
		http.NotFound(w, r)
		return
	}
	p := capture.NewPending()
	p.ID, p.Route, p.Dialect = newID(), rt.cfg.Name, rt.cfg.Dialect
	p.Precision, p.Engine, p.StreamUsage = rt.cfg.Precision, rt.cfg.Engine, rt.cfg.StreamUsage
	p.Path, p.T0 = r.URL.Path, t0
	if h.sem != nil {
		select {
		case h.sem <- struct{}{}:
			defer func() { <-h.sem }()
		default:
			http.Error(w, "proxy at capacity", http.StatusServiceUnavailable)
			p.End, p.Status, p.Outcome = time.Now(), http.StatusServiceUnavailable, capture.OutcomeProxyError
			p.ResolveRequest(nil)
			h.emit(p, true)
			return
		}
	}
	p.InflightGlobal = h.inflight.Add(1)
	defer h.inflight.Add(-1)
	p.InflightRoute = rt.inflight.Add(1)
	defer rt.inflight.Add(-1)

	// Capture only where a dialect can use it; generic routes get timings,
	// sizes and the SSE counter only.
	p.Capture = h.capture.Enabled && rt.cfg.Dialect != config.DialectGeneric
	if p.Capture && rt.cfg.StreamUsage == config.StreamUsageInject {
		h.maybeInject(r, p, rt.cfg)
	}

	var body *countingBody
	if r.Body == nil || r.Body == http.NoBody {
		p.ReqEnd = t0
		p.ResolveRequest(nil)
	} else {
		body = &countingBody{ReadCloser: r.Body, p: p, emit: h.emit}
		if p.Capture {
			body.buf = capture.NewBuffer(int(h.capture.RequestMaxBytes), int(h.capture.BudgetStep), h.budget)
		}
		r.Body = body
	}
	st := &state{p: p}
	cw := &countingWriter{ResponseWriter: w, h: h, p: p}

	// ReverseProxy aborts a broken stream by panicking with
	// http.ErrAbortHandler; the response side is still emitted, then the
	// panic goes on to the server, which closes the connection.
	defer func() {
		rec := recover()
		p.End = time.Now()
		p.Status = cw.status
		if body != nil {
			body.finish(false)
			p.ReqBytes = body.n.Load()
			if t := body.end.Load(); t != nil {
				p.ReqEnd = *t
			}
		}
		switch {
		case r.Context().Err() != nil:
			p.Outcome = capture.OutcomeClientCancelled
		case st.upstreamError || rec != nil:
			p.Outcome = capture.OutcomeUpstreamError
		default:
			p.Outcome = capture.OutcomeOK
		}
		if !h.emit(p, true) {
			p.ReleaseResponse()
		}
		if rec != nil {
			panic(rec)
		}
	}()
	rt.proxy.ServeHTTP(cw, r.WithContext(context.WithValue(r.Context(), stateKey{}, st)))
}

func onHeaders(res *http.Response) error {
	if st := stateOf(res.Request.Context()); st != nil {
		st.p.Headers = time.Now()
	}
	return nil
}

func (h *Handler) onError(w http.ResponseWriter, r *http.Request, err error) {
	st := stateOf(r.Context())
	if r.Context().Err() != nil {
		return // the client is gone; nothing to answer
	}
	if st != nil {
		st.upstreamError = true
		h.log.Debug("upstream error", "route", st.p.Route, "id", st.p.ID, "err", err)
	}
	w.WriteHeader(http.StatusBadGateway)
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// countingBody measures and captures the request body. The transport reads it
// from its own goroutine, possibly after ServeHTTP has returned; the mutex
// guards the capture buffer, the atomics the measurements.
type countingBody struct {
	io.ReadCloser
	p    *capture.Pending
	emit Emit
	buf  *capture.Buffer // nil = no capture

	n   atomic.Int64
	end atomic.Pointer[time.Time]

	mu     sync.Mutex
	closed bool // the request side was handed over
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	if n > 0 && b.buf != nil {
		b.mu.Lock()
		if !b.closed {
			b.buf.Write(p[:n])
		}
		b.mu.Unlock()
	}
	if err == io.EOF {
		if b.end.Load() == nil {
			t := time.Now()
			b.end.CompareAndSwap(nil, &t)
		}
		b.finish(true)
	}
	return n, err
}

// finish hands the request side to stage 2 exactly once: at EOF, or when the
// handler ends with the body not read to the end.
func (b *countingBody) finish(eof bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	if b.buf == nil {
		b.p.ResolveRequest(nil)
		return
	}
	b.p.Req, b.p.ReqIncomplete = b.buf, !eof
	if !b.emit(b.p, false) {
		b.buf.Release()
		b.p.ResolveRequest(nil)
	}
}

// countingWriter measures the response sent to the client and captures its
// head and tail. Unwrap lets http.ResponseController reach Flush and Hijack.
type countingWriter struct {
	http.ResponseWriter
	h      *Handler
	p      *capture.Pending
	status int
}

func (w *countingWriter) WriteHeader(code int) {
	if code >= 200 && w.status == 0 {
		w.status = code
		w.start()
	}
	w.ResponseWriter.WriteHeader(code)
}

// start reads the response headers and prepares the capture.
func (w *countingWriter) start() {
	p, c := w.p, w.h.capture
	p.ContentType = w.Header().Get("Content-Type")
	p.ContentEncoding = w.Header().Get("Content-Encoding")
	mt, _, _ := mime.ParseMediaType(p.ContentType)
	p.Stream = mt == "text/event-stream"
	if p.Capture {
		limit := c.ResponseMaxBytes
		if p.Stream {
			limit = c.ResponseHeadBytes
		}
		p.Head = capture.NewBuffer(int(limit), int(c.BudgetStep), w.h.budget)
	}
}

func (w *countingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
		w.start()
	}
	n, err := w.ResponseWriter.Write(b)
	p := w.p
	if n > 0 {
		chunk := b[:n]
		if p.Head != nil {
			kept := p.Head.Write(chunk)
			if rest := chunk[kept:]; len(rest) > 0 && !p.TailFailed {
				if p.Tail == nil {
					if p.Tail = capture.NewRing(int(w.h.capture.ResponseTailBytes), w.h.budget); p.Tail == nil {
						p.TailFailed = true
					}
				}
				if p.Tail != nil {
					p.Tail.Write(rest)
				}
			}
		}
		if p.Stream {
			p.SSE.Write(chunk)
		}
		p.RespBytes += int64(n)
		p.AddPoint(p.RespBytes, time.Now(), w.h.capture.TimelineMaxPoints)
	}
	return n, err
}

func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
