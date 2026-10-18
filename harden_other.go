//go:build unix && !linux

package main

// platformHarden has nothing to add: PR_SET_DUMPABLE exists only on Linux,
// the production platform.
func platformHarden() error { return nil }
