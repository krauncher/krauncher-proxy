// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/capture"
	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/fakeupstream"
)

// recorder collects emitted events.
type recorder struct {
	mu     sync.Mutex
	events []*capture.Pending
	added  chan struct{}
}

func newRecorder() *recorder { return &recorder{added: make(chan struct{}, 1000)} }

// emit records response sides; request sides are released unparsed.
func (r *recorder) emit(e *capture.Pending, response bool) bool {
	if !response {
		e.Req.Release()
		e.ResolveRequest(nil)
		return true
	}
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	r.added <- struct{}{}
	return true
}

// wait returns the first n events, failing after a timeout.
func (r *recorder) wait(t *testing.T, n int) []*capture.Pending {
	t.Helper()
	for {
		r.mu.Lock()
		if len(r.events) >= n {
			ev := append([]*capture.Pending(nil), r.events...)
			r.mu.Unlock()
			return ev
		}
		r.mu.Unlock()
		select {
		case <-r.added:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %d events", n)
		}
	}
}

func startProxy(t *testing.T, routes []config.Route, limits config.Limits) (string, *recorder, *Handler) {
	t.Helper()
	rec := newRecorder()
	url, h := startProxyWith(t, routes, limits, NewTransport(config.Default().Upstream), rec.emit, slog.Default())
	return url, rec, h
}

func startProxyWith(t *testing.T, routes []config.Route, limits config.Limits, tr http.RoundTripper, emit Emit, log *slog.Logger) (string, *Handler) {
	t.Helper()
	for i := range routes {
		if routes[i].Dialect == "" {
			routes[i].Dialect = config.DialectGeneric
		}
	}
	h, err := New(Options{Routes: routes, Limits: limits, Capture: config.Default().Capture, Transport: tr, Emit: emit, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL, h
}

func oneRoute(upstream string) []config.Route {
	return []config.Route{{Name: "main", Prefix: "/", Upstream: upstream}}
}

// client never adds Accept-Encoding, so header transparency can be checked.
var client = &http.Client{Transport: &http.Transport{DisableCompression: true}}

func payload(n int, seed uint64) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(seed, seed))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func TestBodiesForwardedIdentically(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		w.Header().Set("X-Body-SHA", hex.EncodeToString(sum[:]))
		n, _ := strconv.Atoi(r.Header.Get("X-Resp-Size"))
		w.Write(payload(n, 7))
	}))
	defer up.Close()
	url, rec, _ := startProxy(t, oneRoute(up.URL), config.Limits{})

	sizes := []int{0, 1, 1000, 1<<20 + 7}
	for i, n := range sizes {
		body := payload(n, uint64(n))
		req, _ := http.NewRequest(http.MethodPost, url+"/v1/x", bytes.NewReader(body))
		req.Header.Set("X-Resp-Size", strconv.Itoa(n*2+3))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		sum := sha256.Sum256(body)
		if resp.Header.Get("X-Body-SHA") != hex.EncodeToString(sum[:]) {
			t.Errorf("size %d: request body changed in transit", n)
		}
		if !bytes.Equal(got, payload(n*2+3, 7)) {
			t.Errorf("size %d: response body changed in transit", n)
		}
		ev := rec.wait(t, i+1)[i]
		if ev.ReqBytes != int64(n) || ev.RespBytes != int64(n*2+3) {
			t.Errorf("size %d: recorded req %d resp %d", n, ev.ReqBytes, ev.RespBytes)
		}
	}
}

func TestHeadersPassThrough(t *testing.T) {
	seen := make(chan http.Header, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.Header().Set("X-Resp", "r")
		w.WriteHeader(201)
	}))
	defer up.Close()
	url, rec, _ := startProxy(t, oneRoute(up.URL), config.Limits{})

	req, _ := http.NewRequest(http.MethodGet, url+"/v1/models?x=1", nil)
	req.Header.Set("Authorization", "Bearer upstream-key")
	req.Header.Set("X-Custom", "c")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "h")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h := <-seen
	if h.Get("Authorization") != "Bearer upstream-key" || h.Get("X-Custom") != "c" {
		t.Errorf("end-to-end headers lost: %v", h)
	}
	if h.Get("X-Forwarded-For") != "203.0.113.9" {
		t.Errorf("X-Forwarded-For = %q, want the client's value unchanged", h.Get("X-Forwarded-For"))
	}
	if h.Get("X-Hop") != "" {
		t.Error("hop-by-hop header forwarded")
	}
	if h.Get("Accept-Encoding") != "" {
		t.Errorf("Accept-Encoding added: %q", h.Get("Accept-Encoding"))
	}
	if resp.StatusCode != 201 || resp.Header.Get("X-Resp") != "r" {
		t.Errorf("response status %d header %q", resp.StatusCode, resp.Header.Get("X-Resp"))
	}
	if ev := rec.wait(t, 1)[0]; ev.Status != 201 || ev.Outcome != capture.OutcomeOK || ev.ReqEnd != ev.T0 {
		t.Errorf("event %+v", ev)
	}
}

