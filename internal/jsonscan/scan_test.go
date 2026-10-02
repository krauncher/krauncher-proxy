// SPDX-License-Identifier: Apache-2.0

package jsonscan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// collector records the walk as strings for comparison.
type collector struct {
	values []string
	ends   []string
}

func pathString(p Path) string {
	var b strings.Builder
	for _, s := range p {
		if s.Key != nil {
			b.WriteString("." + string(s.Key))
		} else {
			b.WriteString("[]")
		}
	}
	return b.String()
}

func (c *collector) Start(Path, Kind, int) {}
func (c *collector) Value(p Path, k Kind, raw []byte, off int) {
	c.values = append(c.values, pathString(p)+"="+string(raw))
}
func (c *collector) End(p Path, k Kind, n, start, end int) {
	c.ends = append(c.ends, pathString(p))
}

func fixtures(t testing.TB) [][]byte {
	files, _ := filepath.Glob("../../testdata/openai/*/*.json")
	var out [][]byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if json.Valid(b) {
			out = append(out, b)
		}
	}
	out = append(out,
		[]byte(`{"a":[1,-2.5e3,true,false,null,"x\"y\\u00e9"],"b":{},"c":[],"d":"😀"}`),
		[]byte(`  [ {"k" : "v"} , [ ] ]  `),
	)
	if len(out) < 10 {
		t.Fatalf("only %d fixtures found", len(out))
	}
	return out
}

func TestCompleteDocuments(t *testing.T) {
	for _, doc := range fixtures(t) {
		var c collector
		complete, err := Scan(doc, &c)
		if err != nil || !complete {
			t.Fatalf("complete=%v err=%v on %s", complete, err, doc)
		}
		for _, v := range c.values {
			raw := v[strings.Index(v, "=")+1:]
			if !json.Valid([]byte(raw)) {
				t.Errorf("reported scalar %q is not valid JSON", raw)
			}
		}
	}
}

// Truncating at every byte offset never panics, never reports a container
// that was not closed in the prefix, and every reported scalar is complete.
func TestTruncationAtEveryOffset(t *testing.T) {
	for _, doc := range fixtures(t) {
		var full collector
		Scan(doc, &full)
		for cut := 0; cut < len(doc); cut++ {
			var c collector
			complete, err := Scan(doc[:cut], &c)
			if err != nil {
				t.Fatalf("cut %d: error %v on a valid prefix", cut, err)
			}
			if complete && strings.TrimSpace(string(doc[cut:])) != "" {
				t.Fatalf("cut %d: reported complete", cut)
			}
			// Prefix events must be a prefix of the full walk.
			if len(c.values) > len(full.values) || len(c.ends) > len(full.ends) {
				t.Fatalf("cut %d: more events than the full document", cut)
			}
			for i := range c.values {
				if c.values[i] != full.values[i] {
					t.Fatalf("cut %d: value %d = %q, full has %q", cut, i, c.values[i], full.values[i])
				}
			}
			for i := range c.ends {
				if c.ends[i] != full.ends[i] {
					t.Fatalf("cut %d: end %d differs", cut, i)
				}
			}
		}
	}
}

func TestSyntaxErrors(t *testing.T) {
	for _, bad := range []string{`{"a" 1}`, `[1 2]`, `{a:1}`, `tru e`, `{"a":1,}x`, "\"a\nb\""} {
		if _, err := Scan([]byte(bad), &collector{}); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestMatch(t *testing.T) {
	p := Path{{Key: []byte("messages")}, {Index: 3}, {Key: []byte("content")}}
	if !p.Match("messages", "*", "content") || p.Match("messages", "content") || p.Match("messages", "*", "role") {
		t.Fatal("Match")
	}
}

func TestDecodedLen(t *testing.T) {
	for _, s := range []string{"", "abc", "é", "😀", "a\"b\\c\n\t", " ", "x\x01y"} {
		raw, _ := json.Marshal(s)
		if got := DecodedLen(raw); got != len(s) {
			t.Errorf("%q (%s): DecodedLen %d, want %d", s, raw, got, len(s))
		}
	}
	// Escaped forms as other encoders write them.
	cases := map[string]int{`"é"`: 2, `"😀"`: 4, `"\ud83d"`: 3, `"\/"`: 1, `"Aé"`: 3}
	for raw, want := range cases {
		var s string
		json.Unmarshal([]byte(raw), &s)
		if got := DecodedLen([]byte(raw)); got != want || len(s) != want {
			t.Errorf("%s: DecodedLen %d, encoding/json %d, want %d", raw, got, len(s), want)
		}
	}
}

func FuzzScan(f *testing.F) {
	for _, d := range fixtures(f) {
		f.Add(d)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var c collector
		complete, err := Scan(data, &c)
		// No false negatives: every valid document is read completely.
		if json.Valid(data) && (!complete || err != nil) {
			t.Fatalf("valid JSON %q: complete=%v err=%v", data, complete, err)
		}
		for _, v := range c.values {
			raw := v[strings.Index(v, "=")+1:]
			if strings.HasPrefix(raw, `"`) {
				var s string
				if json.Unmarshal([]byte(raw), &s) == nil && DecodedLen([]byte(raw)) != len(s) {
					t.Fatalf("DecodedLen(%s) = %d, want %d", raw, DecodedLen([]byte(raw)), len(s))
				}
			}
		}
	})
}
