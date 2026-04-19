//go:build !windows

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
func processAlive(pid int) (bool, error) {
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
	return false, err
}
