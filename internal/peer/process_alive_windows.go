//go:build windows

package peer

import "syscall"

const (
	windowsProcessStillActive = 259
	windowsErrAccessDenied    = syscall.Errno(5)
	windowsErrInvalidParam    = syscall.Errno(87)
)

// processAlive checks whether pid still refers to a live process on Windows.
// We cannot use Signal(0) here: it is unsupported and would make every stale
// lock look alive forever. OpenProcess + GetExitCodeProcess gives us the same
// "exists and still running?" predicate without disturbing the process.
func processAlive(pid int) (bool, error) {
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		switch err {
		case windowsErrAccessDenied:
			return true, nil
		case windowsErrInvalidParam:
			return false, nil
		default:
			return false, err
		}
	}
	defer syscall.CloseHandle(handle)

	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false, err
	}
	return exitCode == windowsProcessStillActive, nil
}
