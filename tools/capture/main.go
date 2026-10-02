// SPDX-License-Identifier: Apache-2.0

// Command capture records fixtures from a real OpenAI-compatible API, for the
// dialect tests and for replaying real traffic without the model. Generated
// text, ids and tool arguments are replaced by 'x' of the same escaped length,
// so byte sizes and the JSON envelope stay exactly as the provider sent them.
// Each response is stored with its timing: when headers arrived and when each
// piece of the body arrived, so a replay can reproduce TTFT and decode pace.
// The API key is read from an environment variable and never written.
//
//	set -a; . ./.env; set +a
//	go run ./tools/capture -suite basic -base https://api.deepseek.com/v1 \
//	    -model deepseek-chat -key-env DEEPSEEK_API_KEY -out testdata/openai/deepseek
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type scenario struct {
	name     string
	model    string // empty = -model
	badKey   bool
	parallel int // >1: send this many copies at once, saved as name_c1…
	request  map[string]any
}

// redactRe matches string values that carry generated content or identifiers.
var redactRe = regexp.MustCompile(`"(content|reasoning_content|reasoning|arguments|id|system_fingerprint)":"((?:[^"\\]|\\.)*)"`)

func redact(b []byte) []byte {
	return redactRe.ReplaceAllFunc(b, func(m []byte) []byte {
		sub := redactRe.FindSubmatch(m)
		return []byte(fmt.Sprintf(`"%s":"%s"`, sub[1], strings.Repeat("x", len(sub[2]))))
	})
}

// timing is stored next to each response: offsets are cumulative body bytes
// after each read, times are milliseconds since the request was sent.
type timing struct {
	HeadersMS float64      `json:"headers_ms"`
	Points    [][2]float64 `json:"points"` // [offset, ms]
	BatchMS   float64      `json:"batch_start_ms,omitempty"`
}

func main() {
	suite := flag.String("suite", "basic", "basic | shapes")
	base := flag.String("base", "https://api.deepseek.com/v1", "API base URL")
	model := flag.String("model", "deepseek-chat", "model")
	reasoner := flag.String("reasoner", "deepseek-reasoner", "reasoning model for the basic suite, empty to skip")
	keyEnv := flag.String("key-env", "DEEPSEEK_API_KEY", "environment variable holding the API key")
	noThink := flag.String("no-think", `{"reasoning_effort":"none"}`, "JSON merged into shapes requests to disable thinking, empty for none")
	out := flag.String("out", "testdata/openai/deepseek", "output directory")
	flag.Parse()
	key := os.Getenv(*keyEnv)
	if key == "" {
		log.Fatalf("%s is not set", *keyEnv)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	var scenarios []scenario
	switch *suite {
	case "basic":
		scenarios = basicSuite(*reasoner)
	case "shapes":
		var extra map[string]any
		if *noThink != "" {
			if err := json.Unmarshal([]byte(*noThink), &extra); err != nil {
				log.Fatalf("-no-think: %v", err)
			}
		}
		scenarios = shapesSuite(extra)
	default:
		log.Fatalf("unknown suite %q", *suite)
	}

	c := &capturer{
		base: strings.TrimSuffix(*base, "/"), key: key, out: *out,
		client: &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{DisableCompression: true}},
	}
	for _, s := range scenarios {
		if s.model == "" {
			s.model = *model
		}
		s.request["model"] = s.model
		if s.parallel <= 1 {
			c.run(s, s.name, 0, time.Now())
			continue
		}
		var wg sync.WaitGroup
		batch := time.Now()
		for i := 1; i <= s.parallel; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c.run(s, fmt.Sprintf("%s_c%d", s.name, i), s.parallel, batch)
			}(i)
		}
		wg.Wait()
	}
}

type capturer struct {
	base, key, out string
	client         *http.Client
}

func (c *capturer) run(s scenario, name string, parallel int, batch time.Time) {
	body, _ := json.Marshal(s.request)
	req, _ := http.NewRequest(http.MethodPost, c.base+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	k := c.key
	if s.badKey {
		k = "sk-invalid-for-fixture"
	}
	req.Header.Set("Authorization", "Bearer "+k)

	start := time.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		log.Fatalf("%s: %v", name, err)
	}
	tm := timing{HeadersMS: msSince(start)}
	if parallel > 1 {
		tm.BatchMS = float64(start.Sub(batch).Microseconds()) / 1000
	}
	var respBody []byte
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			respBody = append(respBody, buf[:n]...)
			tm.Points = append(tm.Points, [2]float64{float64(len(respBody)), msSince(start)})
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalf("%s: %v", name, err)
		}
	}
	resp.Body.Close()

	meta := map[string]any{
		"status": resp.StatusCode, "content_type": resp.Header.Get("Content-Type"),
		"content_encoding": resp.Header.Get("Content-Encoding"), "bytes": len(respBody),
	}
	if parallel > 1 {
		meta["parallel"] = parallel
	}
	ext := ".json"
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		ext = ".sse"
	}
	write(c.out, name+".req.json", body)
	write(c.out, name+".meta.json", indent(meta))
	write(c.out, name+".timing.json", indent(tm))
	write(c.out, name+".resp"+ext, redact(respBody))
	log.Printf("%-28s %d %-30s %8d bytes  headers %7.0f ms  total %7.0f ms",
		name, resp.StatusCode, resp.Header.Get("Content-Type"), len(respBody), tm.HeadersMS, msSince(start))
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

