//go:build unix

package main

import (
	"fmt"
	"syscall"
)

// hardenProcess keeps request data (proofs, recipients, client IPs) and the
// OZ API key out of crash dumps and other processes:
//   - RLIMIT_CORE 0 (soft and hard): the kernel writes no core file, even
//     with GOTRACEBACK=crash.
//   - platformHarden: PR_SET_DUMPABLE 0 on Linux (see harden_linux.go).
//
// Swap is not controlled here; the systemd unit sets MemorySwapMax=0.
func hardenProcess() error {
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		return fmt.Errorf("disable core dumps: %w", err)
	}
	return platformHarden()
}
