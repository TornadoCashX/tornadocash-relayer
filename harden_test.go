//go:build unix

package main

import (
	"syscall"
	"testing"
)

func TestHardenProcessDisablesCoreDumps(t *testing.T) {
	if err := hardenProcess(); err != nil {
		t.Fatalf("hardenProcess: %v", err)
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &limit); err != nil {
		t.Fatal(err)
	}
	if limit.Cur != 0 || limit.Max != 0 {
		t.Fatalf("RLIMIT_CORE = %d/%d, want 0/0", limit.Cur, limit.Max)
	}
	// Idempotent: lowering an already zero hard limit must still succeed.
	if err := hardenProcess(); err != nil {
		t.Fatalf("second hardenProcess: %v", err)
	}
}
