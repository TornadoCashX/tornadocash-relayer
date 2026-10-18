//go:build !unix

package main

// hardenProcess is a no-op on platforms without POSIX resource limits.
func hardenProcess() error { return nil }
