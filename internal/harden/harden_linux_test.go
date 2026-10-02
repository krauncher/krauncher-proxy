// SPDX-License-Identifier: Apache-2.0

//go:build linux

package harden

import (
	"syscall"
	"testing"
)

func TestApply(t *testing.T) {
	if err := Apply(); err != nil {
		t.Fatal(err)
	}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &lim); err != nil || lim.Cur != 0 || lim.Max != 0 {
		t.Fatalf("RLIMIT_CORE %+v, %v", lim, err)
	}
	r1, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, 3 /* PR_GET_DUMPABLE */, 0, 0)
	if errno != 0 || r1 != 0 {
		t.Fatalf("dumpable = %d, %v", r1, errno)
	}
}
