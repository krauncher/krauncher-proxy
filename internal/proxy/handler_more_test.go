// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/capture"
	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/fakeupstream"
	"github.com/krauncher/krauncher-proxy/internal/pipeline"
)

func TestConcurrencyAtArrival(t *testing.T) {
	const n = 8
	var entered sync.WaitGroup
	entered.Add(n)
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered.Done()
		<-release
	}))
	defer up.Close()
	url, rec, h := startProxy(t, oneRoute(up.URL), config.Limits{})

	var done sync.WaitGroup
	for range n {
		done.Add(1)
		go func() {
			defer done.Done()
			// One at a time into the proxy, so arrival order is deterministic.
			resp, err := client.Get(url + "/x")
			if err == nil {
				resp.Body.Close()
			}
		}()
		time.Sleep(20 * time.Millisecond)
	}
	entered.Wait()
	close(release)
	done.Wait()

	var route, global []int64
	for _, e := range rec.wait(t, n) {
		route, global = append(route, e.InflightRoute), append(global, e.InflightGlobal)
	}
	slices.Sort(route)
	slices.Sort(global)
	want := []int64{1, 2, 3, 4, 5, 6, 7, 8}
	if !slices.Equal(route, want) || !slices.Equal(global, want) {
		t.Errorf("concurrency route %v global %v, want %v", route, global, want)
	}
	if h.inflight.Load() != 0 || h.routes[0].inflight.Load() != 0 {
		t.Errorf("in-flight counters not back to 0: %d %d", h.inflight.Load(), h.routes[0].inflight.Load())
	}
}

// A stalled stage 2 must not slow traffic: events are dropped instead.
func TestStalledPipelineDoesNotBlockTraffic(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer up.Close()
	stall := make(chan struct{})
	defer close(stall)
	q := pipeline.New(1, 1, func(*capture.Pending) { <-stall })
	emit := func(p *capture.Pending, response bool) bool { return q.Submit(p) }
	url, _ := startProxyWith(t, oneRoute(up.URL), config.Limits{}, NewTransport(config.Default().Upstream), emit, slog.Default())

	const n = 20
	start := time.Now()
	for range n {
		resp, err := client.Get(url + "/x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("%d requests took %v with a stalled pipeline", n, el)
	}
	// One event is held by the stalled worker and one is queued; the rest are
	// dropped. The last handler may still be emitting after the client got its
	// response, so wait briefly for the count.
	deadline := time.Now().Add(2 * time.Second)
	for q.Dropped() < n-2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if q.Dropped() != n-2 {
		t.Errorf("dropped %d, want %d", q.Dropped(), n-2)
	}
}

// After the declared length the body wrapper reports EOF itself and never
// reads the server's body again, so a body that net/http has already closed
// cannot fail the transport's final read.
func TestBodyEOFAtContentLength(t *testing.T) {
	under := &strictBody{data: []byte("0123456789")}
	b := &countingBody{ReadCloser: under, p: capture.NewPending(), emit: func(*capture.Pending, bool) bool { return true }, length: 10}
	got, err := io.ReadAll(b)
	if err != nil || string(got) != "0123456789" {
		t.Fatalf("read %q, %v", got, err)
	}
	under.closed = true // as net/http does once it has drained the body
	if n, err := b.Read(make([]byte, 8)); n != 0 || err != io.EOF {
		t.Fatalf("read after the end: %d, %v", n, err)
	}
	if b.end.Load() == nil {
		t.Fatal("EOF time not recorded")
	}
}

// strictBody fails any read after Close, like net/http's request body.
type strictBody struct {
	data   []byte
	closed bool
}

func (s *strictBody) Read(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("http: invalid Read on closed Body")
	}
	n := copy(p, s.data)
	s.data = s.data[n:]
	return n, nil // never io.EOF: the wrapper must stop by length
}

func (s *strictBody) Close() error { s.closed = true; return nil }

