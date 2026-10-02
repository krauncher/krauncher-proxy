// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/krauncher/krauncher-proxy/internal/auth"
	"github.com/krauncher/krauncher-proxy/internal/capture"
	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/dialect"
	"github.com/krauncher/krauncher-proxy/internal/metrics"
	"github.com/krauncher/krauncher-proxy/internal/pipeline"
	"github.com/krauncher/krauncher-proxy/internal/proxy"
	"github.com/krauncher/krauncher-proxy/internal/shape"
	"github.com/krauncher/krauncher-proxy/internal/sink/jsonl"
)

type parts struct {
	met   *metrics.Metrics
	h     *proxy.Handler
	q     *pipeline.Queue[shape.Job]
	asm   *shape.Assembler
	sink  *jsonl.Writer
	authn *auth.Authenticator
	busy  atomic.Int64
	sinkC config.JSONL
}

// build wires the components the way serve does, without listeners.
func build(t *testing.T, handle func(shape.Job)) *parts {
	t.Helper()
	dir := t.TempDir()
	tokens := filepath.Join(dir, "tokens")
	os.WriteFile(tokens, []byte("app-a:"+auth.HashToken("token-a")+"\n"), 0o600)
	cfg := config.Default()
	cfg.Routes = []config.Route{{Name: "main", Prefix: "/v1", Upstream: "http://127.0.0.1:1", Dialect: config.DialectOpenAI}}
	cfg.ClientAuth = config.ClientAuth{Mode: config.AuthHeader, Header: "X-Proxy-Key", TokensFile: tokens}
	cfg.Sink.JSONL.Enabled, cfg.Sink.JSONL.Dir, cfg.Sink.JSONL.Gzip = true, filepath.Join(dir, "data"), false
	cfg.Sink.JSONL.MaxBytes, cfg.Sink.JSONL.Batch = 100, 1
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := &parts{sinkC: cfg.Sink.JSONL}
	var err error
	if p.authn, err = auth.New(cfg.ClientAuth); err != nil {
		t.Fatal(err)
	}
	p.met = metrics.New(cfg.Metrics, p.authn.Names())
	if p.sink, err = jsonl.Open(cfg.Sink.JSONL, "test", log); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.sink.Close() })
	p.asm = &shape.Assembler{Instance: "test", Log: log, Out: func(shape.Record) {}}
	p.q = pipeline.New(1, 1, handle)
	if p.h, err = proxy.New(proxy.Options{
		Routes: cfg.Routes, Capture: cfg.Capture, Transport: proxy.NewTransport(cfg.Upstream),
		Emit: func(*capture.Pending, bool) bool { return true }, Log: log, Auth: p.authn,
	}); err != nil {
		t.Fatal(err)
	}
	selfMetrics(p.met, p.h, p.q, p.asm, p.sink, p.authn, &p.busy)
	return p
}

// values gathers name{label=value,…} → value for llm_shape_* series.
func values(t *testing.T, m *metrics.Metrics) map[string]float64 {
	fams, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range fams {
		for _, mt := range f.Metric {
			var labels []string
			for _, l := range mt.Label {
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			key := f.GetName()
			if len(labels) > 0 {
				key += "{" + strings.Join(labels, ",") + "}"
			}
			out[key] = value(mt)
		}
	}
	return out
}

func value(m *dto.Metric) float64 {
	switch {
	case m.Counter != nil:
		return m.Counter.GetValue()
	case m.Gauge != nil:
		return m.Gauge.GetValue()
	}
	return -1
}

type panicky struct{ dialect.Generic }

func (panicky) ParseRequest(dialect.Endpoint, []byte, bool) dialect.Request { panic("parser bug") }

// Every self metric reports what actually happened in its component.
func TestSelfMetricsReflectComponents(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	p := build(t, func(shape.Job) { <-block })

	// One job held by the worker, one queued, one dropped.
	for range 3 {
		p.q.Submit(shape.Job{P: capture.NewPending(), Response: true})
	}
	// An unrouted request and an unauthenticated one.
	rec := httptest.NewRecorder()
	p.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/elsewhere", nil))
	p.h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/x", nil))
	// A stage-2 panic.
	shape.Dialects["panicky"] = panicky{}
	defer delete(shape.Dialects, "panicky")
	pp := capture.NewPending()
	pp.Dialect, pp.Req = "panicky", capture.NewBuffer(10, 10, capture.NewBudget(100))
	p.asm.Handle(shape.Job{P: pp})
	// Sink write errors: the directory disappears, the next rotation fails.
	os.RemoveAll(p.sinkC.Dir)
	for i := range 5 {
		p.sink.Submit(shape.Record{ID: strings.Repeat("x", 50+i)})
	}
	p.busy.Store(int64(2500 * time.Millisecond))

	var v map[string]float64
	for range 100 {
		v = values(t, p.met)
		if v[`llm_shape_sink_write_errors_total{sink=jsonl}`] > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := map[string]float64{
		`llm_shape_events_dropped_total`:                1,
		`llm_shape_event_queue_length`:                  1,
		`llm_shape_unrouted_total`:                      1,
		`llm_shape_auth_failures_total{reason=missing}`: 1,
		`llm_shape_auth_failures_total{reason=invalid}`: 0,
		`llm_shape_stage2_panics_total`:                 1,
		`llm_shape_worker_busy_seconds_total`:           2.5,
		`llm_shape_capture_budget_used_bytes`:           0,
		`llm_shape_inflight{route=main}`:                0,
		`llm_shape_records_dropped_total{sink=jsonl}`:   0,
	}
	for k, w := range want {
		if got, ok := v[k]; !ok || got != w {
			t.Errorf("%s = %v (present %v), want %v", k, got, ok, w)
		}
	}
	if v[`llm_shape_sink_write_errors_total{sink=jsonl}`] == 0 {
		t.Error("sink write errors not reported")
	}
	if rec.Code != 404 {
		t.Errorf("unrouted status %d", rec.Code)
	}
}

// Every llm_shape_* metric the dashboard queries exists in the code, so a
// rename cannot silently empty a panel.
func TestDashboardMetricsExist(t *testing.T) {
	p := build(t, func(shape.Job) {})
	names := p.met.Names()
	raw, err := os.ReadFile("../../dashboards/shape-overview.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`llm_shape_[a-z0-9_]+`)
	used := map[string]bool{}
	for _, panel := range d.Panels {
		for _, target := range panel.Targets {
			for _, n := range re.FindAllString(target.Expr, -1) {
				used[n] = true
				if !names[n] {
					t.Errorf("panel %q uses %s, which the proxy does not export", panel.Title, n)
				}
			}
		}
	}
	if len(used) < 15 {
		t.Fatalf("only %d metric names found in the dashboard: parsing broke?", len(used))
	}
	var list []string
	for n := range used {
		list = append(list, n)
	}
	sort.Strings(list)
	t.Logf("dashboard uses %d metrics: %s", len(list), strings.Join(list, ", "))
}
