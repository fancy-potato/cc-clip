//go:build !linux

package peer

// readBootID is a no-op on non-Linux platforms. macOS exposes kern.boottime
// via sysctl and Windows exposes GetTickCount64, but neither is read here:
// the registry+lock files live under ~/.cache/cc-clip on the SAME machine
// that's holding the lock, and the cc-clip remote-daemon target is Linux,
// so the platforms most exposed to PID-reuse-across-reboot already get the
// stronger Linux check. Returning ("", nil) signals "boot-id unavailable"
// to staleRegistryLock, which then falls back to the PID-and-hard-ceiling
// path that was the only option before this guard existed.
func readBootID() (string, error) {
	return "", nil
}
