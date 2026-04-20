//go:build !windows

package sshconfig

import (
	"fmt"
	"os"
	"syscall"
)

// acquireConfigLock takes an exclusive advisory lock on a sidecar file
// next to `path` (e.g. ~/.ssh/config.cc-clip.lock). Locking the sidecar
// rather than ~/.ssh/config itself avoids interfering with the atomic
// rename-in-place pattern used by writeAtomic — flock on the target file
// would be lost across rename since the new inode is distinct.
//
// Returns a release func; callers must defer it. On any error the release
// func is a no-op. The lock file persists between runs (empty 0600 file)
// so that two concurrent Applies both contend on the same inode.
//
// ownerUID / ownerGID / hasOwnerID describe the desired owner of the
// sidecar lock file (typically the owner of `path` itself). Without
// chowning, a `sudo cc-clip setup` run creates the sidecar root-owned
// (the process's euid is 0), which leaves a stale root-owned file in
// ~/.ssh/ that the user can no longer flock without sudo on subsequent
// non-sudo invocations. When ownership info is unavailable
// (hasOwnerID=false, e.g. non-Unix FS) chown is skipped.
func acquireConfigLock(path string, ownerUID, ownerGID int, hasOwnerID bool) (release func(), err error) {
	lockPath := path + ".cc-clip.lock"
	// O_NOFOLLOW mirrors the same defense applied to the config itself in
	// readConfig. Without it, an attacker with write access to ~/.ssh/
	// could pre-plant `config.cc-clip.lock` as a symlink to any
	// user-writable path, causing us to flock+close (and potentially
	// materialize a 0600 file at) an unexpected target. Consistent policy
	// even though the blast radius here is smaller than the config itself.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return func() {}, err
	}
	// Match the config's owner. Only chown when our euid differs from the
	// target uid — typical sudo case (euid=0, ownerUID=user). If euid
	// already matches ownerUID, the file we just opened (or pre-existing)
	// is already correctly owned and chown would be a no-op or a needless
	// EPERM. We propagate any chown error in the sudo case because a
	// half-fixed lock file (root-owned but flockable) is worse than a
	// loud failure: the user can rm the file and rerun.
	if hasOwnerID && os.Geteuid() != ownerUID {
		if chErr := applyOwnership(lockPath, ownerUID, ownerGID); chErr != nil {
			f.Close()
			return func() {}, fmt.Errorf("chown ssh_config lock %s to owner uid=%d gid=%d: %w", lockPath, ownerUID, ownerGID, chErr)
		}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return func() {}, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
