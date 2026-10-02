// SPDX-License-Identifier: Apache-2.0

// Command llm-shape-proxy is a transparent LLM API proxy that records the
// workload shape of the traffic. See doc/.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/krauncher/krauncher-proxy/internal/auth"
	"github.com/krauncher/krauncher-proxy/internal/capture"
	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/pipeline"
	"github.com/krauncher/krauncher-proxy/internal/proxy"
	"github.com/krauncher/krauncher-proxy/internal/shape"
	"github.com/krauncher/krauncher-proxy/internal/sink/jsonl"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("llm-shape-proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to the YAML configuration file")
	check := fs.Bool("check", false, "validate the configuration and exit")
	showVersion := fs.Bool("version", false, "print the version and exit")
	hashToken := fs.Bool("hash-token", false, "read a token from stdin, print its SHA-256 for the tokens file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	switch {
	case *showVersion:
		fmt.Fprintln(stdout, buildVersion())
		return 0
	case *hashToken:
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			fmt.Fprintln(stderr, err)
			return 1
		}
		token := strings.TrimRight(line, "\r\n")
		if token == "" {
			fmt.Fprintln(stderr, "empty token")
			return 1
		}
		fmt.Fprintln(stdout, auth.HashToken(token))
		return 0
	}

	if *configPath == "" {
		fmt.Fprintln(stderr, "-config is required")
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *check {
		fmt.Fprintln(stdout, "configuration OK")
		return 0
	}

	log := newLogger(cfg.Log, stderr)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx, cfg, log); err != nil {
		log.Error("fatal", "err", err)
		return 1
	}
	return 0
}

// serve runs the proxy until ctx is cancelled, then shuts down in order:
// stop accepting and wait for in-flight requests (up to shutdown.grace),
// drain the event queue, flush the sinks.
func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	var sink *jsonl.Writer
	if cfg.Sink.JSONL.Enabled {
		var err error
		if sink, err = jsonl.Open(cfg.Sink.JSONL, cfg.Instance.Name, log); err != nil {
			return fmt.Errorf("jsonl sink: %w", err)
		}
	}
	workers := cfg.Pipeline.Workers
	if workers == 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	asm := &shape.Assembler{
		Instance:    cfg.Instance.Name,
		Estimate:    cfg.Estimate,
		RequestWait: cfg.Pipeline.RequestWait,
		MaxBody:     int(cfg.Capture.ResponseMaxBytes),
		Log:         log,
		Out: func(r shape.Record) {
			if sink != nil {
				sink.Submit(r)
			}
		},
	}
	events := pipeline.New(cfg.Pipeline.QueueSize, workers, asm.Handle)
	// Runs on every exit path, after the server has stopped: drain the
	// events, then flush the sink.
	defer func() {
		events.Close()
		if sink != nil {
			if err := sink.Close(); err != nil {
				log.Error("jsonl sink close", "err", err)
			}
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
		Log: log,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Listen.Addr,
		Handler:           h,
		ReadHeaderTimeout: cfg.Listen.ReadHeaderTimeout,
		IdleTimeout:       cfg.Listen.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	errc := make(chan error, 1)
	go func() {
		if cfg.Listen.TLS.Enabled() {
			errc <- srv.ListenAndServeTLS(cfg.Listen.TLS.CertFile, cfg.Listen.TLS.KeyFile)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	log.Info("listening", "addr", cfg.Listen.Addr, "version", buildVersion(), "instance", cfg.Instance.Name, "routes", len(cfg.Routes))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down", "grace", cfg.Shutdown.Grace)
	sctx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Grace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("in-flight requests cut at grace period", "err", err)
		srv.Close()
	}
	log.Info("server stopped", "not_found", h.NotFound())
	return nil
}

func newLogger(c config.Log, w io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Level)) // validated by config
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "dev"
}
