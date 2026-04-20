//go:build linux

package peer

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// processAlive checks whether pid still refers to a live process on Linux.
// In addition to kill(pid, 0), it inspects /proc/<pid>/stat so zombie
// processes are treated as dead holders: a crashed `cc-clip peer` that has
// exited but not yet been waited on must not pin the registry lock until the
// hard ceiling expires.
func processAlive(pid int) (bool, error) {
	// Defense-in-depth: pid 0 is never a valid process; pid 1 is init/
	// systemd and can't be a cc-clip holder. readLockHolderFull already
	// rejects these, but the per-platform guard protects against a future
	// caller that bypasses the pid-file parser.
	if pid <= 1 {
		return false, nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		zombie, zErr := linuxProcessZombie(pid)
		if zErr != nil {
			return true, zErr
		}
		if zombie {
			return false, nil
		}
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

// linuxProcessZombie reports whether pid is a zombie OR has already been
// reaped between the kill(0) probe and this read. The ENOENT branch
// returns true, not false: if /proc/<pid>/stat has vanished the process
// is gone, which is semantically equivalent to "zombie" for the caller
// (processAlive flips zombie==true into alive=false). Returning (false,
// nil) on ENOENT — as the original code did — would falsely report the
// PID as live, delaying lock reap to the 10-minute hard ceiling.
func linuxProcessZombie(pid int) (bool, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// ENOENT: /proc entry disappeared between kill(0) and this
			// read. Treat as zombie-or-gone so processAlive reports
			// alive=false and the registry lock is reaped promptly.
			return true, nil
		}
		return false, err
	}
	state, err := parseLinuxProcStatState(string(data))
	if err != nil {
		return false, err
	}
	return state == 'Z' || state == 'X', nil
}

func parseLinuxProcStatState(stat string) (byte, error) {
	closeIdx := strings.LastIndex(stat, ")")
	if closeIdx == -1 || closeIdx+2 >= len(stat) {
		return 0, fmt.Errorf("malformed /proc stat: missing state field")
	}
	return stat[closeIdx+2], nil
}
