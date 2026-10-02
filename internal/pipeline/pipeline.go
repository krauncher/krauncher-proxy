// SPDX-License-Identifier: Apache-2.0

// Package pipeline provides the bounded, drop-on-full work queue between the
// data plane and the shape pipeline (stage 2).
package pipeline

import (
	"sync"
	"sync/atomic"
)

// Queue runs handle on items submitted from any goroutine, using a fixed
// number of workers. Submit never blocks: when the queue is full the item is
// dropped and counted. The type parameter lets the same queue carry request
// and response events, or records towards a sink.
type Queue[T any] struct {
	ch      chan T
	handle  func(T)
	wg      sync.WaitGroup
	mu      sync.RWMutex // guards closed against Submit racing Close
	closed  bool
	dropped atomic.Uint64
	panics  atomic.Uint64
}

// New starts workers goroutines (at least one) reading a queue of size items.
func New[T any](size, workers int, handle func(T)) *Queue[T] {
	q := &Queue[T]{ch: make(chan T, size), handle: handle}
	workers = max(workers, 1)
	q.wg.Add(workers)
	for range workers {
		go func() {
			defer q.wg.Done()
			for item := range q.ch {
				q.run(item)
			}
		}()
	}
	return q
}

// run handles one item; a panic is counted and the worker goes on. Handlers
// should recover themselves to clean up; this is the last line of defence.
func (q *Queue[T]) run(item T) {
	defer func() {
		if recover() != nil {
			q.panics.Add(1)
		}
	}()
	q.handle(item)
}

// Panics returns the number of items whose handler panicked.
func (q *Queue[T]) Panics() uint64 { return q.panics.Load() }

// Submit enqueues item without blocking. It reports false if the item was
// dropped because the queue is full or closed.
func (q *Queue[T]) Submit(item T) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		q.dropped.Add(1)
		return false
	}
	select {
	case q.ch <- item:
		return true
	default:
		q.dropped.Add(1)
		return false
	}
}

// Close stops accepting items, lets the workers drain what is queued, and
// waits for them.
func (q *Queue[T]) Close() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.ch)
	}
	q.mu.Unlock()
	q.wg.Wait()
}

// Dropped returns the number of items dropped so far.
func (q *Queue[T]) Dropped() uint64 { return q.dropped.Load() }

// Len returns the number of queued items.
func (q *Queue[T]) Len() int { return len(q.ch) }
