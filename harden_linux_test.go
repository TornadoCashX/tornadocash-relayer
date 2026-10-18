//go:build linux

package main

import "testing"

func TestHardenProcessClearsDumpable(t *testing.T) {
	if err := hardenProcess(); err != nil {
		t.Fatalf("hardenProcess: %v", err)
	}
	dumpable, err := processDumpable()
	if err != nil {
		t.Fatal(err)
	}
	if dumpable != 0 {
		t.Fatalf("PR_GET_DUMPABLE = %d, want 0", dumpable)
	}
}
