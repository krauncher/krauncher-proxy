// SPDX-License-Identifier: Apache-2.0

// Package fakeupstream simulates an OpenAI-compatible chat completions API for
// tests, demos and load generation. Answers are placeholder tokens; timing and
// sizes are what matters.
package fakeupstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Options set the default behaviour; request headers override it per request:
// X-Fake-Tokens, X-Fake-Header-Delay, X-Fake-Token-Interval, X-Fake-Status.
type Options struct {
	Tokens        int           // completion length when the request sets no max_tokens
	HeaderDelay   time.Duration // before response headers (prefill)
	TokenInterval time.Duration // between streamed tokens (decode)
}

type chatRequest struct {
	Model         string `json:"model"`
	Stream        bool   `json:"stream"`
	MaxTokens     int    `json:"max_tokens"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Messages []struct {
		Content string `json:"content"`
	} `json:"messages"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Handler serves POST .../chat/completions; everything else is 404.
func Handler(o Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error")
			return
		}
		tokens := headerInt(r, "X-Fake-Tokens", o.Tokens)
		if req.MaxTokens > 0 && req.MaxTokens < tokens {
			tokens = req.MaxTokens
		}
		headerDelay := headerDuration(r, "X-Fake-Header-Delay", o.HeaderDelay)
		interval := headerDuration(r, "X-Fake-Token-Interval", o.TokenInterval)
		promptBytes := 0
		for _, m := range req.Messages {
			promptBytes += len(m.Content)
		}
		u := usage{PromptTokens: max(promptBytes/4, 1), CompletionTokens: tokens}
		u.TotalTokens = u.PromptTokens + u.CompletionTokens

		if !sleep(r, headerDelay) {
			return
		}
		if status := headerInt(r, "X-Fake-Status", 0); status >= 400 {
			writeError(w, status, errorType(status))
			return
		}
		id, created := "chatcmpl-fake", time.Now().Unix()
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id": id, "object": "chat.completion", "created": created, "model": req.Model,
				"choices": []any{map[string]any{
					"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": strings.Repeat("tok ", tokens)},
				}},
				"usage": u,
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		rc := http.NewResponseController(w)
		send := func(v any) bool {
			b, _ := json.Marshal(v)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return false
			}
			return rc.Flush() == nil
		}
		chunk := func(delta map[string]any, finish any) map[string]any {
			return map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
			}
		}
		if !send(chunk(map[string]any{"role": "assistant", "content": ""}, nil)) {
			return
		}
		for i := 0; i < tokens; i++ {
			if i > 0 && !sleep(r, interval) {
				return
			}
			if !send(chunk(map[string]any{"content": "tok "}, nil)) {
				return
			}
		}
		if !send(chunk(map[string]any{}, "stop")) {
			return
		}
		if req.StreamOptions.IncludeUsage {
			if !send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created,
				"model": req.Model, "choices": []any{}, "usage": u}) {
				return
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		rc.Flush()
	})
}

// sleep waits d unless the client goes away first.
func sleep(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return r.Context().Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

func writeError(w http.ResponseWriter, status int, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": typ, "message": "simulated"}})
}

func errorType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

func headerInt(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.Header.Get(name)); err == nil {
		return v
	}
	return def
}

func headerDuration(r *http.Request, name string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(r.Header.Get(name)); err == nil {
		return v
	}
	return def
}
