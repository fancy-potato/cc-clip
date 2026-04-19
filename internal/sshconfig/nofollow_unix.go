//go:build !windows

package sshconfig

import "syscall"

// openNoFollow is OR'd into os.OpenFile's flag argument in readConfig so
// the kernel rejects symlinks at open time with ELOOP. This is the only
// way to close the TOCTOU between a prior Lstat check and the read.
const openNoFollow = syscall.O_NOFOLLOW
