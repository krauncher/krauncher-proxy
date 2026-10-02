// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package harden

import "errors"

// Apply is only implemented on Linux.
func Apply() error { return errors.New("process hardening is only implemented on Linux") }
