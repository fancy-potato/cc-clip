//go:build !windows

package sshconfig

import (
	"errors"
	"os"
	"syscall"
)

// captureOwnership extracts the uid/gid backing the given FileInfo. Returns
// ok=false when the underlying Stat_t is missing (e.g. a non-Unix FS on a
// FUSE mount that hides ownership). Callers then skip chown preservation.
func captureOwnership(info os.FileInfo) (uid, gid int, ok bool) {
	if info == nil {
		return 0, 0, false
	}
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// applyOwnershipFd is the fd-based variant of applyOwnership. Used by
// writeAtomic on the still-open tmp file fd so the chmod/chown can't be
// races by an attacker swapping the tmpfile between os.File.Close() and
// a path-based os.Chown(tmpName, …). Mirrors the EPERM gid-only-fallback
// semantics of applyOwnership exactly.
func applyOwnershipFd(fd uintptr, uid, gid int) error {
	err := syscall.Fchown(int(fd), uid, gid)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EPERM) {
		return err
	}
	if os.Geteuid() != uid {
		return err
	}
	if gidErr := syscall.Fchown(int(fd), -1, gid); gidErr != nil {
		if !errors.Is(gidErr, syscall.EPERM) {
			return gidErr
		}
		// EPERM again — accept gid drift.
	}
	return nil
}

// applyOwnership chown's path to (uid, gid). Used by writeAtomic to prevent
// a sudo-driven rewrite from silently flipping ~/.ssh/config from user-owned
// to root-owned, which would break the subsequent user-mode OpenSSH/cc-clip
// reads.
//
// EPERM fallback: non-privileged processes cannot change gid to a
// supplementary group (Linux rejects chown(uid, supplementary_gid) without
// CAP_CHOWN even when uid == euid). In the common multi-user-lab scenario
// ~/.ssh/config may have a group other than the user's primary gid (e.g.
// inherited from a shared dotfiles directory). When the EPERM happens and
// we're already running as the owning uid, we accept the gid drift: the
// temp file was created with the process's egid so ownership is still
// user-readable; flipping the gid is cosmetic here and aborting would
// defeat the exact multi-laptop SetEnv feature this chown is guarding.
// The sudo case (euid != uid) still propagates EPERM because we must NOT
// rename a root-owned temp file over a user's config.
func applyOwnership(path string, uid, gid int) error {
	err := os.Chown(path, uid, gid)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EPERM) {
		return err
	}
	// EPERM: we must still uphold the "never rename a wrong-uid temp
	// file over the user's config" invariant. That invariant is broken
	// when uid != euid (e.g. running under sudo and preserving the
	// original user's uid) — in that case we MUST propagate the EPERM.
	//
	// When uid == euid, the temp file was created with our euid already
	// (os.CreateTemp inherits the process uid), so the only possible
	// drift is on gid: the target's gid is a supplementary group the
	// kernel refuses to set via chown(uid, gid) without CAP_CHOWN. Try
	// a gid-only chown (uid=-1) first — some kernels allow that when a
	// combined chown was refused, which preserves the user's configured
	// group. If the gid-only chown also fails, accept gid drift: the
	// alternative is aborting the whole SetEnv feature for the common
	// multi-user-lab case where ~/.ssh/config has a gid other than the
	// user's primary gid (P1-1 review fix).
	if os.Geteuid() != uid {
		return err
	}
	if gidErr := os.Chown(path, -1, gid); gidErr != nil {
		// Only swallow another EPERM (the specific case we're handling:
		// the gid is still a supplementary group we can't set). EIO,
		// ENOENT, EROFS, and every other errno class indicate a real
		// filesystem problem that would be hidden by returning nil —
		// propagate them so writeAtomic fails closed rather than
		// renaming a temp file whose ownership might be wrong in a way
		// we haven't diagnosed.
		if !errors.Is(gidErr, syscall.EPERM) {
			return gidErr
		}
		// EPERM again — accept gid drift as documented above.
	}
	return nil
}
