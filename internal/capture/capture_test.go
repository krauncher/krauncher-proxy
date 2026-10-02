// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"bytes"
	"math/rand/v2"
	"testing"
	"time"
)

func TestBufferStepsCapAndBudget(t *testing.T) {
	b := NewBudget(1000)
	buf := NewBuffer(300, 64, b)
	buf.Write(bytes.Repeat([]byte("a"), 100))
	if b.Used() != 128 || buf.Truncated() {
		t.Fatalf("used %d truncated %v after 100 bytes", b.Used(), buf.Truncated())
	}
	buf.Write(bytes.Repeat([]byte("b"), 500))
	if buf.Len() != 300 || !buf.Truncated() || b.Used() != 300 {
		t.Fatalf("len %d truncated %v used %d", buf.Len(), buf.Truncated(), b.Used())
	}
	raw := buf.Bytes()[:cap(buf.Bytes())]
	buf.Release()
	if b.Used() != 0 {
		t.Fatalf("budget not returned: %d", b.Used())
	}
	for _, c := range raw {
		if c != 0 {
			t.Fatal("released buffer not zeroed")
		}
	}
}

func TestBufferStopsWhenBudgetExhausted(t *testing.T) {
	b := NewBudget(100)
	buf := NewBuffer(1000, 64, b)
	buf.Write(make([]byte, 500))
	if buf.Len() != 64 || !buf.Truncated() || b.Used() != 64 {
		t.Fatalf("len %d truncated %v used %d", buf.Len(), buf.Truncated(), b.Used())
	}
	if NewRing(64, b) != nil {
		t.Fatal("ring allocated beyond the budget")
	}
}

// The ring holds exactly the last size bytes, whatever the write sizes.
func TestRingKeepsLastBytes(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for trial := 0; trial < 200; trial++ {
		size := 1 + r.IntN(50)
		ring := NewRing(size, NewBudget(1<<20))
		var all []byte
		for w := r.IntN(20); w >= 0; w-- {
			p := make([]byte, r.IntN(3*size))
			for i := range p {
				p[i] = byte(r.Uint32())
			}
			ring.Write(p)
			all = append(all, p...)
		}
		got, skipped := ring.Bytes()
		want := all[max(0, len(all)-size):]
		if !bytes.Equal(got, want) || skipped != int64(len(all)-len(want)) {
			t.Fatalf("size %d total %d: got %d bytes skipped %d", size, len(all), len(got), skipped)
		}
	}
}

func TestPendingHandoffAndTimeline(t *testing.T) {
	p := NewPending()
	if _, ok := p.WaitRequest(10 * time.Millisecond); ok {
		t.Fatal("resolved before ResolveRequest")
	}
	p.ResolveRequest("x")
	p.ResolveRequest("y") // second call ignored
	if v, ok := p.WaitRequest(time.Second); !ok || v != "x" {
		t.Fatalf("got %v %v", v, ok)
	}
	t0 := time.Now()
	for i := 1; i <= 5; i++ {
		p.AddPoint(int64(i*10), t0.Add(time.Duration(i)), 3)
	}
	if len(p.Timeline) != 3 || p.TimelineTotal != 5 || p.Last.Off != 50 {
		t.Fatalf("timeline %v total %d last %v", p.Timeline, p.TimelineTotal, p.Last)
	}
	if ts, ok := p.TimeAt(15); !ok || !ts.Equal(t0.Add(2)) {
		t.Fatalf("TimeAt(15) = %v %v", ts, ok)
	}
	if _, ok := p.TimeAt(45); ok {
		t.Fatal("TimeAt beyond kept points")
	}
}
