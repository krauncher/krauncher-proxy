// SPDX-License-Identifier: Apache-2.0

// Package openai parses the OpenAI-compatible API format: chat completions,
// completions, responses and embeddings, plain JSON and SSE streams.
package openai

import (
	"strconv"

	"github.com/krauncher/krauncher-proxy/internal/dialect"
	"github.com/krauncher/krauncher-proxy/internal/jsonscan"
)

// Dialect implements dialect.Dialect.
type Dialect struct{}

func (Dialect) Name() string { return "openai" }

var endpoints = []dialect.Suffix{
	{Suffix: "/chat/completions", Endpoint: dialect.Chat},
	{Suffix: "/completions", Endpoint: dialect.Completion},
	{Suffix: "/responses", Endpoint: dialect.Responses},
	{Suffix: "/embeddings", Endpoint: dialect.Embeddings},
}

func (Dialect) Endpoint(path string) dialect.Endpoint { return dialect.MatchSuffix(path, endpoints) }

// nonTextPart lists content part types that are not text (not estimated).
var nonTextPart = map[string]bool{
	"image_url": true, "input_image": true, "image": true,
	"input_audio": true, "audio": true, "file": true, "input_file": true,
}

// reqVisitor collects request fields. Counts and text sizes are published
// only when their enclosing array closed (see doc/protocols.md).
type reqVisitor struct {
	r dialect.Request

	listKey     string // "messages" or "input": the array holding the prompt
	listClosed  bool
	count       int
	text, image int
	sawSystem   bool
	inputString bool // "input" given as a single string
}

func intOf(raw []byte) *int {
	v, err := strconv.Atoi(string(raw))
	if err != nil {
		return nil
	}
	return &v
}

func (v *reqVisitor) Start(jsonscan.Path, jsonscan.Kind, int) {}

func (v *reqVisitor) Value(p jsonscan.Path, k jsonscan.Kind, raw []byte, _ int) {
	switch {
	case p.Match("model") && k == jsonscan.String:
		if s, ok := jsonscan.Unquote(raw); ok {
			s = dialect.SanitizeName(s)
			v.r.Model = &s
		}
	case p.Match("stream") && k == jsonscan.Bool:
		b := raw[0] == 't'
		v.r.Stream = &b
	case (p.Match("max_tokens") || p.Match("max_completion_tokens") || p.Match("max_output_tokens")) && k == jsonscan.Number:
		v.r.MaxTokens = intOf(raw)
	case p.Match("n") && k == jsonscan.Number:
		v.r.N = intOf(raw)
	case p.Match("instructions") && k == jsonscan.String:
		v.sawSystem = true
		v.text += jsonscan.DecodedLen(raw)
	case p.Match("input") && k == jsonscan.String:
		v.listKey, v.listClosed, v.count, v.inputString = "input", true, 1, true
		v.text += jsonscan.DecodedLen(raw)

	// messages[*] (chat) and input[*] (responses, embeddings)
	case (p.Match("messages", "*", "role") || p.Match("input", "*", "role")) && k == jsonscan.String:
		if s := string(raw); s == `"system"` || s == `"developer"` {
			v.sawSystem = true
		}
	case p.Match("input", "*") && k == jsonscan.String: // embeddings / simple responses input
		v.text += jsonscan.DecodedLen(raw)
	case (p.Match("messages", "*", "content") || p.Match("input", "*", "content")) && k == jsonscan.String,
		(p.Match("messages", "*", "content", "*", "text") || p.Match("input", "*", "content", "*", "text")) && k == jsonscan.String,
		p.Match("messages", "*", "tool_calls", "*", "function", "arguments") && k == jsonscan.String:
		v.text += jsonscan.DecodedLen(raw)
	case (p.Match("messages", "*", "content", "*", "type") || p.Match("input", "*", "content", "*", "type")) && k == jsonscan.String:
		if s, ok := jsonscan.Unquote(raw); ok && nonTextPart[s] {
			v.image++
		}
	}
}

func (v *reqVisitor) End(p jsonscan.Path, k jsonscan.Kind, n, start, end int) {
	switch {
	case p.Match("messages") && k == jsonscan.Array:
		v.listKey, v.listClosed, v.count = "messages", true, n
	case p.Match("input") && k == jsonscan.Array:
		v.listKey, v.listClosed, v.count = "input", true, n
	case p.Match("tools") && k == jsonscan.Array:
		v.r.ToolCount = &n
		v.r.ToolBytes = end - start
	}
}

func (Dialect) ParseRequest(ep dialect.Endpoint, buf []byte, truncated bool) dialect.Request {
	var v reqVisitor
	complete, err := jsonscan.Scan(buf, &v)
	r := v.r
	r.Truncated = truncated || !complete
	if err != nil {
		r.ParseError = "request_syntax"
	}
	if v.listClosed {
		count, text, image := v.count, v.text, v.image
		if ep == dialect.Embeddings {
			r.EmbeddingInputs = &count
		} else {
			r.MessageCount = &count
		}
		r.TextBytes, r.ImageInputs = &text, &image
	}
	if v.sawSystem || v.listClosed {
		has := v.sawSystem
		r.HasSystem = &has
	}
	if ep == dialect.Embeddings {
		r.HasSystem, r.MessageCount = nil, nil
	}
	return r
}
