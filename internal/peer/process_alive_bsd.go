//go:build !windows && !linux && !darwin

// Package-level note (non-Darwin Unix): this file's processAlive relies on
// kill(pid, 0), which on BSD/illumos returns 0 for ZOMBIE processes — so a
// crashed cc-clip whose parent has not yet reaped it will be reported as
// "alive" here. That is a known gap: the registry fails closed via the
// 10-minute `registryLockHardCeiling` in registry.go, which will reap such
// a holder even when the per-platform zombie check is unavailable. Darwin
// has a dedicated `process_alive_darwin.go` that adds a sysctl-based
// zombie probe; adding one for other BSDs is a natural follow-up when an
// actual user runs cc-clip on FreeBSD/OpenBSD/illumos.

package peer

import (
	"errors"
	"os"
	"syscall"
)

// processAlive uses the POSIX "signal 0" probe to test whether pid is a
// live process. Signal 0 performs permission and existence checks but does
// not deliver a signal. EPERM means the process exists (we lack permission
// to signal it) and ESRCH means the process has exited.
//
// Unknown kill(2) errors (anything that isn't ESRCH/EPERM/ProcessDone) are
// returned with alive=true as an advisory: a transient kernel hiccup must
// not demote a live process to "dead", which would let staleRegistryLock
// steal a lock held by a live process. staleRegistryLock treats the
// advisory error as "fall through to the hard ceiling" so the lock still
// gets reaped after registryLockHardCeiling if the holder really is gone.
//
// Zombie caveat: on non-Linux Unix (BSD/illumos) kill(pid, 0) returns 0
// for zombies, so this function will report a crashed-but-unreaped holder
// as alive. The 10-minute hard ceiling in staleRegistryLock catches that.
func processAlive(pid int) (bool, error) {
	// Defense-in-depth: pid 0 is never a valid process; pid 1 is init
	// and can't be a cc-clip holder. readLockHolderFull already rejects
	// these, but the per-platform guard protects against a future caller
	// that bypasses the pid-file parser.
	if pid <= 1 {
		return false, nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return false, nil
	}
	return true, err
}
