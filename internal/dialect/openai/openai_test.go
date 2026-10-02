// SPDX-License-Identifier: Apache-2.0

package openai

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krauncher/krauncher-proxy/internal/dialect"
	"github.com/krauncher/krauncher-proxy/internal/sse"
)

// Fixture sets, one per provider.
var fixtureDirs = []string{"../../../testdata/openai/deepseek", "../../../testdata/openai/ollama-qwen35"}

type fixture struct {
	dir    string
	name   string
	req    []byte
	resp   []byte
	status int
	stream bool
}

func load(t *testing.T, fixtureDir, name string) fixture {
	t.Helper()
	f := fixture{dir: filepath.Base(fixtureDir), name: name}
	var err error
	if f.req, err = os.ReadFile(filepath.Join(fixtureDir, name+".req.json")); err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Status      int    `json:"status"`
		ContentType string `json:"content_type"`
	}
	m, _ := os.ReadFile(filepath.Join(fixtureDir, name+".meta.json"))
	json.Unmarshal(m, &meta)
	f.status, f.stream = meta.Status, strings.HasPrefix(meta.ContentType, "text/event-stream")
	ext := ".resp.json"
	if f.stream {
		ext = ".resp.sse"
	}
	if f.resp, err = os.ReadFile(filepath.Join(fixtureDir, name+ext)); err != nil {
		t.Fatal(err)
	}
	return f
}

// all loads every fixture of every provider.
func all(t *testing.T) []fixture {
	var out []fixture
	for _, dir := range fixtureDirs {
		files, _ := filepath.Glob(filepath.Join(dir, "*.req.json"))
		if len(files) < 10 {
			t.Fatalf("%s: fixtures missing", dir)
		}
		for _, f := range files {
			out = append(out, load(t, dir, strings.TrimSuffix(filepath.Base(f), ".req.json")))
		}
	}
	return out
}

func (f fixture) String() string { return f.dir + "/" + f.name }

func countEvents(b []byte) int {
	var c sse.Counter
	c.Write(b)
	return c.Events()
}

func capture(f fixture, head, tail int) dialect.ResponseCapture {
	rc := dialect.ResponseCapture{Status: f.status, Stream: f.stream, Total: int64(len(f.resp)), SSEEvents: countEvents(f.resp)}
	if head >= len(f.resp) {
		rc.Body = f.resp
		return rc
	}
	rc.Body, rc.Truncated = f.resp[:head], true
	rest := f.resp[head:]
	if len(rest) > tail {
		rc.TailSkipped = int64(len(rest) - tail)
		rest = rest[len(rest)-tail:]
	}
	rc.Tail = rest
	return rc
}

// truth is computed with encoding/json, independently of the scanner.
type truth struct {
	prompt, completion, cached, reasoning *int
	finish                                string
	outText, reasonText                   int
	overhead                              int // stream events without generated output
	hasUsage                              bool
}

