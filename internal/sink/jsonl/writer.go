// SPDX-License-Identifier: Apache-2.0

// Package jsonl writes shape records as JSON Lines with batching, rotation,
// compression and retention. See doc/outputs.md.
package jsonl

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/config"
	"github.com/krauncher/krauncher-proxy/internal/pipeline"
	"github.com/krauncher/krauncher-proxy/internal/shape"
)

const (
	filePrefix = "shape-"
	fileSuffix = ".jsonl"
	tsLayout   = "2006-01-02T15:04:05.000Z"
)

// Writer appends records to the active file of <dir>/<instance>/.
type Writer struct {
	cfg config.JSONL
	dir string
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex // guards the active file
	f       *os.File
	buf     *bufio.Writer
	enc     *json.Encoder
	size    int64
	opened  time.Time
	pending int

	q    *pipeline.Queue[shape.Record]
	stop chan struct{}
	tick sync.WaitGroup
	bg   sync.Mutex // serializes compression and retention
	bgWG sync.WaitGroup

	writeErrors atomic.Uint64
	closeOnce   sync.Once
	closeErr    error
}

// Open creates the directory (mode 0700) and the first file (mode 0600).
func Open(cfg config.JSONL, instance string, log *slog.Logger) (*Writer, error) {
	return open(cfg, instance, log, time.Now)
}

func open(cfg config.JSONL, instance string, log *slog.Logger, now func() time.Time) (*Writer, error) {
	w := &Writer{cfg: cfg, dir: filepath.Join(cfg.Dir, instance), log: log, now: now, stop: make(chan struct{})}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(w.dir, 0o700); err != nil {
		return nil, err
	}
	w.mu.Lock()
	err := w.openFile()
	w.mu.Unlock()
	if err != nil {
		return nil, err
	}
	w.q = pipeline.New(cfg.QueueSize, 1, w.write)
	w.tick.Add(1)
	go w.ticker()
	return w, nil
}

// Submit queues a record without blocking; false means it was dropped.
func (w *Writer) Submit(r shape.Record) bool { return w.q.Submit(r) }

// Dropped returns records dropped because the queue was full.
func (w *Writer) Dropped() uint64 { return w.q.Dropped() }

// WriteErrors returns the number of failed writes.
func (w *Writer) WriteErrors() uint64 { return w.writeErrors.Load() }

// Close writes everything queued, closes and finalizes the active file.
// Calling it again returns the first result.
func (w *Writer) Close() error {
	w.closeOnce.Do(func() {
		close(w.stop)
		w.tick.Wait()
		w.q.Close()
		w.mu.Lock()
		w.closeErr = w.finishFile()
		w.mu.Unlock()
		w.bgWG.Wait()
	})
	return w.closeErr
}

func (w *Writer) write(r shape.Record) {
	r.TS = r.Time.UTC().Truncate(w.cfg.TimeResolution).Format(tsLayout)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.openFile(); err != nil {
			w.fail("open", err)
			return
		}
	}
	if err := w.enc.Encode(r); err != nil {
		w.fail("encode", err)
		return
	}
	w.pending++
	if w.pending >= w.cfg.Batch {
		w.flush()
	}
	if w.size >= int64(w.cfg.MaxBytes) {
		w.rotate()
	}
}

func (w *Writer) ticker() {
	defer w.tick.Done()
	t := time.NewTicker(w.cfg.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			w.mu.Lock()
			w.flush()
			if w.f != nil && w.size > 0 && w.now().Sub(w.opened) >= w.cfg.MaxAge {
				w.rotate()
			}
			w.mu.Unlock()
		}
	}
}

func (w *Writer) flush() {
	if w.buf == nil || w.pending == 0 {
		return
	}
	if err := w.buf.Flush(); err != nil {
		w.fail("flush", err)
	}
	w.pending = 0
}

// rotate finishes the active file and opens the next one. Caller holds mu.
func (w *Writer) rotate() {
	if err := w.finishFile(); err != nil {
		w.fail("rotate", err)
	}
	if err := w.openFile(); err != nil {
		w.fail("open", err)
	}
}

// finishFile flushes and closes the active file, then compresses it and
// applies retention in the background. Caller holds mu.
func (w *Writer) finishFile() error {
	if w.f == nil {
		return nil
	}
	w.pending = 1 // force the flush
	w.flush()
	name, empty, err := w.f.Name(), w.size == 0, w.f.Close()
	w.f, w.buf, w.enc = nil, nil, nil
	if empty { // nothing was written: leave no empty files behind
		return errors.Join(err, os.Remove(name))
	}
	w.bgWG.Add(1)
	go func() {
		defer w.bgWG.Done()
		w.bg.Lock()
		defer w.bg.Unlock()
		if w.cfg.Gzip {
			if err := compress(name); err != nil {
				w.fail("compress", err)
			}
		}
		w.applyRetention()
	}()
	return err
}

// openFile creates a new active file named after the current UTC time.
// Caller holds mu.
func (w *Writer) openFile() error {
	base := filePrefix + w.now().UTC().Format("20060102T150405Z")
	for i := 0; ; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(filepath.Join(w.dir, name+fileSuffix), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		w.f, w.size, w.opened, w.pending = f, 0, w.now(), 0
		w.buf = bufio.NewWriterSize(&countingWriter{w: f, n: &w.size}, 256<<10)
		w.enc = json.NewEncoder(w.buf)
		w.enc.SetEscapeHTML(false)
		return nil
	}
}

// applyRetention deletes finished files older than the retention period, then
// the oldest ones while the directory exceeds max_total_bytes.
func (w *Writer) applyRetention() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		w.fail("retention", err)
		return
	}
	w.mu.Lock()
	active := ""
	if w.f != nil {
		active = filepath.Base(w.f.Name())
	}
	activeSize := w.size
	w.mu.Unlock()

	type file struct {
		name string
		size int64
		mod  time.Time
	}
	var files []file
	total := activeSize
	for _, e := range entries {
		n := e.Name()
		if n == active || !strings.HasPrefix(n, filePrefix) || !(strings.HasSuffix(n, fileSuffix) || strings.HasSuffix(n, fileSuffix+".gz")) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{n, info.Size(), info.ModTime()})
		total += info.Size()
	}
	slices.SortFunc(files, func(a, b file) int { return strings.Compare(a.name, b.name) }) // names sort by time
	cutoff := w.now().Add(-w.cfg.Retention)
	for _, f := range files {
		if f.mod.Before(cutoff) || total > int64(w.cfg.MaxTotalBytes) {
			if err := os.Remove(filepath.Join(w.dir, f.name)); err == nil || errors.Is(err, fs.ErrNotExist) {
				total -= f.size
			}
		}
	}
}

func (w *Writer) fail(op string, err error) {
	n := w.writeErrors.Add(1)
	if n == 1 || n%1000 == 0 { // rate-limited: a broken disk would flood the log
		w.log.Error("jsonl sink", "op", op, "err", err, "errors_total", n)
	}
}

// compress writes name.gz (mode 0600) and removes name.
func compress(name string) error {
	src, err := os.Open(name)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(name+".gz", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(dst)
	if _, err := io.Copy(zw, src); err != nil {
		dst.Close()
		os.Remove(name + ".gz")
		return err
	}
	if err := errors.Join(zw.Close(), dst.Close()); err != nil {
		os.Remove(name + ".gz")
		return err
	}
	return os.Remove(name)
}

type countingWriter struct {
	w io.Writer
	n *int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	*c.n += int64(n)
	return n, err
}
