// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"testing"
)

func TestInjectUsage(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"model":"m","stream":true,"messages":[]}`, `{"stream_options":{"include_usage":true},"model":"m","stream":true,"messages":[]}`},
		{` { "stream" : true }`, ` {"stream_options":{"include_usage":true}, "stream" : true }`},
		{`{"stream":true,"stream_options":{}}`, `{"stream":true,"stream_options":{"include_usage":true}}`},
		{`{"stream":true,"stream_options":{"x":1}}`, `{"stream":true,"stream_options":{"include_usage":true,"x":1}}`},
	}
	for _, c := range cases {
		got, ok := injectUsage([]byte(c.in))
		if !ok || string(got) != c.want {
			t.Errorf("%s → %s, %v; want %s", c.in, got, ok, c.want)
		}
		if !json.Valid(got) {
			t.Errorf("%s: result is not valid JSON", c.in)
		}
	}
	unchanged := []string{
		`{"stream":false}`, `{"model":"m"}`,
		`{"stream":true,"stream_options":{"include_usage":false}}`, // the client decided
		`{"stream":true,"stream_options":{"include_usage":true}}`,
		`{"stream":true,"stream_options":null}`,
		`{"stream":true`, `[1]`, `{"stream":tru}`,
	}
	for _, in := range unchanged {
		if out, ok := injectUsage([]byte(in)); ok {
			t.Errorf("%s changed to %s", in, out)
		}
	}
}
