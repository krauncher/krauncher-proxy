// SPDX-License-Identifier: Apache-2.0

// Package capture holds what the data plane copies for stage 2: bounded
// buffers charged against a process-wide memory budget, the chunk timeline,
// and the per-request Pending object shared by the request and response sides.
package capture

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/krauncher/krauncher-proxy/internal/sse"
)

// Budget caps the memory held by all capture buffers. Acquisition never
// blocks: when the budget is exhausted, capture stops instead.
type Budget struct {
	max  int64
	used atomic.Int64
}

func NewBudget(max int64) *Budget { return &Budget{max: max} }

func (b *Budget) TryAcquire(n int64) bool {
	for {
		u := b.used.Load()
		if u+n > b.max {
			return false
		}
		if b.used.CompareAndSwap(u, u+n) {
			return true
		}
	}
}

func (b *Budget) Release(n int64) { b.used.Add(-n) }

// Used returns the bytes currently reserved.
func (b *Budget) Used() int64 { return b.used.Load() }

// Buffer keeps the first limit bytes written to it, reserving budget in steps
// as bytes arrive, so memory follows the actual size, not the cap. The budget
// is reserved in steps, but the slice itself grows only as bytes arrive (a
// 2 KiB body allocates about 2 KiB, not a whole step): allocation volume, not
// the reservation, is what drives GC cost under load (doc/benchmarks.md, M5).
type Buffer struct {
	buf       []byte
	limit     int
	step      int
	reserved  int
	budget    *Budget
	truncated bool // bytes were dropped: cap reached or budget exhausted
}

func NewBuffer(limit, step int, budget *Budget) *Buffer {
	return &Buffer{limit: max(limit, 0), step: max(step, 1), budget: budget}
}

// Write keeps what fits and returns how many bytes it kept. Once a byte has
// been dropped the buffer takes nothing more, so what it holds is always one
// contiguous prefix of the input.
func (b *Buffer) Write(p []byte) int {
	if b.truncated {
		return 0
	}
	need := min(len(p), b.limit-len(b.buf))
	for len(b.buf)+need > b.reserved {
		grow := min(b.step, b.limit-b.reserved)
		if !b.budget.TryAcquire(int64(grow)) {
			need = b.reserved - len(b.buf)
			break
		}
		b.reserved += grow
	}
	b.buf = append(b.buf, p[:need]...)
	if need < len(p) {
		b.truncated = true
	}
	return need
}

func (b *Buffer) Bytes() []byte { return b.buf }
func (b *Buffer) Len() int      { return len(b.buf) }

// Truncated reports whether any written byte was not kept.
func (b *Buffer) Truncated() bool { return b.truncated }

// Release zeroes the bytes and returns the reservation.
func (b *Buffer) Release() {
	clear(b.buf[:cap(b.buf)])
	b.budget.Release(int64(b.reserved))
	b.buf, b.reserved = nil, 0
}

// Ring keeps the last size bytes written to it.
type Ring struct {
	buf    []byte
	n      int64 // total bytes written
	budget *Budget
}

// NewRing reserves size bytes, or returns nil if size is not positive or the
// budget is exhausted.
func NewRing(size int, budget *Budget) *Ring {
	if size <= 0 || !budget.TryAcquire(int64(size)) {
		return nil
	}
	return &Ring{buf: make([]byte, size), budget: budget}
}

func (r *Ring) Write(p []byte) {
	size := len(r.buf)
	if len(p) >= size {
		r.n += int64(len(p))
		// Keep the last size bytes with the oldest at the next write position.
		pos := int(r.n % int64(size))
		k := copy(r.buf[pos:], p[len(p)-size:])
		copy(r.buf, p[len(p)-size+k:])
		return
	}
	pos := int(r.n % int64(size))
	k := copy(r.buf[pos:], p)
	copy(r.buf, p[k:])
	r.n += int64(len(p))
}

// Bytes returns the kept bytes in order (allocates) and how many bytes of the
// ring's input precede them.
func (r *Ring) Bytes() (b []byte, skipped int64) {
	size := int64(len(r.buf))
	if r.n <= size {
		return slices.Clone(r.buf[:r.n]), 0
	}
	pos := r.n % size
	return append(slices.Clone(r.buf[pos:]), r.buf[:pos]...), r.n - size
}

func (r *Ring) Release() {
	clear(r.buf)
	r.budget.Release(int64(len(r.buf)))
	r.buf = nil
}

// Outcome values (doc/data-model.md).
const (
	OutcomeOK              = "ok"
	OutcomeClientCancelled = "client_cancelled"
	OutcomeUpstreamError   = "upstream_error"
	OutcomeProxyError      = "proxy_error"
	OutcomeUnauthorized    = "unauthorized"
)

// Point is one write to the client: cumulative bytes after it and when.
type Point struct {
	Off int64
	T   time.Time
}

// Pending is one request in flight between the data plane and stage 2.
// The data plane fills it; once it submits the response side it no longer
// touches it.
type Pending struct {
	ID, Route, Dialect, Precision, Engine, Client string
	Path                                          string // without query, for the endpoint
	StreamUsage                                   string // route option

	T0, ReqEnd, Headers, End time.Time // zero = did not happen

	InflightRoute, InflightGlobal int64
	ReqBytes, RespBytes           int64
	Status                        int
	Outcome                       string

	Captured bool // body capture was attempted for this request

	// Request side. Req is nil when nothing was captured.
	Req           *Buffer
	ReqIncomplete bool // the body was not read to the end
	Injected      bool // the proxy added stream_options.include_usage

	// Response side.
	ContentType, ContentEncoding string
	Stream                       bool    // text/event-stream
	Head                         *Buffer // first bytes (SSE: head; otherwise the whole body up to its cap)
	Tail                         *Ring   // bytes after Head, last ones kept
	TailFailed                   bool    // a tail was needed but the budget was exhausted
	Timeline                     []Point
	TimelineTotal                int
	Last                         Point
	SSE                          sse.Counter

	reqOnce   sync.Once
	reqDone   chan struct{}
	reqResult any
}

// NewPending prepares the request-side handoff.
func NewPending() *Pending { return &Pending{reqDone: make(chan struct{})} }

// ResolveRequest publishes the parsed request side (or nil) exactly once.
func (p *Pending) ResolveRequest(result any) {
	p.reqOnce.Do(func() {
		p.reqResult = result
		close(p.reqDone)
	})
}

// WaitRequest waits up to d for the request side. ok is false on timeout.
func (p *Pending) WaitRequest(d time.Duration) (result any, ok bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-p.reqDone:
		return p.reqResult, true
	case <-t.C:
		return nil, false
	}
}

// AddPoint records a write to the client, keeping at most limit points.
func (p *Pending) AddPoint(off int64, t time.Time, limit int) {
	p.TimelineTotal++
	p.Last = Point{off, t}
	if len(p.Timeline) < limit {
		p.Timeline = append(p.Timeline, p.Last)
	}
}

// TimeAt returns when the byte at offset off (exclusive end) reached the
// client: the first point whose cumulative offset covers it.
func (p *Pending) TimeAt(off int64) (time.Time, bool) {
	for _, pt := range p.Timeline {
		if pt.Off >= off {
			return pt.T, true
		}
	}
	return time.Time{}, false // beyond the kept points
}

// ReleaseResponse frees the response buffers.
func (p *Pending) ReleaseResponse() {
	if p.Head != nil {
		p.Head.Release()
	}
	if p.Tail != nil {
		p.Tail.Release()
	}
}
