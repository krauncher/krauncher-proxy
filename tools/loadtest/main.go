// SPDX-License-Identifier: Apache-2.0

// Command loadtest measures the proxy against doc/performance.md. Run it once
// against the upstream directly and once through the proxy; the difference is
// what the proxy adds.
//
//	rps mode:     -mode rps -concurrency 64 -duration 30s
//	              closed-loop non-streaming requests: throughput and latency
//	streams mode: -mode streams -streams 20000 -ramp 2000
//	              opens long-lived streams and holds them: time to headers,
//	              peak open streams, chunks per second
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	target := flag.String("target", "http://127.0.0.1:8080/v1/chat/completions", "URL")
	mode := flag.String("mode", "rps", "rps | streams")
	conc := flag.Int("concurrency", 64, "rps: parallel clients")
	rate := flag.Int("rate", 0, "rps: fixed request rate (open loop); 0 = closed loop, as fast as possible")
	dur := flag.Duration("duration", 30*time.Second, "measurement time (after warmup)")
	warm := flag.Duration("warmup", 5*time.Second, "rps: warmup, not measured")
	streams := flag.Int("streams", 1000, "streams: how many to open")
	ramp := flag.Int("ramp", 1000, "streams: openings per second")
	bodySize := flag.Int("body", 2000, "prompt bytes in the request")
	label := flag.String("label", "", "label for the summary line")
	flag.BoolVar(&chunked, "chunked", false, "send request bodies without Content-Length (chunked)")
	flag.Parse()

	stream := *mode == "streams"
	body, _ := json.Marshal(map[string]any{
		"model": "load", "stream": stream,
		"messages": []any{map[string]any{"role": "user", "content": string(bytes.Repeat([]byte("a"), *bodySize))}},
	})
	tr := &http.Transport{
		MaxIdleConns: 100000, MaxIdleConnsPerHost: 100000, IdleConnTimeout: time.Minute,
		DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, DisableCompression: true,
	}
	client := &http.Client{Transport: tr}
	var out map[string]any
	if stream {
		out = runStreams(client, *target, body, *streams, *ramp, *dur)
	} else {
		out = runRPS(client, *target, body, *conc, *rate, *warm, *dur)
	}
	out["label"], out["mode"] = *label, *mode
	json.NewEncoder(os.Stdout).Encode(out)
}

var chunked bool

func post(ctx context.Context, c *http.Client, url string, body []byte) (*http.Response, error) {
	var r io.Reader = bytes.NewReader(body)
	if chunked {
		r = io.MultiReader(r) // hides the length: the client sends chunked
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, r)
	req.Header.Set("Content-Type", "application/json")
	return c.Do(req)
}

func pct(d []time.Duration, p float64) float64 {
	if len(d) == 0 {
		return 0
	}
	return float64(d[min(len(d)-1, int(float64(len(d))*p))].Microseconds()) / 1000
}

func runRPS(c *http.Client, url string, body []byte, conc, rate int, warm, dur time.Duration) map[string]any {
	var measuring atomic.Bool
	var mu sync.Mutex
	var lat []time.Duration
	var errs atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), warm+dur)
	defer cancel()
	time.AfterFunc(warm, func() { measuring.Store(true) })
	// Open loop: a ticker hands out request slots at the fixed rate, so a slow
	// response does not slow the offered load (latency is not hidden by
	// coordinated omission). Slots nobody can take are counted as missed.
	var slots chan time.Time
	var missed atomic.Int64
	if rate > 0 {
		slots = make(chan time.Time, conc)
		go func() {
			// A fixed schedule: slot i is due at start + i/rate. When behind,
			// slots go out immediately to catch up, so the offered rate holds
			// even though sleeps are coarse.
			interval := time.Second / time.Duration(rate)
			for next := time.Now(); ctx.Err() == nil; next = next.Add(interval) {
				if d := time.Until(next); d > 0 {
					time.Sleep(d)
				}
				select {
				case slots <- next:
				default:
					if measuring.Load() {
						missed.Add(1)
					}
				}
			}
		}()
	}
	var wg sync.WaitGroup
	for range conc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]time.Duration, 0, 4096)
			for ctx.Err() == nil {
				start := time.Now()
				if slots != nil {
					select {
					case start = <-slots: // latency counts from the scheduled time
					case <-ctx.Done():
						continue
					}
				}
				resp, err := post(ctx, c, url, body)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				if !measuring.Load() || ctx.Err() != nil {
					continue
				}
				if err != nil || resp.StatusCode != 200 {
					errs.Add(1)
					continue
				}
				local = append(local, time.Since(start))
			}
			mu.Lock()
			lat = append(lat, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	slices.Sort(lat)
	return map[string]any{
		"requests": len(lat), "errors": errs.Load(), "missed_slots": missed.Load(), "rps": float64(len(lat)) / dur.Seconds(),
		"p50_ms": pct(lat, 0.5), "p90_ms": pct(lat, 0.9), "p99_ms": pct(lat, 0.99), "max_ms": pct(lat, 1),
	}
}

func runStreams(c *http.Client, url string, body []byte, n, ramp int, hold time.Duration) map[string]any {
	var open, peak, chunks, errs atomic.Int64
	var mu sync.Mutex
	var ttfb []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	tick := time.NewTicker(time.Second / time.Duration(max(ramp, 1)))
	opened := time.Now()
	for i := 0; i < n; i++ {
		<-tick.C
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			resp, err := post(ctx, c, url, body)
			if err != nil || resp.StatusCode != 200 {
				if ctx.Err() == nil {
					errs.Add(1)
				}
				return
			}
			defer resp.Body.Close()
			mu.Lock()
			ttfb = append(ttfb, time.Since(start))
			mu.Unlock()
			if v := open.Add(1); v > peak.Load() {
				peak.Store(v)
			}
			defer open.Add(-1)
			br := bufio.NewReaderSize(resp.Body, 4096)
			for {
				line, err := br.ReadSlice('\n')
				if err != nil {
					return
				}
				if len(line) == 1 { // blank line ends an event
					chunks.Add(1)
				}
			}
		}()
	}
	tick.Stop()
	rampTime := time.Since(opened)
	// Hold all streams open and measure the chunk rate.
	time.Sleep(2 * time.Second)
	c0, t0 := chunks.Load(), time.Now()
	time.Sleep(hold)
	rate := float64(chunks.Load()-c0) / time.Since(t0).Seconds()
	openNow := open.Load()
	cancel()
	wg.Wait()
	slices.Sort(ttfb)
	log.Printf("opened %d in %v", n, rampTime)
	return map[string]any{
		"streams": n, "errors": errs.Load(), "open_during_hold": openNow, "peak_open": peak.Load(),
		"chunks_per_s": rate, "ttfb_p50_ms": pct(ttfb, 0.5), "ttfb_p99_ms": pct(ttfb, 0.99), "ttfb_max_ms": pct(ttfb, 1),
		"note": fmt.Sprintf("held %v", hold),
	}
}
