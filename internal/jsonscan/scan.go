// SPDX-License-Identifier: Apache-2.0

// Package jsonscan walks a JSON document, possibly truncated, in one pass and
// reports values by path without allocating for the values it skips. It is
// the parser for captured request bodies, which may be cut at any byte.
//
// It is lenient where leniency is harmless: number grammar is not validated
// and bytes after the first top-level value are ignored.
//
// Guarantees: a scalar is reported only after it was read completely; a
// container's element count is reported only when the container was closed
// within the buffer. A truncated document is not an error: Scan reports how far
// it got.
package jsonscan

import (
	"encoding/json"
	"errors"
	"strconv"
	"unicode/utf8"
)

// Kind of a JSON value.
type Kind uint8

const (
	Object Kind = iota + 1
	Array
	String
	Number
	Bool
	Null
)

// Seg is one path step: an object key (Key != nil) or an array index.
type Seg struct {
	Key   []byte // raw key bytes without quotes, escapes not decoded
	Index int
}

// Path is the location of the current value. It is only valid during the
// callback.
type Path []Seg

// Match reports whether the path equals pattern, where each element is an
// object key, or "*" for any array index.
func (p Path) Match(pattern ...string) bool {
	if len(p) != len(pattern) {
		return false
	}
	for i, s := range p {
		if pattern[i] == "*" {
			if s.Key != nil {
				return false
			}
			continue
		}
		if s.Key == nil || string(s.Key) != pattern[i] {
			return false
		}
	}
	return true
}

// Visitor receives the walk. Value is called for complete scalars with their
// raw bytes (strings include the quotes). End is called when an object or
// array closes, with the number of members or elements and the byte span of
// the container. Start is called when a container opens, at offset off.
type Visitor interface {
	Start(p Path, k Kind, off int)
	Value(p Path, k Kind, raw []byte, off int)
	End(p Path, k Kind, n int, start, end int)
}

// ErrSyntax reports malformed JSON (as opposed to truncated JSON).
var ErrSyntax = errors.New("jsonscan: syntax error")

// Scan walks buf. complete is true when one whole top-level value was read.
// A truncated buffer returns complete=false and a nil error.
func Scan(buf []byte, v Visitor) (complete bool, err error) {
	s := scanner{buf: buf, v: v, path: make(Path, 0, 8)}
	return s.run()
}

type scanner struct {
	buf  []byte
	i    int
	v    Visitor
	path Path
}

var errEOF = errors.New("eof") // internal: ran out of input

