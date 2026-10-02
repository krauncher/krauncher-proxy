// SPDX-License-Identifier: Apache-2.0

package sse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func collect(buf string, partial bool) (names, data []string) {
	Each([]byte(buf), partial, func(e Event) bool {
		names, data = append(names, string(e.Name)), append(data, string(e.Data))
		return true
	})
	return
}

func TestEach(t *testing.T) {
	in := "data: one\n\nevent: x\ndata: a\ndata: b\n\n: comment\n\ndata:two\r\n\r\ndata: partial"
	names, data := collect(in, false)
	want := []string{"one", "a\nb", "", "two"}
	if strings.Join(data, "|") != strings.Join(want, "|") || names[1] != "x" {
		t.Fatalf("data %q names %q", data, names)
	}
}

func TestPartialStart(t *testing.T) {
	_, data := collect("tail of something\n\ndata: next\n\n", true)
	if len(data) != 1 || data[0] != "next" {
		t.Fatalf("%q", data)
	}
	_, data = collect("no boundary at all", true)
	if len(data) != 0 {
		t.Fatalf("%q", data)
	}
}

func TestSpans(t *testing.T) {
	in := "data: a\n\ndata: bb\n\n"
	var spans []string
	Each([]byte(in), false, func(e Event) bool { spans = append(spans, in[e.Start:e.End]); return true })
	if len(spans) != 2 || spans[1] != "data: bb\n\n" {
		t.Fatalf("%q", spans)
	}
}

// The counter agrees with the scanner on every fixture, whatever the
// write boundaries.
func TestCounterMatchesScanner(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/openai/*/*.sse")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	files = append(files, "")
	for _, f := range files {
		buf := []byte("data: a\r\n\r\ndata: b\n\n")
		if f != "" {
			buf, _ = os.ReadFile(f)
		}
		want := 0
		Each(buf, false, func(Event) bool { want++; return true })
		for _, step := range []int{1, 2, 3, 7, 64, len(buf)} {
			var c Counter
			for i := 0; i < len(buf); i += step {
				c.Write(buf[i:min(i+step, len(buf))])
			}
			if c.Events() != want {
				t.Errorf("%s step %d: counter %d, scanner %d", f, step, c.Events(), want)
			}
		}
	}
}
