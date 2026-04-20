//go:build darwin

package peer

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// readBootID returns a cross-boot discriminator on Darwin.
//
// Primary source: `kern.bootsessionuuid` — a UUID assigned at kernel boot,
// stable for the lifetime of the session, immune to wall-clock changes.
// This is the correct discriminator to use for "has the machine rebooted?"
// because `kern.boottime` (the previous source) returns a Timeval derived
// from wall-clock time at boot and can change mid-session when the system
// clock is stepped (settimeofday, manual adjustment, virt-host resume). A
// clock step while a cc-clip process was holding the registry lock would
// otherwise cause `staleRegistryLock` to observe a boot-id mismatch and
// reap a still-live lock on the next acquisition attempt.
//
// Fallback: `kern.boottime` for older macOS kernels (pre-10.12 or
// sandboxed sysctl allowlists that only permit boottime). The fallback
// value is still a useful cross-boot signal between two reboots even if
// it's clock-step-fragile within a single session.
//
// A read failure from both sources returns ("", err); callers fall back
// to PID-only liveness checking when the boot-id is unavailable.
func readBootID() (string, error) {
	if uuid, err := unix.Sysctl("kern.bootsessionuuid"); err == nil {
		uuid = strings.TrimSpace(uuid)
		if uuid != "" {
			return uuid, nil
		}
	}
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", err
	}
	if tv.Sec == 0 && tv.Usec == 0 {
		return "", nil
	}
	return fmt.Sprintf("%d.%06d", tv.Sec, tv.Usec), nil
}
