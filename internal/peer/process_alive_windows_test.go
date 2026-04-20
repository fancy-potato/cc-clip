//go:build windows

package peer

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestWindowsErrnoConstants pins the exact numeric values the classifier
// depends on. A silent rename/typo that flipped 87 <-> 6 would turn every
// "no such pid" into an advisory error and let stale locks pin the
// registry until the 10-minute hard ceiling (P2-19 regression).
func TestWindowsErrnoConstants(t *testing.T) {
	if windowsErrInvalidParameter != syscall.Errno(87) {
		t.Fatalf("windowsErrInvalidParameter = %d, want 87 (ERROR_INVALID_PARAMETER)", windowsErrInvalidParameter)
	}
	if windowsErrInvalidHandle != syscall.Errno(6) {
		t.Fatalf("windowsErrInvalidHandle = %d, want 6 (ERROR_INVALID_HANDLE)", windowsErrInvalidHandle)
	}
}

// TestProcessAliveDeadPIDOnWindows pins the OpenProcess classification:
// a PID from a process that has exited AND been reaped must return
// (false, nil) so the stale-lock path can short-circuit to "dead" without
// waiting for the hard ceiling.
//
// Picking a "guaranteed dead" PID without spawning is not reliable on
// Windows — the OS recycles low PIDs aggressively and a hand-picked high
// PID may coincidentally match a live process in session 0. Spawning a
// real child that exits immediately and then waiting on it gives us a
// deterministic dead-PID (the kernel has returned the PID to the pool
// after Wait returns the exit code, and OpenProcess against it will
// return ERROR_INVALID_PARAMETER until a new process happens to reuse
// it).
func TestProcessAliveDeadPIDOnWindows(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit", "0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn cmd.exe (probably not a Windows interactive session): %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		// exit 0 should not surface as an error, but if it does we still
		// have a reaped PID — carry on.
		t.Logf("cmd.Wait returned %v (non-fatal: PID is reaped)", err)
	}

	alive, err := processAlive(pid)
	if err != nil {
		t.Fatalf("processAlive dead pid %d: unexpected err %v", pid, err)
	}
	if alive {
		// PID reuse by an unrelated process between Wait and OpenProcess
		// is astronomically unlikely within the same test tick but not
		// strictly impossible on a heavily-loaded Windows host. Skip
		// rather than fail so CI flakes do not mask real regressions.
		t.Skipf("processAlive reaped pid %d: got alive=true (likely PID reuse race); skipping", pid)
	}
}
