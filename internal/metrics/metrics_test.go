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
	if got := testutil.ToFloat64(m.cells.WithLabelValues("main", "", "", "m1", "chat", "reported", "2048", "128")); got != 1 {
		t.Errorf("cell 2048×128 = %v", got)
	}
	if got := testutil.ToFloat64(m.cells.WithLabelValues("main", "", "", "m1", "chat", "estimated", "1024", "64")); got != 1 {
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
	if got := testutil.ToFloat64(m.cells.WithLabelValues("main", "", "", "e", "embeddings", "reported", "512", "0", "app-a")); got != 1 {
		t.Errorf("embeddings cell for app-a %v", got)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("main", "e", "embeddings", "true", "2xx", "ok", "other")); got != 1 {
		t.Errorf("unknown client not mapped to other: %v", got)
	}
}

func TestServerBearerAndHealth(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	os.WriteFile(tok, []byte("scrape-secret\n"), 0o600)
	c := config.Default().Metrics
	c.Listen, c.BearerTokenFile = "127.0.0.1:0", tok
	m := New(c, nil)
	m.Func("test_gauge", "test", false, nil, func() float64 { return 7 })
	s, err := NewServer(c, m)
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve()
	defer s.Close()
	get := func(path, auth string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+s.Addr()+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/metrics", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := get("/metrics", "Bearer wrong"); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	code, body := get("/metrics", "Bearer scrape-secret")
	if code != 200 || !strings.Contains(body, "llm_shape_test_gauge 7") || !strings.Contains(body, "go_goroutines") {
		t.Errorf("metrics: %d", code)
	}
	if code, _ := get("/debug/pprof/", "Bearer scrape-secret"); code != 404 {
		t.Errorf("pprof served while disabled: %d", code)
	}
}