func indent(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}

func write(dir, name string, b []byte) {
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		log.Fatal(err)
	}
}

func user(s string) map[string]any { return map[string]any{"role": "user", "content": s} }

var weatherTools = []any{map[string]any{"type": "function", "function": map[string]any{
	"name": "get_weather", "description": "Current weather for a city",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{
		"city": map[string]any{"type": "string"}}, "required": []string{"city"}},
}}}

// basicSuite covers the format: plain, length-limited, streams, tools, cache,
// errors, reasoning.
func basicSuite(reasoner string) []scenario {
	sys := map[string]any{"role": "system", "content": "You are a terse assistant."}
	// A long shared prefix (well above the provider's cache granularity) for
	// the cache-hit pair.
	longSys := map[string]any{"role": "system", "content": strings.Repeat("Policy clause: answer in one short sentence and never use lists. ", 120)}
	s := []scenario{
		{name: "chat_basic", request: map[string]any{"messages": []any{sys, user("Name one prime number.")}, "max_tokens": 40}},
		{name: "chat_length", request: map[string]any{"messages": []any{user("Count from one to fifty in words.")}, "max_tokens": 5}},
		{name: "chat_stream", request: map[string]any{"messages": []any{sys, user("Name three colors.")}, "stream": true, "max_tokens": 40}},
		{name: "chat_stream_usage", request: map[string]any{"messages": []any{sys, user("Name three colors.")}, "stream": true, "max_tokens": 40,
			"stream_options": map[string]any{"include_usage": true}}},
		{name: "chat_tools", request: map[string]any{"messages": []any{user("What is the weather in Berlin?")}, "tools": weatherTools, "max_tokens": 60}},
		{name: "chat_tools_stream_usage", request: map[string]any{"messages": []any{user("What is the weather in Paris?")}, "tools": weatherTools, "max_tokens": 60,
			"stream": true, "stream_options": map[string]any{"include_usage": true}}},
		{name: "chat_cache_first", request: map[string]any{"messages": []any{longSys, user("Say hello.")}, "max_tokens": 10}},
		{name: "chat_cache_second", request: map[string]any{"messages": []any{longSys, user("Say goodbye.")}, "max_tokens": 10}},
		{name: "err_model", model: "no-such-model", request: map[string]any{"messages": []any{user("hi")}}},
		{name: "err_auth", badKey: true, request: map[string]any{"messages": []any{user("hi")}}},
		{name: "err_bad_request", request: map[string]any{"messages": "not a list"}},
	}
	if reasoner != "" {
		s = append(s, scenario{name: "reasoner_stream_usage", model: reasoner, request: map[string]any{
			"messages": []any{user("Is 91 prime? Answer yes or no.")}, "stream": true, "max_tokens": 400,
			"stream_options": map[string]any{"include_usage": true}}})
	}
	return s
}

// filler returns deterministic prose of about n tokens (4 bytes per token).
func filler(n int) string {
	const sentence = "The river carried silt from the northern hills past mills, bridges and quiet farms toward the delta. "
	return strings.Repeat(sentence, max(1, n*4/len(sentence)))
}

// shapesSuite records realistic shapes for replay: a prompt × output grid,
// a long answer, real tool calls, and a parallel batch. extra disables
// thinking where the model would otherwise spend the budget on reasoning.
func shapesSuite(extra map[string]any) []scenario {
	stream := func(msgs []any, maxTokens int) map[string]any {
		r := map[string]any{"messages": msgs, "max_tokens": maxTokens, "stream": true,
			"stream_options": map[string]any{"include_usage": true}}
		for k, v := range extra {
			r[k] = v
		}
		return r
	}
	var s []scenario
	for _, in := range []int{512, 2048, 8192} {
		for _, out := range []int{32, 256, 1024} {
			msgs := []any{user("Read the text and then write a detailed, long commentary on it.\n\n" + filler(in))}
			s = append(s, scenario{name: fmt.Sprintf("grid_in%d_out%d", in, out), request: stream(msgs, out)})
		}
	}
	plain := stream([]any{user("Name one prime number.")}, 64)
	delete(plain, "stream")
	delete(plain, "stream_options")
	tool := stream([]any{user("What is the weather in Berlin right now? Use the tool.")}, 128)
	tool["tools"] = weatherTools
	toolPlain := stream([]any{user("What is the weather in Rome right now? Use the tool.")}, 128)
	toolPlain["tools"] = weatherTools
	delete(toolPlain, "stream")
	delete(toolPlain, "stream_options")
	s = append(s,
		scenario{name: "nothink_basic", request: plain},
		scenario{name: "tool_call_stream_usage", request: tool},
		scenario{name: "tool_call", request: toolPlain},
		scenario{name: "long_answer", request: stream([]any{user("Write a very long, detailed story about a river journey, with many chapters.")}, 6000)},
		scenario{name: "parallel_in2048_out256", parallel: 4,
			request: stream([]any{user("Read the text and then write a detailed, long commentary on it.\n\n" + filler(2048))}, 256)},
	)
	return s
}
