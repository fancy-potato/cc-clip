//go:build windows

package sshconfig

import "os"

// Windows does not use POSIX uid/gid ownership, so captureOwnership always
// returns ok=false and applyOwnership is never called.
func captureOwnership(_ os.FileInfo) (uid, gid int, ok bool) {
	return 0, 0, false
}

func applyOwnership(_ string, _, _ int) error {
	return nil
}

// applyOwnershipFd is the fd-based variant. No-op on Windows for the same
// reason as applyOwnership: Windows uses SIDs, not POSIX uid/gid. Signature
// parity keeps writeAtomic's call site cross-platform.
func applyOwnershipFd(_ uintptr, _, _ int) error {
	return nil
}
