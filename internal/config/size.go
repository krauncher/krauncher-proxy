// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Size is a byte count written in configuration as an integer or with a
// B, KiB, MiB or GiB suffix.
type Size int64

const (
	KiB Size = 1 << 10
	MiB Size = 1 << 20
	GiB Size = 1 << 30
)

// Longest suffixes first, so "KiB" is not matched as "B".
var sizeUnits = []struct {
	suffix string
	mult   Size
}{{"GiB", GiB}, {"MiB", MiB}, {"KiB", KiB}, {"B", 1}}

// ParseSize parses "64KiB", "1MiB", "512B" or a plain integer.
func ParseSize(in string) (Size, error) {
	s := strings.TrimSpace(in)
	mult := Size(1)
	for _, u := range sizeUnits {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q: want a non-negative integer with optional B, KiB, MiB or GiB", in)
	}
	return Size(n) * mult, nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (z *Size) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseSize(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*z = v
	return nil
}
