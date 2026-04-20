//go:build linux

package sshconfig

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames src → dst while refusing to clobber dst if it
// already exists. On Linux this is implemented via renameat2(2) with
// RENAME_NOREPLACE, which closes the residual TOCTOU window between the
// pre-rename Lstat check in writeAtomic and os.Rename: even if an
// attacker pre-plants a symlink at dst between the Lstat and this call,
// the kernel's atomic check fails the rename with EEXIST instead of
// silently following the symlink and writing to an unexpected target.
//
// Returns (true, nil) when the rename took place, (false, os.ErrExist)
// when dst already existed (caller should treat as "swap race" and
// bail), and (false, err) for any other filesystem error. If the running
// kernel does not support renameat2 (pre-3.15, or a filesystem that
// doesn't implement it — notably older NFS), we fall back to the legacy
// Lstat-then-Rename path and signal to the caller with ok=false and a
// nil error meaning "use fallback".
func renameNoReplace(src, dst string) (ok bool, err error) {
	err = unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EEXIST) {
		// dst already exists — either a legitimate pre-existing config
		// (the normal case, caller will do its own safety checks) or an
		// attacker-planted symlink. Caller decides.
		return false, os.ErrExist
	}
	// ENOSYS: kernel too old. EINVAL: filesystem rejects the flag (some
	// older NFS, FUSE, or cross-fs attempts). In both cases the syscall
	// is not usable; signal to the caller to fall through to the
	// Lstat+Rename fallback. We return (false, nil) rather than
	// propagating the errno so the caller does not have to special-case
	// syscall-availability probing — it just sees "renameat2 said no"
	// and runs the backup path.
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return false, nil
	}
	return false, fmt.Errorf("renameat2 %s -> %s: %w", src, dst, err)
}

// renameNoReplaceSupported reports whether renameNoReplace is the
// primary rename path on this platform. Callers use this to decide
// whether the Lstat-before-Rename comment in writeAtomic should claim
// "atomic via kernel" or "best-effort TOCTOU window".
const renameNoReplaceSupported = true
