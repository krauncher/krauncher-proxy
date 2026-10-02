// SPDX-License-Identifier: Apache-2.0

// Package proxy is the data plane (stage 1): it matches routes, forwards
// requests and responses unchanged, and measures sizes and timings. It never
// parses bodies and never blocks on the shape pipeline.
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/shape"
)

// Emit hands a finished event to stage 2. It must not block.
type Emit func(shape.Event) bool

// Handler is the proxy's http.Handler.
type Handler struct {
	routes   []*route // longest prefix first
	inflight atomic.Int64
	sem      chan struct{} // nil = unlimited
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
func New(routes []config.Route, limits config.Limits, transport http.RoundTripper, emit Emit, log *slog.Logger) (*Handler, error) {
	h := &Handler{emit: emit, log: log}
	if limits.MaxInflight > 0 {
		h.sem = make(chan struct{}, limits.MaxInflight)
	}
	for _, rc := range routes {
		target, err := url.Parse(rc.Upstream)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", rc.Name, err)
		}
		r := &route{cfg: rc}
		r.proxy = &httputil.ReverseProxy{
			Rewrite:        rewrite(rc, target),
			Transport:      transport,
			FlushInterval:  -1, // never buffer: LLM responses are streams
			ModifyResponse: onHeaders,
			ErrorHandler:   h.onError,
			ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelDebug),
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
	ev            *shape.Event
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
	ev := &shape.Event{
		ID: newID(), Route: rt.cfg.Name, Dialect: rt.cfg.Dialect,
		Precision: rt.cfg.Precision, Engine: rt.cfg.Engine, T0: t0,
	}
	if h.sem != nil {
		select {
		case h.sem <- struct{}{}:
			defer func() { <-h.sem }()
		default:
			http.Error(w, "proxy at capacity", http.StatusServiceUnavailable)
			ev.End, ev.Status, ev.Outcome = time.Now(), http.StatusServiceUnavailable, shape.OutcomeProxyError
			h.emit(*ev)
			return
		}
	}
	ev.InflightGlobal = h.inflight.Add(1)
	defer h.inflight.Add(-1)
	ev.InflightRoute = rt.inflight.Add(1)
	defer rt.inflight.Add(-1)

	var body *countingBody
	if r.Body == nil || r.Body == http.NoBody {
		ev.ReqEnd = t0
	} else {
		body = &countingBody{ReadCloser: r.Body}
		r.Body = body
	}
	st := &state{ev: ev}
	cw := &countingWriter{ResponseWriter: w, ev: ev}

	// ReverseProxy aborts a broken stream by panicking with
	// http.ErrAbortHandler; the event is still emitted, then the panic goes on
	// to the server, which closes the connection.
	defer func() {
		p := recover()
		ev.End = time.Now()
		ev.Status = cw.status
		if body != nil {
			ev.ReqBytes = body.n.Load()
			if t := body.end.Load(); t != nil {
				ev.ReqEnd = *t
			}
		}
		switch {
		case r.Context().Err() != nil:
			ev.Outcome = shape.OutcomeClientCancelled
		case st.upstreamError || p != nil:
			ev.Outcome = shape.OutcomeUpstreamError
		default:
			ev.Outcome = shape.OutcomeOK
		}
		h.emit(*ev)
		if p != nil {
			panic(p)
		}
	}()
	rt.proxy.ServeHTTP(cw, r.WithContext(context.WithValue(r.Context(), stateKey{}, st)))
}

func onHeaders(res *http.Response) error {
	if st := stateOf(res.Request.Context()); st != nil {
		st.ev.Headers = time.Now()
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
		h.log.Debug("upstream error", "route", st.ev.Route, "id", st.ev.ID, "err", err)
	}
	w.WriteHeader(http.StatusBadGateway)
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// countingBody measures the request body and when it was fully consumed.
// The transport reads it from its own goroutine, possibly after ServeHTTP has
// returned, hence the atomics.
type countingBody struct {
	io.ReadCloser
	n   atomic.Int64
	end atomic.Pointer[time.Time]
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	if err == io.EOF && b.end.Load() == nil {
		t := time.Now()
		b.end.CompareAndSwap(nil, &t)
	}
	return n, err
}

// countingWriter measures the response sent to the client. Unwrap lets
// http.ResponseController reach Flush and Hijack on the real writer.
type countingWriter struct {
	http.ResponseWriter
	ev     *shape.Event
	status int
}

func (w *countingWriter) WriteHeader(code int) {
	if code >= 200 && w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.ev.RespBytes += int64(n)
	return n, err
}

func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
