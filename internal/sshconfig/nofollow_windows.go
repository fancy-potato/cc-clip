//go:build windows

package sshconfig

// Windows' CreateFile does not expose an O_NOFOLLOW equivalent that Go's
// os.OpenFile forwards, so openNoFollow is 0 and readConfig relies on the
// fallback Lstat-after-open check to refuse symlinks. Windows symlink
// creation requires SeCreateSymbolicLinkPrivilege, so the degraded TOCTOU
// window is narrow.
const openNoFollow = 0
