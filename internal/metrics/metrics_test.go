// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/shape"
	"github.com/krauncher/krauncher-proxy/internal/testpki"
)

func ptr[T any](v T) *T { return &v }

func rec(model string, status int, outcome, usage string, prompt, completion int) shape.Record {
	r := shape.Record{
		Route: "main", Dialect: "openai", Endpoint: "chat", Model: ptr(model), Stream: ptr(true),
		Status: status, Outcome: outcome, UsageSource: usage, LatencyMS: 1500, TTFTMS: ptr(200.0),
		Capture: shape.CaptureFull, ConcurrencyAtArrival: 2, ReqBytes: 900, RespBytes: 9000,
	}
	if usage != shape.UsageNone {
		r.PromptTokens, r.CompletionTokens = ptr(prompt), ptr(completion)
	}
	return r
}

func TestObserve(t *testing.T) {
	c := config.Default().Metrics
	m := New(c, nil)
	m.Observe(rec("m1", 200, shape.OutcomeOK, shape.UsageStreamFinal, 1500, 100))
	m.Observe(rec("m1", 200, shape.OutcomeOK, shape.UsageEstimated, 600, 40))
	m.Observe(rec("m1", 429, shape.OutcomeOK, shape.UsageNone, 0, 0))
	m.Observe(rec("m1", 200, shape.OutcomeClientCancelled, shape.UsageStreamFinal, 10, 1))

	if got := testutil.ToFloat64(m.requests.WithLabelValues("main", "m1", "chat", "true", "4xx", "ok")); got != 1 {
		t.Errorf("4xx requests %v", got)
	}
	// Only the two successful requests reach the shape metrics.
	if got := testutil.CollectAndCount(m.latency); got != 1 {
		t.Errorf("latency series %d", got)
	}
	reported := testutil.ToFloat64(m.completionTotal.WithLabelValues("main", "m1", "chat", "true", "reported"))
	estimated := testutil.ToFloat64(m.completionTotal.WithLabelValues("main", "m1", "chat", "true", "estimated"))
	if reported != 100 || estimated != 40 {
		t.Errorf("completion totals reported %v estimated %v", reported, estimated)
	}
	if got := testutil.ToFloat64(m.cells.WithLabelValues("main", "unknown", "unknown", "m1", "chat", "reported", "2048", "128")); got != 1 {
		t.Errorf("cell 2048×128 = %v", got)
	}
	if got := testutil.ToFloat64(m.cells.WithLabelValues("main", "unknown", "unknown", "m1", "chat", "estimated", "1024", "64")); got != 1 {
		t.Errorf("cell 1024×64 = %v", got)
	}
	if got := testutil.ToFloat64(m.usageMissing.WithLabelValues("main", "m1", "chat")); got != 1 {
		t.Errorf("usage missing %v", got)
	}
}

func TestBucket(t *testing.T) {
	b := []int{512, 1024, 2048}
	for v, want := range map[int]string{0: "512", 512: "512", 513: "1024", 2048: "2048", 2049: "inf"} {
		if got := bucket(v, b); got != want {
			t.Errorf("bucket(%d) = %s, want %s", v, got, want)
		}
	}
}

func TestModelLabelBounds(t *testing.T) {
	first := newLabelSet(nil, 2)
	if first.value("a") != "a" || first.value("b") != "b" || first.value("c") != "other" || first.value("a") != "a" {
		t.Error("first-N")
	}
	allow := newLabelSet([]string{"x"}, 50)
	if allow.value("x") != "x" || allow.value("y") != "other" || allow.value("") != "unknown" {
		t.Error("allowlist")
	}
}

func TestClientLabelAndEmbeddings(t *testing.T) {
	c := config.Default().Metrics
	c.ClientLabel = true
	m := New(c, []string{"app-a"})
	r := rec("e", 200, shape.OutcomeOK, shape.UsageResponse, 300, 0)
	r.Endpoint, r.CompletionTokens, r.Client = "embeddings", nil, ptr("app-a")
	m.Observe(r)
	r.Client = ptr("intruder")
	m.Observe(r)
	if got := testutil.ToFloat64(m.cells.WithLabelValues("main", "unknown", "unknown", "e", "embeddings", "reported", "512", "0", "app-a")); got != 1 {
		t.Errorf("embeddings cell for app-a %v", got)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("main", "e", "embeddings", "true", "2xx", "ok", "other")); got != 1 {
		t.Errorf("unknown client not mapped to other: %v", got)
	}
}

