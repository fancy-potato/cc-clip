//go:build !linux && !darwin

package sshconfig

// renameNoReplace is a no-op stub on platforms without a native
// exclusive-rename syscall (BSDs other than Darwin, Windows, …).
// Callers fall through to the Lstat-before-Rename path, which has a
// residual TOCTOU window (an attacker with directory write access who
// can plant a symlink between our Lstat and os.Rename can race us). On
// Windows the atomic rename semantics differ enough that we rely on
// the earlier O_NOFOLLOW open and SeCreateSymbolicLinkPrivilege to keep
// the residual window narrow; on the BSDs we simply accept the window
// rather than ship a platform-specific syscall layer.
//
// Returning (false, nil) means "fall back to Lstat+Rename" — this is
// the same signal the Linux / Darwin implementations emit when the
// running kernel / filesystem doesn't support their exclusive rename.
func renameNoReplace(_, _ string) (ok bool, err error) {
	return false, nil
}

const renameNoReplaceSupported = false
