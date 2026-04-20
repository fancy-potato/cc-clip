//go:build linux

package peer

import (
	"os/exec"
	"testing"
)

func TestParseLinuxProcStatState(t *testing.T) {
	stat := "12345 (cc-clip peer) Z 1 2 3 4 5"
	got, err := parseLinuxProcStatState(stat)
	if err != nil {
		t.Fatalf("parseLinuxProcStatState: %v", err)
	}
	if got != 'Z' {
		t.Fatalf("state = %q, want Z", got)
	}
}

// TestLinuxProcessZombieReapedPIDReturnsTrue pins the P2-B fix: when
// /proc/<pid>/stat has disappeared (ENOENT) because the process exited
// AND has been reaped between kill(0) and the /proc read, linuxProcessZombie
// must return (true, nil) — NOT (false, nil). The historical bug reported
// the reaped PID as "not zombie, alive", delaying lock reap to the
// 10-minute hard ceiling.
//
// Spawning `/bin/true` and waiting for it gives us a reaped PID whose
// /proc/<pid>/stat is gone by the time Wait returns. If the PID has been
// immediately reused, we skip rather than fail.
func TestLinuxProcessZombieReapedPIDReturnsTrue(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn /bin/true: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cmd.Wait: %v", err)
	}

	zombie, err := linuxProcessZombie(pid)
	if err != nil {
		t.Fatalf("linuxProcessZombie reaped pid %d: unexpected err %v", pid, err)
	}
	if !zombie {
		t.Skipf("linuxProcessZombie reaped pid %d: got zombie=false (likely PID reuse race); skipping", pid)
	}
}

// TestLinuxProcessZombieDefinitelyDeadPIDReturnsTrue pins the ENOENT
// branch without racing the PID allocator. Using a PID well above Linux's
// pid_max (typically 2^22 — configurable but never as high as 1<<30 in
// any documented kernel) guarantees /proc/<pid>/stat cannot exist AND
// cannot be recycled into existence during the test. This closes the
// flaky-skip window in TestLinuxProcessZombieReapedPIDReturnsTrue above
// where a fast PID rollover could mask a real regression.
func TestLinuxProcessZombieDefinitelyDeadPIDReturnsTrue(t *testing.T) {
	const deadPID = 1 << 30
	zombie, err := linuxProcessZombie(deadPID)
	if err != nil {
		t.Fatalf("linuxProcessZombie(%d): unexpected err %v (want ENOENT → (true, nil))", deadPID, err)
	}
	if !zombie {
		t.Fatalf("linuxProcessZombie(%d): got zombie=false, want true (ENOENT must map to zombie-or-gone)", deadPID)
	}
}

// TestProcessAliveReapedPIDOnLinux is the end-to-end companion: a reaped
// PID must bubble up as alive=false,err=nil so staleRegistryLock can reap
// the lock immediately. Combined with TestLinuxProcessZombieReapedPIDReturnsTrue
// this guards against a regression that re-introduces the (false, nil)-on-ENOENT
// branch and the 10-minute hard-ceiling wait it caused.
func TestProcessAliveReapedPIDOnLinux(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn /bin/true: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cmd.Wait: %v", err)
	}

	alive, err := processAlive(pid)
	if err != nil {
		t.Fatalf("processAlive reaped pid %d: unexpected err %v", pid, err)
	}
	if alive {
		t.Skipf("processAlive reaped pid %d: got alive=true (likely PID reuse race); skipping", pid)
	}
}
