//go:build !windows

package sshconfig

import (
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
func acquireConfigLock(path string) (release func(), err error) {
	lockPath := path + ".cc-clip.lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, err
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
