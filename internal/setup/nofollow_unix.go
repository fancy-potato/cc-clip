//go:build !windows

package setup

import "syscall"

const sysNoFollow = syscall.O_NOFOLLOW
