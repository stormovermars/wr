//go:build !windows

package osutil

import "os"

// ReplaceFile renames tmp over path; rename(2) already has POSIX replace semantics.
func ReplaceFile(tmp, path string) error { return os.Rename(tmp, path) }
