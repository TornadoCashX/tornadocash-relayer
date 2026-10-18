//go:build linux

package main

import (
	"fmt"
	"syscall"
)

// prSetDumpable / prGetDumpable are the prctl options PR_SET_DUMPABLE and
// PR_GET_DUMPABLE (linux/prctl.h).
const (
	prSetDumpable = 4
	prGetDumpable = 3
)

// platformHarden sets PR_SET_DUMPABLE 0: no core dump, and /proc/<pid>/mem,
// environ and ptrace are refused to other processes of the same user.
func platformHarden() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_DUMPABLE, 0): %w", errno)
	}
	return nil
}

// processDumpable reports the PR_GET_DUMPABLE value (tests).
func processDumpable() (int, error) {
	value, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prGetDumpable, 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(value), nil
}
