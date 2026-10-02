// SPDX-License-Identifier: Apache-2.0

// Package metrics exports shape records as Prometheus metrics and serves
// them on a separate listener. Names and labels follow doc/outputs.md.
package metrics

import (
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/shape"
)

const ns = "llm_shape"

// Metrics turns records into Prometheus series.
type Metrics struct {
	reg         *prometheus.Registry
	names       map[string]bool // registered llm_shape_* names
	models      *labelSet
	clients     *labelSet
	clientLabel bool
	cell        config.ShapeCell

	requests, usageMissing, cells                         *prometheus.CounterVec
	promptTotal, completionTotal, cachedTotal             *prometheus.CounterVec
	captures, parseErrors                                 *prometheus.CounterVec
	promptTokens, completionTokens, ttft, latency, decode *prometheus.HistogramVec
	reqBytes, respBytes, concurrency                      *prometheus.HistogramVec
}

// labelSet bounds the values of a label: an allowlist, or the first max
// values seen; everything else becomes "other".
type labelSet struct {
	mu    sync.Mutex
	allow map[string]bool
	fixed bool
	max   int
}

func newLabelSet(allow []string, max int) *labelSet {
	s := &labelSet{allow: map[string]bool{}, fixed: len(allow) > 0, max: max}
	for _, v := range allow {
		s.allow[v] = true
	}
	return s
}

func (s *labelSet) value(v string) string {
	if v == "" {
		return "unknown"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.allow[v] {
		return v
	}
	if !s.fixed && len(s.allow) < s.max {
		s.allow[v] = true
		return v
	}
	return "other"
}

// New registers every metric. clients is the allowlist for the client label
// (token names or mTLS allowed names).
func New(c config.Metrics, clients []string) *Metrics {
	m := &Metrics{
		reg:         prometheus.NewRegistry(),
		names:       map[string]bool{},
		models:      newLabelSet(c.Models, c.MaxModels),
		clients:     newLabelSet(clients, 0),
		clientLabel: c.ClientLabel,
		cell:        c.ShapeCell,
	}
	m.reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	shapeLabels := []string{"route", "model", "endpoint", "stream"}
	withUsage := append(append([]string{}, shapeLabels...), "usage")
	reqLabels := append(append([]string{}, shapeLabels...), "status_class", "outcome")
	cellLabels := []string{"route", "precision", "engine", "model", "endpoint", "usage", "prompt_bucket", "output_bucket"}
	if c.ClientLabel {
		reqLabels = append(reqLabels, "client")
		cellLabels = append(cellLabels, "client")
	}
	counter := func(name, help string, labels []string) *prometheus.CounterVec {
		m.names[ns+"_"+name] = true
		v := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: name, Help: help}, labels)
		m.reg.MustRegister(v)
		return v
	}
	hist := func(name, help string, buckets []float64, labels []string) *prometheus.HistogramVec {
		for _, suffix := range []string{"", "_bucket", "_count", "_sum"} {
			m.names[ns+"_"+name+suffix] = true
		}
		o := prometheus.HistogramOpts{Namespace: ns, Name: name, Help: help, Buckets: buckets}
		if c.NativeHistograms {
			o.NativeHistogramBucketFactor = 1.1
		}
		v := prometheus.NewHistogramVec(o, labels)
		m.reg.MustRegister(v)
		return v
	}

	m.requests = counter("requests_total", "Proxied requests.", reqLabels)
	m.usageMissing = counter("usage_missing_total", "Requests with neither reported nor estimated usage.", []string{"route", "model", "endpoint"})
	m.cells = counter("cell_total", "Requests by prompt × output token bucket (upper bounds, inf above the last).", cellLabels)
	m.promptTotal = counter("prompt_tokens_total", "Input tokens.", withUsage)
	m.completionTotal = counter("completion_tokens_total", "Output tokens.", withUsage)
	m.cachedTotal = counter("cached_prompt_tokens_total", "Input tokens served from the upstream prefix cache (reported usage only).", shapeLabels)
	m.captures = counter("capture_total", "Records by capture state: full, truncated, skipped.", []string{"route", "state"})
	m.parseErrors = counter("parse_errors_total", "Records with a parse error, by short code.", []string{"dialect", "code"})

	m.promptTokens = hist("prompt_tokens", "Input tokens per request.", prometheus.ExponentialBuckets(16, 2, 17), withUsage)
	m.completionTokens = hist("completion_tokens", "Output tokens per request.", prometheus.ExponentialBuckets(4, 2, 16), withUsage)
	m.ttft = hist("ttft_seconds", "Time to first token (streams).", prometheus.ExponentialBucketsRange(0.01, 60, 16), shapeLabels)
	m.latency = hist("latency_seconds", "Request latency.", prometheus.ExponentialBucketsRange(0.01, 600, 18), shapeLabels)
	m.decode = hist("decode_tokens_per_second", "Output tokens per second after the first token.", prometheus.ExponentialBucketsRange(1, 2000, 14), withUsage)
	m.reqBytes = hist("request_bytes", "Request body size.", prometheus.ExponentialBuckets(256, 2, 17), []string{"route", "endpoint"})
	m.respBytes = hist("response_bytes", "Response body size.", prometheus.ExponentialBuckets(256, 2, 17), []string{"route", "endpoint"})
	m.concurrency = hist("concurrency_at_arrival", "Requests in flight on the route when a request arrived (this instance).", prometheus.ExponentialBuckets(1, 2, 13), []string{"route"})
	return m
}