func TestTLSUpstream(t *testing.T) {
	protos := make(chan int, 1)
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protos <- r.ProtoMajor
		io.WriteString(w, "ok")
	}))
	up.EnableHTTP2 = true
	up.StartTLS()
	defer up.Close()

	t.Run("trusted certificate, HTTP/2", func(t *testing.T) {
		tr := NewTransport(config.Default().Upstream)
		pool := x509.NewCertPool()
		pool.AddCert(up.Certificate())
		tr.TLSClientConfig.RootCAs = pool
		rec := newRecorder()
		url, _ := startProxyWith(t, oneRoute(up.URL), config.Limits{}, tr, rec.emit, slog.Default())
		resp, err := client.Get(url + "/x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || <-protos != 2 {
			t.Fatalf("status %d, want 200 over HTTP/2", resp.StatusCode)
		}
	})
	t.Run("untrusted certificate is refused", func(t *testing.T) {
		url, rec, _ := startProxy(t, oneRoute(up.URL), config.Limits{})
		resp, err := client.Get(url + "/x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status %d, want 502", resp.StatusCode)
		}
		if ev := rec.wait(t, 1)[0]; ev.Outcome != capture.OutcomeUpstreamError {
			t.Errorf("outcome %q", ev.Outcome)
		}
	})
}

// The upstream answers without reading the body: the transport stops reading
// it, possibly after ServeHTTP returned. Exercises the atomics in countingBody
// under -race.
func TestUpstreamAnswersBeforeReadingBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer up.Close()
	url, rec, _ := startProxy(t, oneRoute(up.URL), config.Limits{})

	const size = 32 << 20
	resp, err := client.Post(url+"/x", "application/octet-stream", bytes.NewReader(make([]byte, size)))
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("status %d", resp.StatusCode)
		}
	} // an error is acceptable: the proxy may close while the client still uploads
	ev := rec.wait(t, 1)[0]
	if ev.ReqBytes >= size || !ev.ReqEnd.IsZero() {
		t.Errorf("req_bytes %d, req_end set %v: want partial body and no upload time", ev.ReqBytes, !ev.ReqEnd.IsZero())
	}
}

func TestClientCancelBeforeHeaders(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the body first, as a real API does before working. Go's server
		// only notices a closed connection once the body is consumed, so an
		// upstream that never reads it would never see the cancellation.
		io.ReadAll(r.Body)
		<-r.Context().Done()
		close(upstreamCancelled)
	}))
	defer up.Close()
	url, rec, _ := startProxy(t, oneRoute(up.URL), config.Limits{})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/x", strings.NewReader("{}"))
	if _, err := client.Do(req); err == nil {
		t.Fatal("request succeeded, want client timeout")
	}
	select {
	case <-upstreamCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request not cancelled")
	}
	ev := rec.wait(t, 1)[0]
	if ev.Outcome != capture.OutcomeClientCancelled || ev.Status != 0 || !ev.Headers.IsZero() {
		t.Errorf("event %+v, want client_cancelled with no status and no headers", ev)
	}
}

func TestStripPrefixKeepsEscaping(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.RequestURI)
	}))
	defer up.Close()
	url, _, _ := startProxy(t, []config.Route{
		{Name: "a", Prefix: "/a", Upstream: up.URL, StripPrefix: true},
		{Name: "b", Prefix: "/b", Upstream: up.URL},
	}, config.Limits{})

	cases := map[string]string{
		"/a/x%2Fy%20z/%C3%BC?k=%2F": "/x%2Fy%20z/%C3%BC?k=%2F",
		"/a/plain":                  "/plain",
		"/b/x%2Fy":                  "/b/x%2Fy",
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

// Logs at debug level, across success and failure paths, must not contain
// credentials, bodies or query strings (doc/privacy.md).
func TestLogsCarryNoContent(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	fake := httptest.NewServer(fakeupstream.Handler(fakeupstream.Options{Tokens: 3}))
	defer fake.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: secret-answer\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer broken.Close()
	rec := newRecorder()
	url, _ := startProxyWith(t, []config.Route{
		{Name: "ok", Prefix: "/ok", Upstream: fake.URL, StripPrefix: true},
		{Name: "dead", Prefix: "/dead", Upstream: "http://127.0.0.1:1"},
		{Name: "broken", Prefix: "/broken", Upstream: broken.URL},
	}, config.Limits{}, NewTransport(config.Default().Upstream), rec.emit, log)

	body := `{"model":"m","stream":true,"messages":[{"content":"secret-prompt"}]}`
	for _, path := range []string{"/ok/v1/chat/completions?token=secret-query", "/dead/x?token=secret-query", "/broken/x?token=secret-query"} {
		req, _ := http.NewRequest(http.MethodPost, url+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret-key")
		if resp, err := client.Do(req); err == nil {
			io.ReadAll(resp.Body)
			resp.Body.Close()
		}
	}
	rec.wait(t, 3)
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "upstream error") {
		t.Fatalf("expected debug output from the failure paths, got %q", out)
	}
	for _, secret := range []string{"secret-key", "secret-prompt", "secret-answer", "secret-query"} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q:\n%s", secret, out)
		}
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
