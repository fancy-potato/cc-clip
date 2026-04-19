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
	if errors.Is(err, syscall.EPERM) && os.Geteuid() == uid {
		return nil
	}
	return err
}
