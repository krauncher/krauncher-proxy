// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/krauncher/krauncher-proxy/internal/auth"
	"github.com/krauncher/krauncher-proxy/internal/capture"
	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/harden"
	"github.com/krauncher/krauncher-proxy/internal/metrics"
	"github.com/krauncher/krauncher-proxy/internal/pipeline"
	"github.com/krauncher/krauncher-proxy/internal/proxy"
	"github.com/krauncher/krauncher-proxy/internal/shape"
	"github.com/krauncher/krauncher-proxy/internal/sink/jsonl"
)

// serve runs the proxy until ctx is cancelled, then shuts down in order:
// stop accepting and wait for in-flight requests (up to shutdown.grace),
// drain the event queue, flush the sinks, close the metrics listener.
func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if err := harden.Apply(); err != nil {
		log.Warn("process hardening not applied", "err", err)
	}
	for key, path := range map[string]string{
		"client_auth.tokens_file":   cfg.ClientAuth.TokensFile,
		"metrics.bearer_token_file": cfg.Metrics.BearerTokenFile,
		"listen.tls.key_file":       cfg.Listen.TLS.KeyFile,
		"metrics.tls.key_file":      cfg.Metrics.TLS.KeyFile,
	} {
		if path == "" {
			continue
		}
		if why := harden.LooseSecretFile(path); why != "" {
			log.Warn("secret file readable by others", "key", key, "detail", why)
		}
	}
	authn, err := auth.New(cfg.ClientAuth)
	if err != nil {
		return err
	}
	met := metrics.New(cfg.Metrics, authn.Names())

	var sink *jsonl.Writer
	if cfg.Sink.JSONL.Enabled {
		if sink, err = jsonl.Open(cfg.Sink.JSONL, cfg.Instance.Name, log); err != nil {
			return fmt.Errorf("jsonl sink: %w", err)
		}
	}
	asm := &shape.Assembler{
		Instance:    cfg.Instance.Name,
		Estimate:    cfg.Estimate,
		RequestWait: cfg.Pipeline.RequestWait,
		MaxBody:     int(cfg.Capture.ResponseMaxBytes),
		Log:         log,
		Out: func(r shape.Record) {
			met.Observe(r)
			// Refused requests are counted in metrics only: writing a record
			// for each would let unauthenticated traffic drive disk writes.
			if sink != nil && r.Outcome != shape.OutcomeUnauthorized {
				sink.Submit(r)
			}
		},
	}
	workers := cfg.Pipeline.Workers
	if workers == 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	var busy atomic.Int64 // nanoseconds spent in stage-2 jobs
	events := pipeline.New(cfg.Pipeline.QueueSize, workers, func(j shape.Job) {
		start := time.Now()
		asm.Handle(j)
		busy.Add(int64(time.Since(start)))
	})
	// Runs on every exit path, after the proxy server has stopped: drain the
	// events, flush the sink, and only then close the metrics listener, so the
	// last records can still be scraped while draining.
	var msrv *metrics.Server
	defer func() {
		events.Close()
		if sink != nil {
			if err := sink.Close(); err != nil {
				log.Error("jsonl sink close", "err", err)
			}
		}
		if msrv != nil {
			msrv.Close()
		}
		log.Info("stopped", "events_dropped", events.Dropped())
	}()

	h, err := proxy.New(proxy.Options{
		Routes:    cfg.Routes,
		Limits:    cfg.Limits,
		Capture:   cfg.Capture,
		Transport: proxy.NewTransport(cfg.Upstream),
		Emit: func(p *capture.Pending, response bool) bool {
			return events.Submit(shape.Job{P: p, Response: response})
		},
		Log:  log,
		Auth: authn,
	})
	if err != nil {
		return err
	}
	selfMetrics(met, h, events, asm, sink, authn, &busy)

	if msrv, err = metrics.NewServer(cfg.Metrics, met); err != nil {
		return err
	}
	go func() {
		if err := msrv.Serve(); err != nil {
			log.Error("metrics listener", "err", err)
		}
	}()

	srv, ln, err := proxyServer(cfg, h, log)
	if err != nil {
		return err
	}
	// Serve mutates TLSConfig (HTTP/2 setup): read it before starting.
	withTLS := srv.TLSConfig != nil
	errc := make(chan error, 1)
	go func() {
		if withTLS {
			errc <- srv.ServeTLS(ln, "", "")
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	log.Info("listening", "addr", ln.Addr().String(), "tls", withTLS, "client_auth", cfg.ClientAuth.Mode,
		"metrics", msrv.Addr(), "version", buildVersion(), "instance", cfg.Instance.Name, "routes", len(cfg.Routes))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down", "grace", cfg.Shutdown.Grace)
	msrv.Drain()
	sctx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Grace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("in-flight requests cut at grace period", "err", err)
		srv.Close()
		// Close does not wait for handlers. Give the cut requests a moment to
		// emit their records before the event queue closes; otherwise exactly
		// the requests that ran longest would be missing from the data.
		for deadline := time.Now().Add(5 * time.Second); h.InflightTotal() > 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}
	log.Info("server stopped", "not_found", h.NotFound())
	return nil
}

