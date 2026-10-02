// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestQueueProcessesAllAndDrainsOnClose(t *testing.T) {
	var n atomic.Int64
	q := New(1000, 4, func(int) { n.Add(1) })
	for i := range 500 {
		if !q.Submit(i) {
			t.Fatalf("item %d dropped with free capacity", i)
		}
	}
	q.Close()
	if n.Load() != 500 {
		t.Fatalf("processed %d, want 500", n.Load())
	}
}

func TestPanicDoesNotStopWorkers(t *testing.T) {
	var n atomic.Int64
	q := New(10, 1, func(i int) {
		if i%2 == 0 {
			panic("boom")
		}
		n.Add(1)
	})
	for i := range 6 {
		q.Submit(i)
	}
	q.Close()
	if n.Load() != 3 || q.Panics() != 3 {
		t.Fatalf("processed %d, panics %d", n.Load(), q.Panics())
	}
}

func TestQueueDropsWhenFullAndAfterClose(t *testing.T) {
	block, started := make(chan struct{}), make(chan struct{}, 3)
	q := New(2, 1, func(int) { started <- struct{}{}; <-block })
	q.Submit(0)
	<-started // the worker holds item 0 and blocks
	q.Submit(1)
	q.Submit(2)
	if q.Submit(3) {
		t.Fatal("submit to a full queue succeeded")
	}
	close(block)
	q.Close()
	if q.Submit(4) {
		t.Fatal("submit after close succeeded")
	}
	if q.Dropped() != 2 {
		t.Fatalf("dropped %d, want 2", q.Dropped())
	}
}

// Every submitted item is either processed or counted as dropped, also when
// Close races with Submit.
func TestSubmitRacingCloseLosesNothingSilently(t *testing.T) {
	var processed, accepted atomic.Int64
	q := New(16, 2, func(int) { processed.Add(1) })
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 1000 {
				if q.Submit(i) {
					accepted.Add(1)
				}
			}
		}()
	}
	q.Close()
	wg.Wait()
	if processed.Load() != accepted.Load() {
		t.Fatalf("processed %d, accepted %d", processed.Load(), accepted.Load())
	}
	if accepted.Load()+int64(q.Dropped()) != 8000 {
		t.Fatalf("accepted %d + dropped %d != 8000", accepted.Load(), q.Dropped())
	}
}
