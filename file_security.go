package main

import (
	"fmt"
	"os"
	"runtime"
)

const (
	// groupOtherPermMask selects all group and other permission bits.
	groupOtherPermMask os.FileMode = 0o077
	// groupOtherWriteMask selects the group and other write bits.
	groupOtherWriteMask os.FileMode = 0o022
	// ownerOnlyFileMode is owner read/write only.
	ownerOnlyFileMode os.FileMode = 0o600
)

// requirePrivateFile fails unless path is a regular file without group or
// other permissions. Empty paths, ":memory:", and Windows are not checked.
func requirePrivateFile(path, label string) error {
	if path == "" || path == ":memory:" || runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return requirePrivateFileInfo(info, path, label)
}

// requirePrivateFileInfo applies the requirePrivateFile checks to info.
func requirePrivateFileInfo(info os.FileInfo, path, label string) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file: %s", label, path)
	}
	if info.Mode().Perm()&groupOtherPermMask != 0 {
		return fmt.Errorf("%s permissions must not allow group or other access: %s has %04o", label, path, info.Mode().Perm())
	}
	return nil
}

// requireReadOnlyConfigFile fails unless path is a regular file that group and
// others cannot write. Empty paths and Windows are not checked.
func requireReadOnlyConfigFile(path string) error {
	if path == "" || runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("configuration file must be a regular file: %s", path)
	}
	if info.Mode().Perm()&groupOtherWriteMask != 0 {
		return fmt.Errorf("configuration file must not be writable by group or others: %s has %04o", path, info.Mode().Perm())
	}
	return nil
}

// secureFileMode chmods path to ownerOnlyFileMode, ignoring a missing file.
func secureFileMode(path string) error {
	if path == "" || path == ":memory:" || runtime.GOOS == "windows" {
		return nil
	}
	if err := os.Chmod(path, ownerOnlyFileMode); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
