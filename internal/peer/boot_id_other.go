//go:build !linux && !darwin

package peer

// readBootID is a no-op on platforms without a first-party boot-id source.
// Linux reads /proc/sys/kernel/random/boot_id (see boot_id_linux.go) and
// Darwin reads kern.boottime (see boot_id_darwin.go); Windows is the
// remaining case here. Returning ("", nil) signals "boot-id unavailable"
// to staleRegistryLock, which then falls back to the PID-and-hard-ceiling
// path that was the only option before the boot-id guard existed.
//
// TODO(windows): GetTickCount64 is monotonically non-decreasing since
// boot but NOT a boot-id — two boots can produce the same tick-at-claim
// value modulo the uptime. A better pick is HKLM\SYSTEM\CurrentControlSet
// \Control\Windows\SystemBootTime or KUSER_SHARED_DATA.SystemTime at
// claim; both are out of scope for now.
func readBootID() (string, error) {
	return "", nil
}