func (s *scanner) run() (bool, error) {
	err := s.value()
	switch {
	case err == errEOF:
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

func (s *scanner) ws() {
	for s.i < len(s.buf) {
		switch s.buf[s.i] {
		case ' ', '\t', '\n', '\r':
			s.i++
		default:
			return
		}
	}
}

// value parses one value at s.i; the path for it is already pushed.
func (s *scanner) value() error {
	s.ws()
	if s.i >= len(s.buf) {
		return errEOF
	}
	start := s.i
	switch c := s.buf[s.i]; {
	case c == '{':
		return s.object(start)
	case c == '[':
		return s.array(start)
	case c == '"':
		end, err := stringEnd(s.buf, s.i)
		if err != nil {
			return err
		}
		s.i = end
		s.v.Value(s.path, String, s.buf[start:end], start)
	case c == 't':
		return s.literal("true", Bool)
	case c == 'f':
		return s.literal("false", Bool)
	case c == 'n':
		return s.literal("null", Null)
	case c == '-' || (c >= '0' && c <= '9'):
		s.i++
		for s.i < len(s.buf) && isNumByte(s.buf[s.i]) {
			s.i++
		}
		if s.i >= len(s.buf) && len(s.path) > 0 {
			return errEOF // inside a container the number may continue past the cut
		}
		s.v.Value(s.path, Number, s.buf[start:s.i], start)
	default:
		return ErrSyntax
	}
	return nil
}

func isNumByte(c byte) bool {
	return (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-'
}

func (s *scanner) literal(word string, k Kind) error {
	start := s.i
	for j := 0; j < len(word); j++ {
		if s.i >= len(s.buf) {
			return errEOF
		}
		if s.buf[s.i] != word[j] {
			return ErrSyntax
		}
		s.i++
	}
	s.v.Value(s.path, k, s.buf[start:s.i], start)
	return nil
}

func (s *scanner) object(start int) error {
	s.i++ // {
	s.v.Start(s.path, Object, start)
	n := 0
	for {
		s.ws()
		if s.i >= len(s.buf) {
			return errEOF
		}
		if s.buf[s.i] == '}' {
			s.i++
			s.v.End(s.path, Object, n, start, s.i)
			return nil
		}
		if n > 0 {
			if s.buf[s.i] != ',' {
				return ErrSyntax
			}
			s.i++
			s.ws()
			if s.i >= len(s.buf) {
				return errEOF
			}
		}
		if s.buf[s.i] != '"' {
			return ErrSyntax
		}
		kEnd, err := stringEnd(s.buf, s.i)
		if err != nil {
			return err
		}
		key := s.buf[s.i+1 : kEnd-1]
		s.i = kEnd
		s.ws()
		if s.i >= len(s.buf) {
			return errEOF
		}
		if s.buf[s.i] != ':' {
			return ErrSyntax
		}
		s.i++
		s.path = append(s.path, Seg{Key: key})
		if err := s.value(); err != nil {
			return err
		}
		s.path = s.path[:len(s.path)-1]
		n++
	}
}

func (s *scanner) array(start int) error {
	s.i++ // [
	s.v.Start(s.path, Array, start)
	n := 0
	for {
		s.ws()
		if s.i >= len(s.buf) {
			return errEOF
		}
		if s.buf[s.i] == ']' {
			s.i++
			s.v.End(s.path, Array, n, start, s.i)
			return nil
		}
		if n > 0 {
			if s.buf[s.i] != ',' {
				return ErrSyntax
			}
			s.i++
		}
		s.path = append(s.path, Seg{Index: n})
		if err := s.value(); err != nil {
			return err
		}
		s.path = s.path[:len(s.path)-1]
		n++
	}
}

// stringEnd returns the index after the closing quote of the string starting
// at buf[i] == '"'.
func stringEnd(buf []byte, i int) (int, error) {
	for j := i + 1; j < len(buf); j++ {
		switch buf[j] {
		case '"':
			return j + 1, nil
		case '\\':
			j++ // skip the escaped byte; \uXXXX digits are plain bytes
		default:
			if buf[j] < 0x20 { // raw control characters are not allowed in JSON strings
				return 0, ErrSyntax
			}
		}
	}
	return 0, errEOF
}

// DecodedLen returns the UTF-8 length of a JSON string literal (with quotes)
// after unescaping, without allocating. Invalid escapes count as their raw
// bytes; a lone surrogate counts as U+FFFD, as encoding/json decodes it.
func DecodedLen(raw []byte) int {
	if len(raw) < 2 {
		return 0
	}
	b := raw[1 : len(raw)-1]
	n := 0
	for i := 0; i < len(b); i++ {
		if b[i] >= utf8.RuneSelf {
			// Invalid UTF-8 decodes to U+FFFD (3 bytes), as in encoding/json.
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size == 1 {
				n += 3
			} else {
				n += size
			}
			i += size - 1
			continue
		}
		if b[i] != '\\' || i+1 >= len(b) {
			n++
			continue
		}
		i++
		if b[i] != 'u' {
			n++ // \n, \", \\ ... decode to one byte
			continue
		}
		r, ok := hex4(b[i+1:])
		if !ok {
			n++
			continue
		}
		i += 4 // i is on the last hex digit
		if r >= 0xD800 && r < 0xDC00 && i+6 < len(b) && b[i+1] == '\\' && b[i+2] == 'u' {
			if lo, ok := hex4(b[i+3:]); ok && lo >= 0xDC00 && lo < 0xE000 {
				i += 6
				n += 4 // a surrogate pair is one 4-byte code point
				continue
			}
		}
		if r >= 0xD800 && r < 0xE000 {
			n += 3 // lone surrogate → U+FFFD
			continue
		}
		n += utf8.RuneLen(rune(r))
	}
	return n
}

func hex4(b []byte) (int, bool) {
	if len(b) < 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(string(b[:4]), 16, 32)
	return int(v), err == nil
}

// Unquote decodes a JSON string literal. Use only for short whitelisted
// values (model names, roles); it allocates.
func Unquote(raw []byte) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
