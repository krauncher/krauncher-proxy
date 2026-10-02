// SPDX-License-Identifier: Apache-2.0

package shape

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/capture"
	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/fakeupstream"
	"github.com/krauncher/krauncher-proxy/internal/pipeline"
	"github.com/krauncher/krauncher-proxy/internal/proxy"
	"github.com/krauncher/krauncher-proxy/internal/sse"
)

const fixtures = "../../testdata/openai/deepseek"

// replay serves recorded responses: the fixture named by X-Fixture, SSE
// event by event with a flush and a short pause, JSON in one piece.
func replay(t *testing.T, gzipJSON bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		name := r.Header.Get("X-Fixture")
		var meta struct {
			Status      int    `json:"status"`
			ContentType string `json:"content_type"`
		}
		m, err := os.ReadFile(filepath.Join(fixtures, name+".meta.json"))
		if err != nil {
			t.Errorf("fixture %s: %v", name, err)
			return
		}
		json.Unmarshal(m, &meta)
		w.Header().Set("Content-Type", meta.ContentType)
		if strings.HasPrefix(meta.ContentType, "text/event-stream") {
			body, _ := os.ReadFile(filepath.Join(fixtures, name+".resp.sse"))
			w.WriteHeader(meta.Status)
			sse.Each(body, false, func(e sse.Event) bool {
				w.Write(body[e.Start:e.End])
				w.(http.Flusher).Flush()
				time.Sleep(2 * time.Millisecond)
				return true
			})
			return
		}
		body, _ := os.ReadFile(filepath.Join(fixtures, name+".resp.json"))
		if gzipJSON {
			var b bytes.Buffer
			zw := gzip.NewWriter(&b)
			zw.Write(body)
			zw.Close()
			body = b.Bytes()
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.WriteHeader(meta.Status)
		w.Write(body)
	}))
}

type harness struct {
	url     string
	h       *proxy.Handler
	mu      sync.Mutex
	records []Record
	added   chan struct{}
	queue   *pipeline.Queue[Job]
}

