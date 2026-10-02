// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Package harden applies process-level protections from doc/privacy.md.
package harden

import "syscall"

const prSetDumpable = 4 // PR_SET_DUMPABLE, linux/prctl.h

// Apply disables core dumps and makes the process non-dumpable, which also
// stops other processes of the same user from attaching with ptrace. Captured
// request and response bytes live in memory; neither must reach a core file.
func Apply() error {
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{}); err != nil {
		return err
	}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