func TestStreamIsFlushedImmediately(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: two\n\n")
	}))
	defer up.Close()
	url, _, _ := startProxy(t, oneRoute(up.URL), config.Limits{})

	resp, err := client.Post(url+"/s", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if line != "data: one\n" {
			t.Errorf("first line %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first chunk not delivered before the upstream finished")
	}
	close(release)
}

func TestClientCancelReachesUpstream(t *testing.T) {
	upstreamDone := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamDone)
	}))
	defer up.Close()
	url, rec, _ := startProxy(t, oneRoute(up.URL), config.Limits{})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/s", strings.NewReader("{}"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bufio.NewReader(resp.Body).ReadString('\n')
	cancel()
	resp.Body.Close()
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request not cancelled")
	}
	if ev := rec.wait(t, 1)[0]; ev.Outcome != capture.OutcomeClientCancelled {
		t.Errorf("outcome %q, want client_cancelled", ev.Outcome)
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + l.Addr().String()
	l.Close()
	url, rec, _ := startProxy(t, oneRoute(dead), config.Limits{})

	resp, err := client.Post(url+"/x", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	ev := rec.wait(t, 1)[0]
	if ev.Outcome != capture.OutcomeUpstreamError || ev.Status != 502 || !ev.Headers.IsZero() {
		t.Errorf("event %+v", ev)
	}
}

func TestUpstreamBreaksMidStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer up.Close()
	url, rec, _ := startProxy(t, oneRoute(up.URL), config.Limits{})

	resp, err := client.Post(url+"/s", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if ev := rec.wait(t, 1)[0]; ev.Outcome != capture.OutcomeUpstreamError || ev.RespBytes == 0 {
		t.Errorf("event %+v", ev)
	}
}

func TestRouting(t *testing.T) {
	echo := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, name+" "+r.URL.RequestURI())
		}))
	}
	a, ab, root := echo("a"), echo("ab"), echo("root")
	defer a.Close()
	defer ab.Close()
	defer root.Close()
	url, _, _ := startProxy(t, []config.Route{
		{Name: "root", Prefix: "/", Upstream: root.URL},
		{Name: "a", Prefix: "/a", Upstream: a.URL, StripPrefix: true},
		{Name: "ab", Prefix: "/a/b", Upstream: ab.URL + "/base"},
	}, config.Limits{})

	cases := map[string]string{
		"/a/x?q=1": "a /x?q=1",
		"/a":       "a /",
		"/a/b/y":   "ab /base/a/b/y",
		"/ab":      "root /ab",
		"/z":       "root /z",
	}
	for path, want := range cases {
		resp, err := client.Get(url + path)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(got) != want {
			t.Errorf("%s → %q, want %q", path, got, want)
		}
	}
}

func TestNoRouteIs404(t *testing.T) {
	url, rec, h := startProxy(t, []config.Route{{Name: "a", Prefix: "/a", Upstream: "http://127.0.0.1:1"}}, config.Limits{})
	resp, err := client.Get(url + "/zzz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 || h.NotFound() != 1 {
		t.Fatalf("status %d notFound %d", resp.StatusCode, h.NotFound())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) != 0 {
		t.Fatal("unrouted request produced an event")
	}
}

func TestMaxInflight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	}))
	defer up.Close()
	url, rec, _ := startProxy(t, oneRoute(up.URL), config.Limits{MaxInflight: 1})

	first := make(chan error, 1)
	go func() {
		resp, err := client.Get(url + "/x")
		if err == nil {
			resp.Body.Close()
		}
		first <- err
	}()
	<-entered
	resp, err := client.Get(url + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second request status %d, want 503", resp.StatusCode)
	}
	if ev := rec.wait(t, 1)[0]; ev.Outcome != capture.OutcomeProxyError || ev.Status != 503 {
		t.Errorf("event %+v", ev)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestTimingsAgainstFakeUpstream(t *testing.T) {
	up := httptest.NewServer(fakeupstream.Handler(fakeupstream.Options{
		Tokens: 5, HeaderDelay: 50 * time.Millisecond, TokenInterval: 10 * time.Millisecond,
	}))
	defer up.Close()
	url, rec, _ := startProxy(t, oneRoute(up.URL), config.Limits{})

	resp, err := client.Post(url+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
		t.Fatalf("stream not complete: %q", body)
	}
	p := rec.wait(t, 1)[0]
	if p.Status != 200 || p.Outcome != capture.OutcomeOK || p.InflightRoute != 1 || p.InflightGlobal != 1 {
		t.Errorf("event %+v", p)
	}
	// Order, plus one bound the proxy itself must see: headers not before the
	// upstream's 50 ms header delay. No absolute upper bounds: CI is slow.
	if p.ReqEnd.IsZero() || p.Headers.IsZero() || p.ReqEnd.After(p.Headers) ||
		p.Headers.Sub(p.T0) < 50*time.Millisecond || p.End.Before(p.Headers) {
		t.Errorf("timings t0 %v req %v headers %v end %v", p.T0, p.ReqEnd, p.Headers, p.End)
	}
}
