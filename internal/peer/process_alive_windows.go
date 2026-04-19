//go:build windows

package peer

import (
	"errors"
	"syscall"
)

const (
	windowsProcessStillActive = 259
	windowsErrInvalidParam    = syscall.Errno(87)

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
	handle, err := syscall.OpenProcess(windowsProcessQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windowsErrInvalidParam) {
			return false, nil
		}
		return true, err
	}
	defer syscall.CloseHandle(handle)

	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return true, err
	}
	return exitCode == windowsProcessStillActive, nil
}
