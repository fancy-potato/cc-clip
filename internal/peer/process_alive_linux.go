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

func linuxProcessZombie(pid int) (bool, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
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