func startServer(t *testing.T, c config.Metrics) *Server {
	t.Helper()
	m := New(c, nil)
	m.Func("test_gauge", "test", false, nil, func() float64 { return 7 })
	s, err := NewServer(c, m)
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve()
	t.Cleanup(func() { s.Close() })
	return s
}

func fetch(t *testing.T, c *http.Client, url, auth string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServerBearerHealthAndDrain(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("scrape-secret\n"), 0o600)
	c := config.Default().Metrics
	c.Listen, c.BearerTokenFile, c.Pprof = "127.0.0.1:0", tok, true
	s := startServer(t, c)
	base, hc := "http://"+s.Addr(), http.DefaultClient

	if code, _ := fetch(t, hc, base+"/metrics", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := fetch(t, hc, base+"/metrics", "Bearer wrong"); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	code, body := fetch(t, hc, base+"/metrics", "Bearer scrape-secret")
	if code != 200 || !strings.Contains(body, "llm_shape_test_gauge 7") || !strings.Contains(body, "go_goroutines") {
		t.Errorf("metrics: %d", code)
	}
	if code, _ := fetch(t, hc, base+"/debug/pprof/", ""); code != 401 {
		t.Errorf("pprof without token: %d", code)
	}
	if code, _ := fetch(t, hc, base+"/debug/pprof/", "Bearer scrape-secret"); code != 200 {
		t.Errorf("pprof with token: %d", code)
	}
	// Probes work without the token.
	for _, p := range []string{"/healthz", "/readyz"} {
		if code, _ := fetch(t, hc, base+p, ""); code != 200 {
			t.Errorf("%s: %d", p, code)
		}
	}
	s.Drain()
	if code, _ := fetch(t, hc, base+"/readyz", ""); code != 503 {
		t.Errorf("readyz while draining: %d", code)
	}
	if code, _ := fetch(t, hc, base+"/healthz", ""); code != 200 {
		t.Errorf("healthz while draining: %d", code)
	}
}

func TestServerPprofOffByDefault(t *testing.T) {
	c := config.Default().Metrics
	c.Listen = "127.0.0.1:0"
	s := startServer(t, c)
	if code, _ := fetch(t, http.DefaultClient, "http://"+s.Addr()+"/debug/pprof/", ""); code != 404 {
		t.Errorf("pprof served while disabled: %d", code)
	}
}

func TestServerTLSAndClientCertificates(t *testing.T) {
	pki := testpki.New(t)
	cert, key, _ := pki.Issue(t, "metrics", true)
	_, _, scraper := pki.Issue(t, "prometheus", false)
	c := config.Default().Metrics
	c.Listen, c.TLS = "127.0.0.1:0", config.TLS{CertFile: cert, KeyFile: key, ClientCAFile: pki.CAFile}
	s := startServer(t, c)
	url := "https://" + s.Addr() + "/metrics"
	if code, body := fetch(t, pki.Client(scraper), url, ""); code != 200 || !strings.Contains(body, "llm_shape_test_gauge") {
		t.Errorf("scrape with a client certificate: %d", code)
	}
	if code, _ := fetch(t, pki.Client(), url, ""); code != 401 {
		t.Errorf("scrape without a client certificate got %d", code)
	}
	// Probes present no certificate and must still get through.
	for _, p := range []string{"/healthz", "/readyz"} {
		if code, _ := fetch(t, pki.Client(), "https://"+s.Addr()+p, ""); code != 200 {
			t.Errorf("%s without a client certificate: %d", p, code)
		}
	}
	// A certificate from another CA is refused at the TLS layer.
	_, _, foreign := testpki.New(t).Issue(t, "intruder", false)
	if code, _ := fetch(t, pki.Client(foreign), url, ""); code != 0 {
		t.Errorf("certificate from a foreign CA got %d", code)
	}
	if code, _ := fetch(t, http.DefaultClient, "http://"+s.Addr()+"/metrics", ""); code == 200 {
		t.Error("plain HTTP served metrics on a TLS listener")
	}
}
