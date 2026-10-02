// SPDX-License-Identifier: Apache-2.0

package fakeupstream

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReplayReproducesBytesAndPace(t *testing.T) {
	recs, err := LoadRecordings("../../testdata/replay/ollama-qwen35")
	if err != nil {
		t.Fatal(err)
	}
	const speed = 20
	srv := httptest.NewServer(Replay(recs, speed))
	defer srv.Close()
	for _, name := range []string{"grid_in512_out256", "tool_call"} {
		var rec Recording
		for _, r := range recs {
			if r.Name == name {
				rec = r
			}
		}
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader("{}"))
		req.Header.Set("X-Fixture", name)
		start := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		took := time.Since(start)
		if !bytes.Equal(body, rec.Body) || resp.StatusCode != rec.Status {
			t.Errorf("%s: body or status differs", name)
		}
		want := time.Duration(rec.Points[len(rec.Points)-1][1] / speed * float64(time.Millisecond))
		if took < want*9/10 {
			t.Errorf("%s: replayed in %v, recording took %v at %dx", name, took, want, speed)
		}
	}
}
