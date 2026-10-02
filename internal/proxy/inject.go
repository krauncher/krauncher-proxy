// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/capture"
	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/jsonscan"
)

// maybeInject implements stream_usage: inject (doc/protocols.md): for a
// streaming OpenAI-style request without stream_options.include_usage, add it,
// so the stream ends with a usage chunk. Only when the whole body fits the
// request capture cap; otherwise the request passes unmodified.
//
// The body is read from the client here, before forwarding, so the client's
// upload ends now, not when the transport reads the buffered copy. The
// returned pre-read records that moment and how many bytes the proxy added,
// so the record keeps the client's own upload time and body size.
func (h *Handler) maybeInject(r *http.Request, p *capture.Pending, rc config.Route) (pre preRead) {
	if rc.Dialect != config.DialectOpenAI || r.Method != http.MethodPost ||
		r.ContentLength <= 0 || r.ContentLength > int64(h.capture.RequestMaxBytes) {
		return pre
	}
	orig, err := io.ReadAll(io.LimitReader(r.Body, r.ContentLength))
	rest := r.Body
	body := orig
	if err == nil && int64(len(orig)) == r.ContentLength {
		pre.done = time.Now()
		if nb, ok := injectUsage(orig); ok {
			body, p.Injected = nb, true
			pre.added = int64(len(nb) - len(orig))
			r.ContentLength = int64(len(nb))
			r.Header.Set("Content-Length", strconv.Itoa(len(nb)))
		}
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), rest), rest}
	return pre
}

// preRead describes a request body read by the proxy before forwarding.
type preRead struct {
	done  time.Time // when the client's body was fully read; zero if not
	added int64     // bytes the proxy inserted
}

// injectVisitor finds what injectUsage needs at the top level.
type injectVisitor struct {
	rootIsObject bool
	root         int  // offset of the root '{'
	stream       bool // "stream": true

	soPresent    bool // "stream_options" exists, with any value
	soIsObject   bool
	soStart      int // offset of its '{', or -1
	soMembers    int
	includeUsage bool // stream_options.include_usage exists, with any value
}

func (v *injectVisitor) Start(p jsonscan.Path, k jsonscan.Kind, off int) {
	switch {
	case len(p) == 0 && k == jsonscan.Object:
		v.root, v.rootIsObject = off, true
	case p.Match("stream_options"):
		v.soPresent = true
		if k == jsonscan.Object {
			v.soStart, v.soIsObject = off, true
		}
	}
}

func (v *injectVisitor) Value(p jsonscan.Path, k jsonscan.Kind, raw []byte, _ int) {
	switch {
	case p.Match("stream") && k == jsonscan.Bool:
		v.stream = raw[0] == 't'
	case p.Match("stream_options"):
		v.soPresent = true // a scalar (e.g. null): leave the request alone
	case p.Match("stream_options", "include_usage"):
		v.includeUsage = true // present with any value: the client decided
	}
}

func (v *injectVisitor) End(p jsonscan.Path, k jsonscan.Kind, n, _, _ int) {
	if p.Match("stream_options") && k == jsonscan.Object {
		v.soMembers = n
	}
}

// injectUsage returns the body with stream_options.include_usage = true added
// by inserting bytes, leaving everything else as sent. ok is false when the
// body must not be changed.
func injectUsage(body []byte) ([]byte, bool) {
	v := injectVisitor{soStart: -1}
	complete, err := jsonscan.Scan(body, &v)
	if err != nil || !complete || !v.rootIsObject || !v.stream || v.includeUsage {
		return nil, false
	}
	var at int
	var ins string
	switch {
	case !v.soPresent:
		at, ins = v.root+1, `"stream_options":{"include_usage":true},`
	case v.soIsObject && v.soMembers == 0:
		at, ins = v.soStart+1, `"include_usage":true`
	case v.soIsObject:
		at, ins = v.soStart+1, `"include_usage":true,`
	default:
		return nil, false
	}
	out := make([]byte, 0, len(body)+len(ins))
	out = append(append(append(out, body[:at]...), ins...), body[at:]...)
	return out, true
}
