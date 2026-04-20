//go:build darwin

package peer

import (
	"errors"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// darwinSZOMB mirrors the BSD `SZOMB` process state constant from
// <sys/proc.h>. The value has been stable across Darwin releases
// (SIDL=1, SRUN=2, SSLEEP=3, SSTOP=4, SZOMB=5). golang.org/x/sys/unix
// does not export it under a named symbol, so we redeclare it here —
// the on-wire layout of `struct kinfo_proc.kp_proc.p_stat` is what
// anchors the value, not the symbol name.
const darwinSZOMB int8 = 5

// errZombieProbeUnavailable signals that darwinProcessZombie could not
// authoritatively determine whether `pid` is a zombie — typically because
// the sysctl was refused (EPERM/EACCES) in a sandboxed process that still
// has permission to call kill(0). The caller (processAlive) translates
// this into "zombie-unknown: trust the kill(0) result as-is and do NOT
// mark the process advisorily alive". That avoids a sandboxed macOS
// context pinning the registry lock to the 10-minute hard ceiling on a
// crashed holder just because our zombie probe is blocked.
var errZombieProbeUnavailable = errors.New("darwin zombie probe unavailable (sandboxed or permission denied)")

// processAlive on Darwin combines kill(pid, 0) with a best-effort
// sysctl(kern.proc.pid.<pid>) zombie check. BSD's kill(pid, 0) returns 0
// for zombies, so without the sysctl check a crashed-but-unreaped
// cc-clip would pin the registry lock until the 10-minute hard ceiling.
// The sysctl probe reads `kinfo_proc.kp_proc.p_stat` and treats SZOMB
// (5) as dead.
//
// The sysctl path has two failure modes:
//   - Hard flake (short read, unknown kernel error): advisory-alive
//     (alive=true, err!=nil) so staleRegistryLock falls through to the
//     hard-ceiling path. A real kernel hiccup must not demote a live
//     process to "dead".
//   - Permission-denied / sandboxed (EPERM/EACCES on sysctl): the probe
//     is inconclusive rather than flaky. darwinProcessZombie surfaces
//     errZombieProbeUnavailable; processAlive reports (true, nil) so
//     staleRegistryLock's hard-ceiling clock still applies and the lock
//     is reaped at registryLockHardCeiling rather than pinned forever.
//     This is a deliberate safety/delay trade: (false, nil) would let
//     a crashed holder's zombie PID steal its own lock; (true, nil)
//     with an advisory error would flood logs every poll. Reporting
//     alive=true with nil err keeps the common-case path quiet and
//     defers to the hard ceiling for the occasional crashed holder
//     under sandbox.
func processAlive(pid int) (bool, error) {
	// Defense-in-depth: pid 0 is never a valid process; pid 1 is init/
	// launchd and can't be a cc-clip holder. readLockHolderFull already
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
		zombie, zErr := darwinProcessZombie(pid)
		if zErr != nil {
			if errors.Is(zErr, errZombieProbeUnavailable) {
				// Zombie-unknown: the sysctl was refused by the kernel
				// (sandbox). Fall back to kill(0) alone. Report alive=true
				// but nil err: this is NOT a transient flake to surface,
				// it's a deliberate "probe not available on this host".
				// staleRegistryLock's hard-ceiling branch still reaps the
				// lock at registryLockHardCeiling if the holder is really
				// gone; we just can't short-circuit via the zombie path.
				return true, nil
			}
			// Hard flake: short read / unknown kernel error. Advisory —
			// keep alive=true so staleRegistryLock falls through to the
			// hard-ceiling path rather than treating a probe hiccup as
			// "dead".
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

// darwinProcessZombie queries `kern.proc.pid.<pid>` via sysctl and reports
// whether the returned kinfo_proc.kp_proc.p_stat equals SZOMB. A missing
// entry is reported as (true, nil): zombie==true flips the caller
// (processAlive) into the "not alive" branch, which is what we want when
// the PID vanished between kill(0) and the sysctl — without this a
// SysctlRaw empty-result (Darwin returns (nil, nil) for nonexistent PIDs
// rather than ESRCH) would fall through to the short-read guard and
// return an advisory error, delaying lock reap until the 10-minute hard
// ceiling. Treating "gone" as "zombie" is semantically sound for the
// caller: both mean "holder is dead, reap the lock now."
//
// EPERM / EACCES from sysctl are distinct from a hard flake: they mean
// the current process is sandboxed (App Sandbox, SIP-restricted binary,
// etc.) and CANNOT inspect kinfo_proc at all. Returning a hard advisory
// error in that case pins the registry lock to the 10-minute hard ceiling
// on every crashed holder. Instead, surface errZombieProbeUnavailable so
// the caller can distinguish "probe not allowed" from "probe hiccuped"
// and fall back to kill(0) semantics without the 10-minute wait multiplier.
func darwinProcessZombie(pid int) (bool, error) {
	raw, err := unix.SysctlRaw("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return true, nil
		}
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			return false, errZombieProbeUnavailable
		}
		return false, err
	}
	// Darwin's sysctl path returns an empty byte slice (len(raw) == 0) when
	// the requested PID does not exist, instead of surfacing ESRCH. Check
	// this BEFORE the short-read guard: an empty buffer is the definitive
	// "not present" signal, not a flaky probe.
	if len(raw) == 0 {
		return true, nil
	}
	if len(raw) < int(unsafe.Sizeof(unix.KinfoProc{})) {
		// Short read — refuse to index into a truncated struct. Advisory
		// error so the caller surfaces the flake and falls through to the
		// hard-ceiling path.
		return false, errors.New("darwin sysctl kern.proc.pid: short kinfo_proc")
	}
	kp := (*unix.KinfoProc)(unsafe.Pointer(&raw[0]))
	return kp.Proc.P_stat == darwinSZOMB, nil
}
