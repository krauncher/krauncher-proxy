// SPDX-License-Identifier: Apache-2.0

package jsonl

import (
	"encoding/json"
	"io"
	"testing"
)

// BenchmarkEncode measures encoding one full record.
func BenchmarkEncode(b *testing.B) {
	r := record("bench")
	enc := json.NewEncoder(io.Discard)
	b.ReportAllocs()
	for range b.N {
		enc.Encode(r)
	}
}
