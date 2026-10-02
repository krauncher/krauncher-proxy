// SPDX-License-Identifier: Apache-2.0

package fakeupstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Recording is one captured response with its timing (see tools/capture).
type Recording struct {
	Name        string
	Status      int
	ContentType string
	Body        []byte
	HeadersMS   float64
	Points      [][2]float64 // [cumulative bytes, ms since request]
}

// LoadRecordings reads every recording in dir that has a timing file.
func LoadRecordings(dir string) ([]Recording, error) {
	timings, err := filepath.Glob(filepath.Join(dir, "*.timing.json"))
	if err != nil {
		return nil, err
	}
	var out []Recording
	for _, tf := range timings {
		name := strings.TrimSuffix(filepath.Base(tf), ".timing.json")
		var meta struct {
			Status      int    `json:"status"`
			ContentType string `json:"content_type"`
		}
		var tm struct {
			HeadersMS float64      `json:"headers_ms"`
			Points    [][2]float64 `json:"points"`
		}
		if err := readJSON(filepath.Join(dir, name+".meta.json"), &meta); err != nil {
			return nil, err
		}
		if err := readJSON(tf, &tm); err != nil {
			return nil, err
		}
		body, err := os.ReadFile(filepath.Join(dir, name+".resp.sse"))
		if err != nil {
			body, err = os.ReadFile(filepath.Join(dir, name+".resp.json"))
		}
		if err != nil {
			return nil, fmt.Errorf("%s: no response body", name)
		}
		if n := len(tm.Points); n == 0 || int(tm.Points[n-1][0]) != len(body) {
			return nil, fmt.Errorf("%s: timing does not cover the body", name)
		}
		out = append(out, Recording{name, meta.Status, meta.ContentType, body, tm.HeadersMS, tm.Points})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no recordings", dir)
	}
	return out, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Replay serves recordings with their original timing divided by speed. The
// recording is chosen by the X-Fixture header, otherwise round-robin. The
// request body is read and ignored: the answer is the recording.
func Replay(recs []Recording, speed float64) http.Handler {
	byName := map[string]*Recording{}
	for i := range recs {
		byName[recs[i].Name] = &recs[i]
	}
	var next atomic.Uint64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		io.Copy(io.Discard, r.Body)
		rec := byName[r.Header.Get("X-Fixture")]
		if rec == nil {
			rec = &recs[int(next.Add(1)-1)%len(recs)]
		}
		at := func(ms float64) bool { // wait until ms (scaled) after start
			return sleep(r, time.Until(start.Add(time.Duration(ms/speed*float64(time.Millisecond)))))
		}
		if !at(rec.HeadersMS) {
			return
		}
		w.Header().Set("Content-Type", rec.ContentType)
		w.WriteHeader(rec.Status)
		rc := http.NewResponseController(w)
		prev := 0
		for _, p := range rec.Points {
			if !at(p[1]) {
				return
			}
			end := int(p[0])
			if _, err := w.Write(rec.Body[prev:end]); err != nil {
				return
			}
			rc.Flush()
			prev = end
		}
	})
}