type delta struct {
	Content          *string `json:"content"`
	Reasoning        *string `json:"reasoning"`
	ReasoningContent *string `json:"reasoning_content"`
	ToolCalls        []struct {
		Function struct {
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func groundTruth(t *testing.T, f fixture) truth {
	var tr truth
	apply := func(doc []byte) (output bool) {
		var d struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
				Message      delta   `json:"message"`
				Delta        delta   `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        *int `json:"prompt_tokens"`
				CompletionTokens    *int `json:"completion_tokens"`
				PromptTokensDetails *struct {
					CachedTokens *int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				CompletionTokensDetails *struct {
					ReasoningTokens *int `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(doc, &d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, c := range d.Choices {
			for _, m := range []delta{c.Message, c.Delta} {
				if m.Content != nil {
					tr.outText += len(*m.Content)
					output = output || *m.Content != ""
				}
				for _, r := range []*string{m.Reasoning, m.ReasoningContent} {
					if r != nil {
						tr.reasonText += len(*r)
						output = output || *r != ""
					}
				}
				for _, tc := range m.ToolCalls {
					tr.outText += len(tc.Function.Arguments)
					output = true
				}
			}
			if c.FinishReason != nil {
				tr.finish = *c.FinishReason
			}
		}
		if u := d.Usage; u != nil {
			tr.hasUsage = true
			tr.prompt, tr.completion = u.PromptTokens, u.CompletionTokens
			if u.PromptTokensDetails != nil {
				tr.cached = u.PromptTokensDetails.CachedTokens
			}
			if u.CompletionTokensDetails != nil {
				tr.reasoning = u.CompletionTokensDetails.ReasoningTokens
			}
		}
		return output
	}
	if !f.stream {
		apply(f.resp)
		return tr
	}
	sse.Each(f.resp, false, func(e sse.Event) bool {
		if bytes.Equal(e.Data, []byte("[DONE]")) || !apply(e.Data) {
			tr.overhead++
		}
		return true
	})
	return tr
}

func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

func TestRequestsAgainstEncodingJSON(t *testing.T) {
	for _, f := range all(t) {
		var want struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Tools     []any `json:"tools"`
			MaxTokens *int  `json:"max_tokens"`
		}
		badMessages := json.Unmarshal(f.req, &want) != nil // err_bad_request sends a string
		got := Dialect{}.ParseRequest(dialect.Chat, f.req, false)
		if got.Model == nil || *got.Model != want.Model || got.Truncated || got.ParseError != "" {
			t.Errorf("%s: model %v truncated %v err %q", f, got.Model, got.Truncated, got.ParseError)
		}
		if badMessages {
			if got.MessageCount != nil {
				t.Errorf("%s: message count from a non-array", f)
			}
			continue
		}
		text, system := 0, false
		for _, m := range want.Messages {
			text += len(m.Content)
			system = system || m.Role == "system"
		}
		if deref(got.MessageCount) != len(want.Messages) || deref(got.TextBytes) != text || *got.HasSystem != system {
			t.Errorf("%s: messages %d text %d system %v; want %d %d %v", f,
				deref(got.MessageCount), deref(got.TextBytes), *got.HasSystem, len(want.Messages), text, system)
		}
		if want.Stream != (got.Stream != nil && *got.Stream) {
			t.Errorf("%s: stream %v", f, got.Stream)
		}
		if (want.Tools == nil) != (got.ToolCount == nil) || (got.ToolCount != nil && *got.ToolCount != len(want.Tools)) {
			t.Errorf("%s: tools %v", f, got.ToolCount)
		}
		if deref(got.MaxTokens) != deref(want.MaxTokens) {
			t.Errorf("%s: max_tokens %d", f, deref(got.MaxTokens))
		}
	}
}

// Truncated requests: never a partial count, scalars only when complete.
func TestTruncatedRequests(t *testing.T) {
	for _, f := range all(t) {
		full := Dialect{}.ParseRequest(dialect.Chat, f.req, false)
		for cut := 0; cut < len(f.req); cut++ {
			got := Dialect{}.ParseRequest(dialect.Chat, f.req[:cut], true)
			if !got.Truncated {
				t.Fatalf("%s cut %d: not marked truncated", f, cut)
			}
			if got.MessageCount != nil && deref(got.MessageCount) != deref(full.MessageCount) {
				t.Fatalf("%s cut %d: partial message count %d", f, cut, *got.MessageCount)
			}
			if got.TextBytes != nil && deref(got.TextBytes) != deref(full.TextBytes) {
				t.Fatalf("%s cut %d: partial text bytes", f, cut)
			}
			if got.Model != nil && *got.Model != *full.Model {
				t.Fatalf("%s cut %d: partial model %q", f, cut, *got.Model)
			}
		}
	}
}

func TestResponsesExact(t *testing.T) {
	for _, f := range all(t) {
		if f.status >= 400 {
			continue
		}
		tr := groundTruth(t, f)
		got := Dialect{}.ParseResponse(dialect.Chat, capture(f, 1<<20, 1<<20))
		if got.ParseError != "" {
			t.Errorf("%s: parse error %q", f, got.ParseError)
		}
		switch {
		case !tr.hasUsage:
			if got.UsageSource != dialect.UsageNone {
				t.Errorf("%s: usage source %s without usage", f, got.UsageSource)
			}
		default:
			want := dialect.UsageResponse
			if f.stream {
				want = dialect.UsageStreamFinal
			}
			if got.UsageSource != want || deref(got.PromptTokens) != deref(tr.prompt) ||
				deref(got.CompletionTokens) != deref(tr.completion) || deref(got.CachedPromptTokens) != deref(tr.cached) ||
				deref(got.ReasoningTokens) != deref(tr.reasoning) {
				t.Errorf("%s: usage %s %d/%d/%d/%d, want %d/%d/%d/%d", f, got.UsageSource,
					deref(got.PromptTokens), deref(got.CompletionTokens), deref(got.CachedPromptTokens), deref(got.ReasoningTokens),
					deref(tr.prompt), deref(tr.completion), deref(tr.cached), deref(tr.reasoning))
			}
		}
		if got.FinishReason == nil || *got.FinishReason != dialect.NormalizeFinish(tr.finish) {
			t.Errorf("%s: finish %v, want %q", f, got.FinishReason, tr.finish)
		}
		if deref(got.OutputTextBytes) != tr.outText || got.OutputTextBytesSource != "exact" {
			t.Errorf("%s: output bytes %d (%s), want %d", f, deref(got.OutputTextBytes), got.OutputTextBytesSource, tr.outText)
		}
		if f.stream {
			if got.FirstContentEnd <= 0 {
				t.Errorf("%s: no first content event", f)
			}
			if got.Overhead != tr.overhead {
				t.Errorf("%s: overhead %d, want %d", f, got.Overhead, tr.overhead)
			}
		}
	}
}

// Streams captured as head + tail with a gap: usage from the tail, first
// content from the head, output bytes derived.
func TestStreamsWithGap(t *testing.T) {
	for _, f := range all(t) {
		if !f.stream || len(f.resp) < 5000 {
			continue
		}
		tr := groundTruth(t, f)
		exact := Dialect{}.ParseResponse(dialect.Chat, capture(f, 1<<20, 1<<20))
		got := Dialect{}.ParseResponse(dialect.Chat, capture(f, 2048, 2048))
		if got.UsageSource != exact.UsageSource || deref(got.CompletionTokens) != deref(tr.completion) {
			t.Errorf("%s: usage from tail %s %d", f, got.UsageSource, deref(got.CompletionTokens))
		}
		if got.FirstContentEnd != exact.FirstContentEnd {
			t.Errorf("%s: first content %d, exact %d", f, got.FirstContentEnd, exact.FirstContentEnd)
		}
		if got.Overhead != exact.Overhead || got.OutputTextBytesSource != "derived" {
			t.Errorf("%s: overhead %d (exact %d), source %q", f, got.Overhead, exact.Overhead, got.OutputTextBytesSource)
		}
		// Derived bytes include reasoning text; compare with the exact total.
		// The error is the envelope variance times the events in the gap;
		// with envelopes of hundreds of bytes around a few bytes of text it is
		// large in relative terms, so the bound is per content event.
		total := tr.outText + tr.reasonText
		events := countEvents(f.resp) - got.Overhead
		if d := deref(got.OutputTextBytes) - total; d < -8*events || d > 8*events {
			t.Errorf("%s: derived %d vs actual %d over %d events", f, deref(got.OutputTextBytes), total, events)
		}
		t.Logf("%s: derived output bytes %d, actual %d, %d events", f, deref(got.OutputTextBytes), total, events)
	}
}

// Output tokens estimated as content events, compared with reported usage.
func TestOutputTokenEstimate(t *testing.T) {
	for _, f := range all(t) {
		if !f.stream {
			continue
		}
		got := Dialect{}.ParseResponse(dialect.Chat, capture(f, 1<<20, 1<<20))
		c := deref(got.CompletionTokens)
		if c < 0 {
			continue // no usage to compare with
		}
		est := countEvents(f.resp) - got.Overhead
		t.Logf("%s: estimated %d output tokens, reported %d", f, est, c)
		// Text streams emit about one token per event. Tool-call streams emit
		// several per event (and providers count formatting tokens), so the
		// estimate is a known underestimate there: only an upper bound.
		if strings.Contains(f.name, "tools") && got.FirstContentEnd > 0 && est < c && f.dir == "deepseek" {
			continue
		}
		if est < c*75/100 || est > c*12/10 {
			t.Errorf("%s: estimate %d outside −25%%…+20%% of reported %d", f, est, c)
		}
	}
}

func TestErrors(t *testing.T) {
	want := map[string]string{
		"deepseek/err_model":            dialect.ErrInvalidRequest,
		"deepseek/err_auth":             dialect.ErrAuthentication,
		"deepseek/err_bad_request":      dialect.ErrInvalidRequest,
		"ollama-qwen35/err_model":       dialect.ErrNotFound,
		"ollama-qwen35/err_bad_request": dialect.ErrInvalidRequest,
	}
	seen := 0
	for _, f := range all(t) {
		if f.status < 400 {
			continue
		}
		seen++
		got := Dialect{}.ParseResponse(dialect.Chat, capture(f, 1<<20, 1<<20))
		if got.ErrorClass == nil || *got.ErrorClass != want[f.String()] {
			t.Errorf("%s: class %v, want %s", f, got.ErrorClass, want[f.String()])
		}
	}
	if seen != len(want) {
		t.Errorf("checked %d error fixtures, expected %d", seen, len(want))
	}
	if c := classify("", "context_length_exceeded"); c != dialect.ErrContextLength {
		t.Errorf("context_length_exceeded → %q", c)
	}
}

func TestEndpoints(t *testing.T) {
	cases := map[string]dialect.Endpoint{
		"/v1/chat/completions": dialect.Chat, "/v1/completions": dialect.Completion,
		"/v1/responses": dialect.Responses, "/v1/embeddings": dialect.Embeddings, "/v1/models": dialect.Other,
	}
	for p, want := range cases {
		if got := (Dialect{}).Endpoint(p); got != want {
			t.Errorf("%s → %s", p, got)
		}
	}
}

func TestRequestShapes(t *testing.T) {
	// Multimodal parts, developer role, tool-call arguments, responses input.
	chat := `{"model":"m","messages":[{"role":"developer","content":"ab"},` +
		`{"role":"user","content":[{"type":"text","text":"cde"},{"type":"image_url","image_url":{"url":"data:..."}}]},` +
		`{"role":"assistant","tool_calls":[{"function":{"name":"f","arguments":"{\"x\":1}"}}]}],"n":2}`
	r := Dialect{}.ParseRequest(dialect.Chat, []byte(chat), false)
	if deref(r.MessageCount) != 3 || deref(r.TextBytes) != 2+3+7 || deref(r.ImageInputs) != 1 || !*r.HasSystem || deref(r.N) != 2 {
		t.Errorf("chat: %+v", r)
	}
	emb := Dialect{}.ParseRequest(dialect.Embeddings, []byte(`{"model":"e","input":["a","bb","ccc"]}`), false)
	if deref(emb.EmbeddingInputs) != 3 || deref(emb.TextBytes) != 6 || emb.MessageCount != nil {
		t.Errorf("embeddings: %+v", emb)
	}
	resp := Dialect{}.ParseRequest(dialect.Responses, []byte(`{"model":"m","instructions":"be brief","input":"hello","max_output_tokens":9}`), false)
	if deref(resp.MessageCount) != 1 || deref(resp.TextBytes) != 8+5 || !*resp.HasSystem || deref(resp.MaxTokens) != 9 {
		t.Errorf("responses: %+v", resp)
	}
	bad := Dialect{}.ParseRequest(dialect.Chat, []byte(`{"model":"x y z"}`), false)
	if *bad.Model != "invalid" {
		t.Errorf("unsanitized model %q", *bad.Model)
	}
}
