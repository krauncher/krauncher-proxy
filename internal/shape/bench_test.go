// SPDX-License-Identifier: Apache-2.0

package shape

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/capture"
	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/sse"
)

// BenchmarkAssemble measures stage 2 per request: request and response jobs
// for a captured stream (parse, estimate, build the record).
func BenchmarkAssemble(b *testing.B) {
	for _, name := range []string{"chat_stream_usage", "reasoner_stream_usage"} {
		dir := "../../testdata/openai/deepseek"
		req, _ := os.ReadFile(filepath.Join(dir, name+".req.json"))
		resp, _ := os.ReadFile(filepath.Join(dir, name+".resp.sse"))
		var c sse.Counter
		c.Write(resp)
		b.Run(name, func(b *testing.B) {
			budget := capture.NewBudget(1 << 30)
			asm := &Assembler{Instance: "b", Estimate: config.Default().Estimate, RequestWait: time.Second, MaxBody: 1 << 20, Out: func(Record) {}}
			t0 := time.Now()
			b.ReportAllocs()
			for range b.N {
				p := capture.NewPending()
				p.Route, p.Dialect, p.Path, p.Captured, p.Stream, p.Status = "r", config.DialectOpenAI, "/v1/chat/completions", true, true, 200
				p.T0, p.Headers, p.End, p.RespBytes, p.SSE = t0, t0, t0.Add(time.Second), int64(len(resp)), c
				p.Req = capture.NewBuffer(1<<20, 64<<10, budget)
				p.Req.Write(req)
				p.Head = capture.NewBuffer(16<<10, 64<<10, budget)
				p.Head.Write(resp)
				p.AddPoint(int64(len(resp)), t0.Add(100*time.Millisecond), 256)
				asm.Handle(Job{P: p})
				asm.Handle(Job{P: p, Response: true})
			}
		})
	}
}
