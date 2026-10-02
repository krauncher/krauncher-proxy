// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/fakeupstream"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func serveConfig(t *testing.T, upstream string, grace time.Duration) config.Config {
	cfg := config.Default()
	cfg.Instance.Name = "test"
	cfg.Listen.Addr, cfg.Metrics.Listen = freeAddr(t), freeAddr(t)
	cfg.Routes = []config.Route{{Name: "main", Prefix: "/", Upstream: upstream, Dialect: config.DialectOpenAI, StreamUsage: config.StreamUsagePassthrough}}
	cfg.Sink.JSONL.Enabled, cfg.Sink.JSONL.Dir, cfg.Sink.JSONL.Gzip = true, t.TempDir(), false
	cfg.Shutdown.Grace = grace
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// startServe runs serve and returns a cancel func and a channel with its result.
func startServe(t *testing.T, cfg config.Config) (context.CancelFunc, chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	waitListening(t, cfg.Listen.Addr)
	return cancel, done
}

func streamRequest(t *testing.T, addr string) *http.Response {
	resp, err := http.Post("http://"+addr+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	return resp
}

func jsonlLines(t *testing.T, cfg config.Config) []string {
	files, _ := filepath.Glob(filepath.Join(cfg.Sink.JSONL.Dir, "test", "*.jsonl"))
	var lines []string
	for _, f := range files {
		data, _ := os.ReadFile(f)
		lines = append(lines, strings.Split(strings.TrimSpace(string(data)), "\n")...)
	}
	return lines
}

// A stream in progress at shutdown completes, and its record reaches JSONL.
func TestServeShutdownLetsStreamsFinish(t *testing.T) {
	up := httptest.NewServer(fakeupstream.Handler(fakeupstream.Options{Tokens: 10, TokenInterval: 30 * time.Millisecond}))
	defer up.Close()
	cfg := serveConfig(t, up.URL, 10*time.Second)
	cancel, done := startServe(t, cfg)

	resp := streamRequest(t, cfg.Listen.Addr)
	cancel() // shutdown while the stream runs
	// While draining: not ready for new traffic, metrics still served.
	var ready, scrape int
	for range 50 {
		ready, _, _ = get(&http.Client{}, "http://"+cfg.Metrics.Listen+"/readyz", nil)
		if ready == 503 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	scrape, _, _ = get(&http.Client{}, "http://"+cfg.Metrics.Listen+"/metrics", nil)
	if ready != 503 || scrape != 200 {
		t.Errorf("during drain: readyz %d, metrics %d", ready, scrape)
	}
	rest, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasSuffix(string(rest), "data: [DONE]\n\n") {
		t.Errorf("stream cut at shutdown: %q", rest)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return")
	}
	lines := jsonlLines(t, cfg)
	if len(lines) != 1 || !strings.Contains(lines[0], `"outcome":"ok"`) {
		t.Fatalf("records %v", lines)
	}
}

// A stream longer than the grace period is cut and serve still returns.
func TestServeGraceExpiry(t *testing.T) {
	up := httptest.NewServer(fakeupstream.Handler(fakeupstream.Options{Tokens: 10000, TokenInterval: 20 * time.Millisecond}))
	defer up.Close()
	cfg := serveConfig(t, up.URL, 200*time.Millisecond)
	cancel, done := startServe(t, cfg)

	resp := streamRequest(t, cfg.Listen.Addr)
	defer resp.Body.Close()
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve hung after the grace period")
	}
	if el := time.Since(start); el < 200*time.Millisecond {
		t.Errorf("returned after %v, before the grace period", el)
	}
}

func TestServeListenFailureCleansUp(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	cfg := serveConfig(t, "http://127.0.0.1:1", time.Second)
	cfg.Listen.Addr = l.Addr().String() // already taken
	err := serve(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("serve succeeded on a taken port")
	}
	// The sink was opened (its directory exists) and closed: the active file,
	// still empty, was removed on close.
	if _, err := os.Stat(filepath.Join(cfg.Sink.JSONL.Dir, "test")); err != nil {
		t.Fatalf("sink never opened: %v", err)
	}
	if files, _ := filepath.Glob(filepath.Join(cfg.Sink.JSONL.Dir, "test", "*")); len(files) != 0 {
		t.Fatalf("files left after close: %v", files)
	}
}
