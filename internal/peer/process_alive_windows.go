//go:build windows

package peer

import (
	"errors"
	"syscall"
)

const (
	windowsProcessStillActive = 259
	// windowsErrInvalidParameter is ERROR_INVALID_PARAMETER (87). OpenProcess
	// returns this errno for a nonexistent PID (the pid was never allocated
	// or has been fully reaped), which is the definitive "not alive" signal
	// we want to short-circuit the stale-lock wait on.
	windowsErrInvalidParameter = syscall.Errno(87)
	// windowsErrInvalidHandle is ERROR_INVALID_HANDLE (6). It is NOT what
	// OpenProcess returns for a missing pid (OpenProcess uses
	// ERROR_INVALID_PARAMETER for that), but it can surface on the
	// GetExitCodeProcess / CloseHandle paths when an already-closed or
	// malformed handle is passed — treat it as a definite "not alive"
	// there as well so a transient kernel quirk does not pin the registry
	// lock until the 10-minute hard ceiling hits.
	windowsErrInvalidHandle = syscall.Errno(6)

	// PROCESS_QUERY_LIMITED_INFORMATION is the canonical "does this pid refer
	// to a live process?" primitive on Vista+; unlike PROCESS_QUERY_INFORMATION
	// it does not require the caller to have elevated rights against processes
	// owned by other users or session 0, so our probe does not spuriously fail
	// with ERROR_ACCESS_DENIED on lockdown/AppLocker hosts. The constant
	// removes the prior `windowsErrAccessDenied` branch — any residual
	// access-denied failure is now caught by the generic "advisory error,
	// presume alive" path in processAlive.
	windowsProcessQueryLimitedInformation = 0x1000
)

// processAlive checks whether pid still refers to a live process on Windows.
// We cannot use Signal(0) here: it is unsupported and would make every stale
// lock look alive forever. OpenProcess + GetExitCodeProcess gives us the same
// "exists and still running?" predicate without disturbing the process.
//
// Error handling deliberately favors "alive, unknown" over "dead" when the
// answer is ambiguous: both OpenProcess and GetExitCodeProcess can fail for
// reasons unrelated to liveness (access denied because the process is owned
// by another user, transient kernel errors). Treating those as "dead" would
// cause staleRegistryLock to steal a lock held by a live process, so we
// return alive=true on anything that isn't the INVALID_PARAMETER signal
// that OpenProcess uses for "no such pid". syscall.Errno values come back
// wrapped in *os.SyscallError / fs.PathError on some Go versions, so we use
// errors.Is rather than a raw switch against the unwrapped value.
//
// Probe-internal errors (OpenProcess succeeded but GetExitCodeProcess failed,
// or OpenProcess failed for a reason other than INVALID_PARAMETER) are now
// surfaced as advisory errors with alive=true. staleRegistryLock treats an
// advisory error as "live but flaky probe — fall through to the hard
// ceiling" so the stale-reap window still closes after registryLockHardCeiling
// instead of waiting for the next clean probe. Returning a clean (true, nil)
// from a flaky kernel call would let a recycled-PID holder pin the lock
// forever once the original holder crashed.
func processAlive(pid int) (bool, error) {
	// Defense-in-depth: pid 0 is the System Idle Process on Windows and
	// is never a cc-clip holder. OpenProcess with pid=0 can return a
	// handle in some configurations, so we short-circuit here to avoid
	// falsely reporting the idle process as a live holder. pid 1 is also
	// rejected for symmetry with the Unix variants, where pid 1 is init.
	// readLockHolderFull already rejects these, but the per-platform
	// guard protects against a future caller that bypasses the pid-file
	// parser.
	if pid <= 1 {
		return false, nil
	}
	handle, err := syscall.OpenProcess(windowsProcessQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		// OpenProcess's "no such pid" errno is ERROR_INVALID_PARAMETER (87).
		// ERROR_INVALID_HANDLE (6) is not what OpenProcess uses here, but we
		// tolerate it as a definite "not alive" hint too for robustness
		// against edge cases (e.g. a pid that straddles a process-table
		// reshuffle). Anything else is an advisory error.
		if errors.Is(err, windowsErrInvalidParameter) {
			return false, nil
		}
		if errors.Is(err, windowsErrInvalidHandle) {
			return false, nil
		}
		return true, err
	}
	defer syscall.CloseHandle(handle)

	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		// ERROR_INVALID_HANDLE on GetExitCodeProcess means the handle we just
		// opened was invalidated out from under us (extremely rare, but the
		// conservative read is "process gone").
		if errors.Is(err, windowsErrInvalidHandle) {
			return false, nil
		}
		return true, err
	}
	return exitCode == windowsProcessStillActive, nil
}
