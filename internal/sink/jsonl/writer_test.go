// SPDX-License-Identifier: Apache-2.0

package jsonl

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/shape"
)

func testCfg(dir string) config.JSONL {
	c := config.Default().Sink.JSONL
	c.Enabled, c.Dir = true, dir
	return c
}

// fakeClock advances one second per call, so every file gets a distinct name.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

func record(id string) shape.Record {
	return shape.Record{
		V: shape.Version, ID: id, Instance: "inst", Route: "main", Dialect: "openai", Endpoint: "other",
		Status: 200, Outcome: shape.OutcomeOK, UsageSource: shape.UsageNone, LatencyMS: 635.5,
		Capture: shape.CaptureSkipped, Time: time.Date(2026, 1, 15, 10, 0, 2, 364_500_000, time.UTC),
	}
}

// readAll returns every line of every file, decompressing .gz.
func readAll(t *testing.T, dir string) []map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	var out []map[string]any
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		var r io.Reader = f
		if strings.HasSuffix(name, ".gz") {
			if r, err = gzip.NewReader(f); err != nil {
				t.Fatal(err)
			}
		}
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			out = append(out, m)
		}
		f.Close()
	}
	return out
}

func TestWriteFlushOnCloseFormatAndPermissions(t *testing.T) {
	cfg := testCfg(t.TempDir())
	cfg.Gzip = false
	w, err := Open(cfg, "inst", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	w.Submit(record("a"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	dir := filepath.Join(cfg.Dir, "inst")
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v, want 0700", st.Mode().Perm())
	}
	files, _ := filepath.Glob(filepath.Join(dir, "shape-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files %v", files)
	}
	if st, _ := os.Stat(files[0]); st.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v, want 0600", st.Mode().Perm())
	}
	data, _ := os.ReadFile(files[0])
	line := string(data)
	if !strings.HasPrefix(line, `{"v":1,"ts":"2026-01-15T10:00:02.364Z",`) {
		t.Errorf("line starts %q", line[:min(60, len(line))])
	}
	for _, want := range []string{`"model":null`, `"upload_ms":null`, `"latency_ms":635.5`, `"capture":"skipped"`} {
		if !strings.Contains(line, want) {
			t.Errorf("line lacks %s: %s", want, line)
		}
	}
}

func TestTimeResolution(t *testing.T) {
	cfg := testCfg(t.TempDir())
	cfg.TimeResolution = time.Minute
	w, _ := Open(cfg, "inst", slog.Default())
	w.Submit(record("a"))
	w.Close()
	recs := readAll(t, filepath.Join(cfg.Dir, "inst"))
	if len(recs) != 1 || recs[0]["ts"] != "2026-01-15T10:00:00.000Z" {
		t.Fatalf("records %v", recs)
	}
}

func TestRotationCompressionAndNoLoss(t *testing.T) {
	cfg := testCfg(t.TempDir())
	cfg.MaxBytes = 2 * config.KiB
	cfg.Batch = 1
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	w, err := open(cfg, "inst", slog.Default(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		w.Submit(record(strings.Repeat("x", i%7)))
	}
	w.Close()
	dir := filepath.Join(cfg.Dir, "inst")
	gz, _ := filepath.Glob(filepath.Join(dir, "*.gz"))
	plain, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(gz) < 2 || len(plain) != 0 {
		t.Fatalf("gz %d plain %d: want several compressed files and none plain after close", len(gz), len(plain))
	}
	if n := len(readAll(t, dir)); n != 50 {
		t.Fatalf("read %d records, want 50", n)
	}
}

func TestRetentionByTotalSize(t *testing.T) {
	cfg := testCfg(t.TempDir())
	cfg.Gzip = false
	cfg.MaxBytes = 1 * config.KiB
	cfg.MaxTotalBytes = 4 * config.KiB
	cfg.Batch = 1
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	w, _ := open(cfg, "inst", slog.Default(), clk.now)
	for range 100 {
		w.Submit(record("a"))
	}
	w.Close()
	var total int64
	files, _ := filepath.Glob(filepath.Join(cfg.Dir, "inst", "*"))
	for _, f := range files {
		st, _ := os.Stat(f)
		total += st.Size()
	}
	// After Close every file is finished and retention has run on all of them.
	if total > int64(cfg.MaxTotalBytes) {
		t.Fatalf("total %d bytes in %d files exceeds the limit", total, len(files))
	}
}

func TestRotationByAgeAndRetentionByAge(t *testing.T) {
	cfg := testCfg(t.TempDir())
	cfg.Gzip, cfg.MaxAge, cfg.FlushInterval, cfg.Batch = false, 50*time.Millisecond, 10*time.Millisecond, 1
	dir := filepath.Join(cfg.Dir, "inst")
	os.MkdirAll(dir, 0o700)
	old := filepath.Join(dir, "shape-20000101T000000Z.jsonl")
	os.WriteFile(old, []byte("{}\n"), 0o600)
	past := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(old, past, past)

	w, err := Open(cfg, "inst", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	w.Submit(record("a"))
	time.Sleep(200 * time.Millisecond) // past max_age: the ticker rotates
	w.Submit(record("b"))
	w.Close()
	files, _ := filepath.Glob(filepath.Join(dir, "shape-*.jsonl"))
	fileOf := map[string]string{}
	for _, f := range files {
		data, _ := os.ReadFile(f)
		if len(data) == 0 {
			t.Errorf("empty file left: %s", f)
		}
		for _, id := range []string{"a", "b"} {
			if strings.Contains(string(data), `"id":"`+id+`"`) {
				fileOf[id] = f
			}
		}
	}
	if fileOf["a"] == "" || fileOf["b"] == "" || fileOf["a"] == fileOf["b"] {
		t.Errorf("records not split by age rotation: %v", fileOf)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("file older than retention not deleted")
	}
}

// A sink that cannot write counts errors and keeps running.
func TestWriteErrorsAreCountedNotFatal(t *testing.T) {
	cfg := testCfg(t.TempDir())
	cfg.Gzip, cfg.MaxBytes, cfg.Batch = false, 200, 1
	w, err := Open(cfg, "inst", log())
	if err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(cfg.Dir) // the next rotation cannot create a file
	for i := range 20 {
		w.Submit(record(strings.Repeat("x", i)))
	}
	w.Close()
	if w.WriteErrors() == 0 {
		t.Fatal("write errors not counted")
	}
}

func log() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