// Names returns every llm_shape_* series name that can appear, including
// histogram _bucket, _count and _sum, whether or not it has been observed.
func (m *Metrics) Names() map[string]bool { return m.names }

// Registry returns the registry to serve.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Func registers a metric whose value is read on scrape (self metrics).
func (m *Metrics) Func(name, help string, counter bool, constLabels prometheus.Labels, fn func() float64) {
	m.names[ns+"_"+name] = true
	opts := prometheus.Opts{Namespace: ns, Name: name, Help: help, ConstLabels: constLabels}
	if counter {
		m.reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts(opts), fn))
	} else {
		m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts(opts), fn))
	}
}

func statusClass(s int) string {
	switch {
	case s >= 200 && s < 300:
		return "2xx"
	case s >= 400 && s < 500:
		return "4xx"
	case s >= 500 && s < 600:
		return "5xx"
	}
	return "other"
}

func boolLabel(b *bool) string {
	if b == nil {
		return "unknown"
	}
	return strconv.FormatBool(*b)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// orUnknown is the label value for an optional operator-declared field.
func orUnknown(s *string) string {
	if s == nil || *s == "" {
		return "unknown"
	}
	return *s
}

// bucket returns the label of the smallest bound ≥ v, or "inf".
func bucket(v int, bounds []int) string {
	for _, b := range bounds {
		if v <= b {
			return strconv.Itoa(b)
		}
	}
	return "inf"
}

// Observe records one shape record.
func (m *Metrics) Observe(r shape.Record) {
	model := m.models.value(deref(r.Model))
	stream := boolLabel(r.Stream)
	reqLabels := []string{r.Route, model, r.Endpoint, stream, statusClass(r.Status), r.Outcome}
	if m.clientLabel {
		reqLabels = append(reqLabels, m.clients.value(deref(r.Client)))
	}
	m.requests.WithLabelValues(reqLabels...).Inc()
	m.reqBytes.WithLabelValues(r.Route, r.Endpoint).Observe(float64(r.ReqBytes))
	m.respBytes.WithLabelValues(r.Route, r.Endpoint).Observe(float64(r.RespBytes))
	m.concurrency.WithLabelValues(r.Route).Observe(float64(r.ConcurrencyAtArrival))
	m.captures.WithLabelValues(r.Route, r.Capture).Inc()
	if r.ParseError != nil {
		m.parseErrors.WithLabelValues(r.Dialect, *r.ParseError).Inc()
	}
	if r.UsageSource == shape.UsageNone {
		m.usageMissing.WithLabelValues(r.Route, model, r.Endpoint).Inc()
	}

	// Failed requests are counted above only: their sizes and timings would
	// distort the shape.
	if r.Status < 200 || r.Status >= 300 || r.Outcome != shape.OutcomeOK {
		return
	}
	shapeLabels := []string{r.Route, model, r.Endpoint, stream}
	m.latency.WithLabelValues(shapeLabels...).Observe(r.LatencyMS / 1000)
	if r.TTFTMS != nil {
		m.ttft.WithLabelValues(shapeLabels...).Observe(*r.TTFTMS / 1000)
	}
	if r.UsageSource == shape.UsageNone {
		return
	}
	usage := "reported"
	if r.UsageSource == shape.UsageEstimated {
		usage = "estimated"
	}
	withUsage := append(shapeLabels, usage)
	if r.PromptTokens != nil {
		m.promptTokens.WithLabelValues(withUsage...).Observe(float64(*r.PromptTokens))
		m.promptTotal.WithLabelValues(withUsage...).Add(float64(*r.PromptTokens))
	}
	if r.CompletionTokens != nil {
		m.completionTokens.WithLabelValues(withUsage...).Observe(float64(*r.CompletionTokens))
		m.completionTotal.WithLabelValues(withUsage...).Add(float64(*r.CompletionTokens))
	}
	if r.CachedPromptTokens != nil {
		m.cachedTotal.WithLabelValues(shapeLabels...).Add(float64(*r.CachedPromptTokens))
	}
	if r.DecodeTPS != nil {
		m.decode.WithLabelValues(withUsage...).Observe(*r.DecodeTPS)
	}
	if r.PromptTokens != nil && (r.CompletionTokens != nil || r.Endpoint == "embeddings") {
		out := "0"
		if r.CompletionTokens != nil && r.Endpoint != "embeddings" {
			out = bucket(*r.CompletionTokens, m.cell.OutputBounds)
		}
		cell := []string{r.Route, orUnknown(r.Precision), orUnknown(r.Engine), model, r.Endpoint, usage,
			bucket(*r.PromptTokens, m.cell.PromptBounds), out}
		if m.clientLabel {
			cell = append(cell, m.clients.value(deref(r.Client)))
		}
		m.cells.WithLabelValues(cell...).Inc()
	}
}
