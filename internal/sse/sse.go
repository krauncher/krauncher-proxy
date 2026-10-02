// SPDX-License-Identifier: Apache-2.0

// Package sse scans Server-Sent Events in captured buffers and counts event
// separators on the data plane.
package sse

import "bytes"

// Event is one complete event. Data is the joined data field; it aliases the
// buffer when the event has a single data line.
type Event struct {
	Name       []byte
	Data       []byte
	Start, End int // byte span in the scanned buffer, End after the blank line
}

// Each calls fn for every complete event in buf, in order, until fn returns
// false. If partialStart is true, buf may begin mid-event (a tail buffer) and
// everything up to the first event boundary is skipped. An event without its
// terminating blank line is not reported. Accepts \n and \r\n line endings.
func Each(buf []byte, partialStart bool, fn func(Event) bool) {
	pos := 0
	if partialStart {
		pos = firstBoundary(buf)
		if pos < 0 {
			return
		}
	}
	var (
		ev       = Event{Start: pos}
		dataN    int
		joined   []byte
		hasField bool
	)
	for pos < len(buf) {
		nl := bytes.IndexByte(buf[pos:], '\n')
		if nl < 0 {
			return // incomplete line
		}
		line := buf[pos : pos+nl]
		pos += nl + 1
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			if hasField {
				ev.End = pos
				if dataN > 1 {
					ev.Data = joined
				}
				if !fn(ev) {
					return
				}
			}
			ev, dataN, joined, hasField = Event{Start: pos}, 0, nil, false
			continue
		}
		hasField = true
		name, value, _ := bytes.Cut(line, []byte{':'})
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(name) {
		case "data":
			dataN++
			switch dataN {
			case 1:
				ev.Data = value
			case 2:
				joined = append(append(append([]byte(nil), ev.Data...), '\n'), value...)
			default:
				joined = append(append(joined, '\n'), value...)
			}
		case "event":
			ev.Name = value
		}
	}
}

// firstBoundary returns the offset just after the first blank line.
func firstBoundary(buf []byte) int {
	for i := 0; i < len(buf); i++ {
		if buf[i] != '\n' {
			continue
		}
		j := i + 1
		if j < len(buf) && buf[j] == '\r' {
			j++
		}
		if j < len(buf) && buf[j] == '\n' {
			return j + 1
		}
	}
	return -1
}

// Counter counts event boundaries in a byte stream fed in arbitrary pieces.
// It is the only byte inspection on the data plane: one pass, no allocation.
type Counter struct {
	n       int
	lastNL  bool // last byte other than \r was \n
	inEvent bool // bytes seen since the last boundary
}

// Write feeds the next piece of the stream.
func (c *Counter) Write(p []byte) {
	for _, b := range p {
		switch b {
		case '\r':
		case '\n':
			if c.lastNL && c.inEvent {
				c.n++
				c.inEvent = false
			}
			c.lastNL = true
		default:
			c.lastNL = false
			c.inEvent = true
		}
	}
}

// Events returns the number of complete events seen.
func (c *Counter) Events() int { return c.n }
