//go:build !windows

package fileutil

import "os"

// RenameReplace renames src onto dst, replacing an existing destination.
// On Unix-like systems os.Rename already replaces the target atomically.
func RenameReplace(src, dst string) error {
	return os.Rename(src, dst)
}