func newHarness(t *testing.T, routes []config.Route, capCfg config.Capture) *harness {
	t.Helper()
	hs := &harness{added: make(chan struct{}, 1000)}
	asm := &Assembler{
		Instance: "test", Estimate: config.Default().Estimate, RequestWait: time.Second,
		MaxBody: int(capCfg.ResponseMaxBytes),
		Out: func(r Record) {
			hs.mu.Lock()
			hs.records = append(hs.records, r)
			hs.mu.Unlock()
			hs.added <- struct{}{}
		},
	}
	hs.queue = pipeline.New(1024, 4, asm.Handle)
	for i := range routes {
		if routes[i].StreamUsage == "" {
			routes[i].StreamUsage = config.StreamUsagePassthrough
		}
	}
	h, err := proxy.New(proxy.Options{
		Routes: routes, Capture: capCfg, Transport: proxy.NewTransport(config.Default().Upstream),
		Emit: func(p *capture.Pending, response bool) bool { return hs.queue.Submit(Job{P: p, Response: response}) },
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hs.url, hs.h = srv.URL, h
	return hs
}

func (hs *harness) wait(t *testing.T, n int) []Record {
	t.Helper()
	for {
		hs.mu.Lock()
		if len(hs.records) >= n {
			out := append([]Record(nil), hs.records...)
			hs.mu.Unlock()
			return out
		}
		hs.mu.Unlock()
		select {
		case <-hs.added:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %d records", n)
		}
	}
}

var client = &http.Client{Transport: &http.Transport{DisableCompression: true}}

// send posts the fixture's own request body and returns the response body.
func send(t *testing.T, url, fixture string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(fixtures, fixture+".req.json"))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("X-Fixture", fixture)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out
}

func val[T any](t *testing.T, name string, p *T) T {
	t.Helper()
	if p == nil {
		t.Fatalf("%s is null", name)
	}
	return *p
}

func TestEndToEndDeepSeekFixtures(t *testing.T) {
	up := replay(t, false)
	defer up.Close()
	hs := newHarness(t, []config.Route{{Name: "ds", Prefix: "/", Upstream: up.URL, Dialect: config.DialectOpenAI}}, config.Default().Capture)

	names := []string{"chat_basic", "chat_stream", "chat_tools_stream_usage", "reasoner_stream_usage", "err_auth"}
	for _, n := range names {
		send(t, hs.url, n)
	}
	recs := hs.wait(t, len(names))
	byStatus := map[int][]Record{}
	for _, r := range recs {
		byStatus[r.Status] = append(byStatus[r.Status], r)
	}
	for _, r := range byStatus[200] {
		if r.Endpoint != "chat" || val(t, "model", r.Model) != "deepseek-chat" && val(t, "model", r.Model) != "deepseek-reasoner" {
			t.Errorf("%s: endpoint %s model %v", r.ID, r.Endpoint, r.Model)
		}
		if r.Capture != CaptureFull || r.ParseError != nil {
			t.Errorf("%s: capture %s parse error %v", r.ID, r.Capture, r.ParseError)
		}
		val(t, "message_count", r.MessageCount)
		val(t, "text_bytes", r.TextBytes)
		val(t, "prompt_tokens", r.PromptTokens)
		val(t, "completion_tokens", r.CompletionTokens)
		val(t, "output_text_bytes", r.OutputTextBytes)
		if r.Stream != nil && *r.Stream {
			if r.UsageSource != UsageStreamFinal {
				t.Errorf("stream usage source %s", r.UsageSource)
			}
			ttft, lat := val(t, "ttft_ms", r.TTFTMS), r.LatencyMS
			if ttft <= 0 || ttft > lat || val(t, "decode_ms", r.DecodeMS) < 0 {
				t.Errorf("ttft %v latency %v", ttft, lat)
			}
			val(t, "sse_events", r.SSEEvents)
			val(t, "chunks", r.Chunks)
		} else if r.UsageSource != UsageResponse || r.TTFTMS != nil {
			t.Errorf("non-stream: usage %s ttft %v", r.UsageSource, r.TTFTMS)
		}
	}
	if len(byStatus[401]) != 1 || val(t, "error_class", byStatus[401][0].ErrorClass) != "authentication" {
		t.Errorf("401 records %+v", byStatus[401])
	}
	if used := hs.h.BudgetUsed(); used != 0 {
		t.Errorf("capture budget leaked: %d bytes", used)
	}
}

// Small caps force a head/tail gap and a cut request; the record says so and
// still carries usage from the tail.
func TestEndToEndTruncatedCapture(t *testing.T) {
	up := replay(t, false)
	defer up.Close()
	c := config.Default().Capture
	c.RequestMaxBytes, c.ResponseHeadBytes, c.ResponseTailBytes, c.BudgetStep = 64, 1024, 1024, 64
	hs := newHarness(t, []config.Route{{Name: "ds", Prefix: "/", Upstream: up.URL, Dialect: config.DialectOpenAI}}, c)

	send(t, hs.url, "reasoner_stream_usage")
	r := hs.wait(t, 1)[0]
	if r.Capture != CaptureTruncated || r.UsageSource != UsageStreamFinal || val(t, "completion_tokens", r.CompletionTokens) != 34 {
		t.Errorf("capture %s usage %s %v", r.Capture, r.UsageSource, r.CompletionTokens)
	}
	if r.MessageCount != nil || r.TextBytes != nil {
		t.Errorf("partial request counts reported: %v %v", r.MessageCount, r.TextBytes)
	}
	if val(t, "output_text_bytes_source", r.OutputTextBytesSource) != "derived" {
		t.Errorf("source %v", r.OutputTextBytesSource)
	}
	val(t, "ttft_ms", r.TTFTMS)
	if hs.h.BudgetUsed() != 0 {
		t.Errorf("budget leaked")
	}
}

func TestEndToEndGzipJSON(t *testing.T) {
	up := replay(t, true)
	defer up.Close()
	hs := newHarness(t, []config.Route{{Name: "ds", Prefix: "/", Upstream: up.URL, Dialect: config.DialectOpenAI}}, config.Default().Capture)
	body := send(t, hs.url, "chat_cache_second")
	if len(body) == 0 || body[0] != 0x1f {
		t.Fatal("client did not receive the gzip body unchanged")
	}
	r := hs.wait(t, 1)[0]
	if val(t, "cached", r.CachedPromptTokens) != 1408 || r.ParseError != nil {
		t.Errorf("cached %v parse error %v", r.CachedPromptTokens, r.ParseError)
	}
}

// Without usage in the stream, tokens are estimated; with stream_usage: inject
// the proxy asks for usage and the record says so.
func TestEstimationAndInjection(t *testing.T) {
	fake := httptest.NewServer(fakeupstream.Handler(fakeupstream.Options{Tokens: 12}))
	defer fake.Close()
	hs := newHarness(t, []config.Route{
		{Name: "plain", Prefix: "/plain", Upstream: fake.URL, StripPrefix: true, Dialect: config.DialectOpenAI},
		{Name: "inject", Prefix: "/inject", Upstream: fake.URL, StripPrefix: true, Dialect: config.DialectOpenAI, StreamUsage: config.StreamUsageInject},
		{Name: "off", Prefix: "/off", Upstream: fake.URL, StripPrefix: true, Dialect: config.DialectOpenAI, StreamUsage: config.StreamUsageOff},
	}, config.Default().Capture)

	req := `{"model":"m","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("a", 400) + `"}]}`
	var injectedBody []byte
	for _, route := range []string{"plain", "inject", "off"} {
		resp, err := client.Post(hs.url+"/"+route+"/v1/chat/completions", "application/json", strings.NewReader(req))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if route == "inject" {
			injectedBody = b
		}
	}
	got := map[string]Record{}
	for _, r := range hs.wait(t, 3) {
		got[r.Route] = r
	}
	plain := got["plain"]
	if plain.UsageSource != UsageEstimated || val(t, "completion", plain.CompletionTokens) != 12 ||
		val(t, "prompt", plain.PromptTokens) != 400/4+4 || plain.CachedPromptTokens != nil {
		t.Errorf("plain: %s completion %v prompt %v", plain.UsageSource, plain.CompletionTokens, plain.PromptTokens)
	}
	inj := got["inject"]
	if inj.UsageSource != UsageInjected || val(t, "completion", inj.CompletionTokens) != 12 {
		t.Errorf("inject: %s %v", inj.UsageSource, inj.CompletionTokens)
	}
	if !bytes.Contains(injectedBody, []byte(`"usage"`)) {
		t.Error("injected request did not produce a usage chunk")
	}
	if inj.ReqBytes <= plain.ReqBytes {
		t.Errorf("injected request size %d not above original %d", inj.ReqBytes, plain.ReqBytes)
	}
	off := got["off"]
	if off.UsageSource != UsageNone || off.CompletionTokens != nil {
		t.Errorf("off: %s %v", off.UsageSource, off.CompletionTokens)
	}
}

// Capture must never change bytes: random sizes and chunkings, capture on
// (openai route, tiny caps so head, tail and gap all occur) and off.
func TestBytesUnchangedWithCapture(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", r.Header.Get("X-CT"))
		// Echo the body back in uneven pieces with flushes.
		for i, step := 0, 1; i < len(body); i, step = i+step, step*3%97+1 {
			w.Write(body[i:min(i+step, len(body))])
			w.(http.Flusher).Flush()
		}
	}))
	defer up.Close()
	c := config.Default().Capture
	c.RequestMaxBytes, c.ResponseMaxBytes, c.ResponseHeadBytes, c.ResponseTailBytes, c.BudgetStep = 100, 300, 50, 70, 16
	hs := newHarness(t, []config.Route{
		{Name: "cap", Prefix: "/cap", Upstream: up.URL, Dialect: config.DialectOpenAI},
		{Name: "gen", Prefix: "/gen", Upstream: up.URL, Dialect: config.DialectGeneric},
	}, c)
	n := 0
	for _, size := range []int{0, 1, 49, 50, 51, 120, 299, 300, 301, 5000} {
		for _, ct := range []string{"text/event-stream", "application/json"} {
			for _, route := range []string{"/cap", "/gen"} {
				body := make([]byte, size)
				for i := range body {
					body[i] = byte(i*7 + size)
				}
				req, _ := http.NewRequest(http.MethodPost, hs.url+route+"/x", bytes.NewReader(body))
				req.Header.Set("X-CT", ct)
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				got, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if !bytes.Equal(got, body) {
					t.Fatalf("size %d %s %s: bytes changed", size, ct, route)
				}
				n++
			}
		}
	}
	hs.wait(t, n)
	if hs.h.BudgetUsed() != 0 {
		t.Errorf("budget leaked: %d", hs.h.BudgetUsed())
	}
}

