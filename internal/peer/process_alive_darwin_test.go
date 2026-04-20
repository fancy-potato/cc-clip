//go:build darwin

package peer

import (
	"os/exec"
	"testing"
)

// TestProcessAliveDeadPIDOnDarwin pins the (alive=false, err=nil)
// contract for a definitely-dead PID on Darwin. The historical regression
// was that `unix.SysctlRaw("kern.proc.pid", <gone-pid>)` returns
// `(nil, nil)` rather than ESRCH on macOS, so darwinProcessZombie's
// short-kinfo_proc guard would fire and return an advisory error — flipping
// the caller into the "alive=true, advisory err" branch and delaying lock
// reap to the 10-minute hard ceiling.
//
// Spawning `/usr/bin/true` and waiting for it gives us a reaped PID that
// is safe to probe: the kernel has returned the PID to the pool, so
// neither kill(0) nor the sysctl can find a live process to match. If the
// PID has been immediately reused by an unrelated process (astronomically
// unlikely within one test tick), we skip rather than fail.
func TestProcessAliveDeadPIDOnDarwin(t *testing.T) {
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn /usr/bin/true: %v", err)
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

// TestDarwinProcessZombieReportsDeadPID pins the sysctl-layer contract
// exercised by processAlive: an empty result from
// unix.SysctlRaw("kern.proc.pid", <gone-pid>) is translated into
// (zombie=true, err=nil) so processAlive's `if zombie { return false, nil }`
// arm fires — which is what reaps a stale lock promptly instead of
// waiting for the hard ceiling.
func TestDarwinProcessZombieReportsDeadPID(t *testing.T) {
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn /usr/bin/true: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cmd.Wait: %v", err)
	}

	zombie, err := darwinProcessZombie(pid)
	if err != nil {
		t.Fatalf("darwinProcessZombie reaped pid %d: unexpected err %v (want nil — Darwin returns empty buffer for missing PID, not ESRCH)", pid, err)
	}
	if !zombie {
		t.Skipf("darwinProcessZombie reaped pid %d: got zombie=false (likely PID reuse race); skipping", pid)
	}
}
