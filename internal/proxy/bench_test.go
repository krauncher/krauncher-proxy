// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krauncher/krauncher-proxy/internal/capture"
	"github.com/krauncher/krauncher-proxy/internal/config"
)

// sseChunk is a typical ~250-byte OpenAI-style stream event.
var sseChunk = []byte(`data: {"id":"chatcmpl-0123456789","object":"chat.completion.chunk","created":1790935197,"model":"model-name","choices":[{"index":0,"delta":{"content":"tok "},"logprobs":null,"finish_reason":null}]}` + "\n\n")

// BenchmarkResponseWrite measures the data-plane cost per streamed chunk:
// head/tail capture, SSE counter, timeline.
func BenchmarkResponseWrite(b *testing.B) {
	for _, capOn := range []bool{false, true} {
		name := "capture_off"
		if capOn {
			name = "capture_on"
		}
		b.Run(name, func(b *testing.B) {
			c := config.Default().Capture
			h := &Handler{capture: c, budget: capture.NewBudget(int64(c.BudgetBytes))}
			rec := httptest.NewRecorder()
			p := capture.NewPending()
			p.Captured = capOn
			w := &countingWriter{ResponseWriter: discard{rec.Header()}, h: h, p: p}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			b.SetBytes(int64(len(sseChunk)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				w.Write(sseChunk)
			}
			b.StopTimer()
			p.ReleaseResponse()
		})
	}
}

// discard is a ResponseWriter that drops the body.
type discard struct{ h http.Header }

func (d discard) Header() http.Header         { return d.h }
func (d discard) Write(p []byte) (int, error) { return len(p), nil }
func (d discard) WriteHeader(int)             {}

// BenchmarkRequestBody measures reading a 2 KiB request body through the
// counting and capturing wrapper.
func BenchmarkRequestBody(b *testing.B) {
	body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("a", 2000) + `"}]}`)
	c := config.Default().Capture
	budget := capture.NewBudget(int64(c.BudgetBytes))
	buf := make([]byte, 32<<10)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for range b.N {
		p := capture.NewPending()
		cb := &countingBody{ReadCloser: nopCloser{bytes.NewReader(body)}, p: p, length: int64(len(body)),
			emit: func(p *capture.Pending, _ bool) bool { p.Req.Release(); return true },
			buf:  capture.NewBuffer(int(c.RequestMaxBytes), int(c.BudgetStep), budget)}
		for {
			if _, err := cb.Read(buf); err != nil {
				break
			}
		}
	}
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