// Real recordings with timing (testdata/replay), replayed fast through the
// proxy: usage matches the provider's, TTFT lands where the recording had its
// first token, and output bytes on long streams are measured against the
// exact value.
func TestEndToEndReplay(t *testing.T) {
	const dir = "../../testdata/replay/ollama-qwen35"
	recs, err := fakeupstream.LoadRecordings(dir)
	if err != nil {
		t.Fatal(err)
	}
	const speed = 200
	up := httptest.NewServer(fakeupstream.Replay(recs, speed))
	defer up.Close()
	hs := newHarness(t, []config.Route{{Name: "ollama", Prefix: "/", Upstream: up.URL, Dialect: config.DialectOpenAI}}, config.Default().Capture)

	for _, rec := range recs {
		body, _ := os.ReadFile(filepath.Join(dir, rec.Name+".req.json"))
		req, _ := http.NewRequest(http.MethodPost, hs.url+"/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("X-Fixture", rec.Name)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		hs.wait(t, 1) // sequential: records arrive in recording order
	}
	records := hs.wait(t, len(recs))
	for i, rec := range recs {
		r := records[i]
		var usage struct {
			Usage struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		exact := 0
		events := 0
		if strings.HasPrefix(rec.ContentType, "text/event-stream") {
			sse.Each(rec.Body, false, func(e sse.Event) bool {
				json.Unmarshal(e.Data, &usage)
				var d struct {
					Choices []struct {
						Delta struct {
							Content   string `json:"content"`
							ToolCalls []struct {
								Function struct{ Arguments string } `json:"function"`
							} `json:"tool_calls"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal(e.Data, &d) == nil && len(d.Choices) > 0 {
					exact += len(d.Choices[0].Delta.Content)
					for _, tc := range d.Choices[0].Delta.ToolCalls {
						exact += len(tc.Function.Arguments)
					}
				}
				events++
				return true
			})
		} else {
			json.Unmarshal(rec.Body, &usage)
		}
		if val(t, rec.Name+" completion", r.CompletionTokens) != usage.Usage.CompletionTokens || r.ParseError != nil {
			t.Errorf("%s: completion %v, recorded %d, parse error %v", rec.Name, r.CompletionTokens, usage.Usage.CompletionTokens, r.ParseError)
		}
		if r.Stream == nil || !*r.Stream {
			continue
		}
		got := val(t, rec.Name+" output bytes", r.OutputTextBytes)
		src := val(t, rec.Name+" source", r.OutputTextBytesSource)
		if src == "exact" && got != exact {
			t.Errorf("%s: exact output bytes %d, actual %d", rec.Name, got, exact)
		}
		if src == "derived" {
			t.Logf("%s: derived output bytes %d, actual %d (%+.1f%%), %d events, %d bytes",
				rec.Name, got, exact, 100*float64(got-exact)/float64(exact), events, len(rec.Body))
			if d := got - exact; d < -exact/4 || d > exact/4 {
				t.Errorf("%s: derived output bytes off by more than 25%%", rec.Name)
			}
		}
		// TTFT in the replay is the recording's first-content time / speed,
		// plus local overhead.
		ttft := val(t, rec.Name+" ttft", r.TTFTMS)
		if want := rec.HeadersMS / speed; ttft < want*0.9 {
			t.Errorf("%s: ttft %.2f ms before the replayed headers %.2f ms", rec.Name, ttft, want)
		}
	}
	if hs.h.BudgetUsed() != 0 {
		t.Errorf("budget leaked")
	}
}
