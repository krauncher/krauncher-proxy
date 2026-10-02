// SPDX-License-Identifier: Apache-2.0

// Command loadgen sends recorded requests (tools/capture replay sets) through
// the proxy, for demos and load tests. Each request carries X-Fixture so a
// replaying fakeupstream answers with the matching recording.
package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type headers []string

func (h *headers) String() string     { return strings.Join(*h, ",") }
func (h *headers) Set(v string) error { *h = append(*h, v); return nil }

type fixture struct {
	name string
	body []byte
}

func main() {
	target := flag.String("target", "http://127.0.0.1:8080", "proxy base URL")
	dir := flag.String("replay", "testdata/replay/ollama-qwen35", "replay set with *.req.json and *.timing.json")
	path := flag.String("path", "/v1/chat/completions", "request path")
	conc := flag.Int("concurrency", 2, "parallel clients")
	rate := flag.Float64("rate", 1, "requests per second across all clients, 0 = no limit")
	dur := flag.Duration("duration", 0, "stop after this long, 0 = until interrupted")
	var hdrs headers
	flag.Var(&hdrs, "header", "extra request header Name: value (repeatable), e.g. for X-Proxy-Key")
	flag.Parse()

	fixtures := load(*dir)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}

	tokens := make(chan struct{}, *conc)
	go func() { // pacing
		if *rate <= 0 {
			for ctx.Err() == nil {
				tokens <- struct{}{}
			}
			return
		}
		t := time.NewTicker(time.Duration(float64(time.Second) / *rate))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				select {
				case tokens <- struct{}{}:
				default: // all clients busy: skip a beat rather than queue
				}
			}
		}
	}()

	var sent, failed atomic.Int64
	client := &http.Client{}
	var wg sync.WaitGroup
	for range *conc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tokens:
				}
				f := fixtures[rand.IntN(len(fixtures))]
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(*target, "/")+*path, bytes.NewReader(f.body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Fixture", f.name)
				for _, h := range hdrs {
					if k, v, ok := strings.Cut(h, ":"); ok {
						req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
					}
				}
				resp, err := client.Do(req)
				sent.Add(1)
				if err != nil {
					if ctx.Err() == nil {
						failed.Add(1)
					}
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode >= 400 {
					failed.Add(1)
				}
			}
		}()
	}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			log.Printf("done: %d sent, %d failed", sent.Load(), failed.Load())
			return
		case <-t.C:
			log.Printf("%d sent, %d failed", sent.Load(), failed.Load())
		}
	}
}

func load(dir string) []fixture {
	timings, _ := filepath.Glob(filepath.Join(dir, "*.timing.json"))
	var out []fixture
	for _, tf := range timings {
		name := strings.TrimSuffix(filepath.Base(tf), ".timing.json")
		body, err := os.ReadFile(filepath.Join(dir, name+".req.json"))
		if err != nil {
			log.Fatal(err)
		}
		out = append(out, fixture{name, body})
	}
	if len(out) == 0 {
		log.Fatalf("%s: no recordings", dir)
	}
	return out
}
