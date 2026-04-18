//go:build windows

package setup

// Windows has no O_NOFOLLOW. Symlink-to-attacker-target is still guarded by
// the explicit os.Lstat + O_EXCL check in writeBackupFileNoFollow; the caller
// rejects any pre-existing symlink and O_EXCL prevents a late race from
// clobbering a regular file. Combined, this matches the Unix posture even
// without an equivalent flag.
const sysNoFollow = 0