// proxyServer binds the proxy listener, with TLS and client certificates
// when configured.
func proxyServer(cfg config.Config, h http.Handler, log *slog.Logger) (*http.Server, net.Listener, error) {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: cfg.Listen.ReadHeaderTimeout,
		IdleTimeout:       cfg.Listen.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	if t := cfg.Listen.TLS; t.Enabled() {
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("listen.tls: %w", err)
		}
		tc := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
		if t.ClientCAFile != "" {
			pool, err := metrics.LoadCAPool(t.ClientCAFile)
			if err != nil {
				return nil, nil, fmt.Errorf("listen.tls.client_ca_file: %w", err)
			}
			tc.ClientCAs, tc.ClientAuth = pool, tls.RequireAndVerifyClientCert
		}
		srv.TLSConfig = tc
	}
	ln, err := net.Listen("tcp", cfg.Listen.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen.addr: %w", err)
	}
	return srv, ln, nil
}

// selfMetrics exposes the proxy's own counters (doc/outputs.md, Self metrics).
func selfMetrics(m *metrics.Metrics, h *proxy.Handler, q *pipeline.Queue[shape.Job], asm *shape.Assembler,
	sink *jsonl.Writer, authn *auth.Authenticator, busy *atomic.Int64) {
	f := func(v uint64) float64 { return float64(v) }
	m.Func("events_dropped_total", "Stage-2 jobs dropped because the event queue was full.", true, nil, func() float64 { return f(q.Dropped()) })
	m.Func("event_queue_length", "Jobs waiting in the event queue.", false, nil, func() float64 { return float64(q.Len()) })
	m.Func("stage2_panics_total", "Stage-2 jobs that panicked (record dropped).", true, nil, func() float64 { return f(asm.Panics() + q.Panics()) })
	m.Func("worker_busy_seconds_total", "Time stage-2 workers spent on jobs.", true, nil, func() float64 { return float64(busy.Load()) / 1e9 })
	m.Func("capture_budget_used_bytes", "Capture memory currently reserved.", false, nil, func() float64 { return float64(h.BudgetUsed()) })
	m.Func("unrouted_total", "Requests that matched no route (404 from the proxy).", true, nil, func() float64 { return f(h.NotFound()) })
	for _, route := range h.Routes() {
		m.Func("inflight", "Requests in flight on the route (this instance).", false, prometheus.Labels{"route": route},
			func() float64 { return float64(h.Inflight(route)) })
	}
	if sink != nil {
		jl := prometheus.Labels{"sink": "jsonl"}
		m.Func("records_dropped_total", "Records dropped because the sink queue was full.", true, jl, func() float64 { return f(sink.Dropped()) })
		m.Func("sink_write_errors_total", "Failed sink writes.", true, jl, func() float64 { return f(sink.WriteErrors()) })
	}
	if authn != nil {
		for _, reason := range auth.Reasons() {
			m.Func("auth_failures_total", "Requests refused by client authentication.", true, prometheus.Labels{"reason": reason},
				func() float64 { return f(authn.Failures(reason)) })
		}
	}
}
