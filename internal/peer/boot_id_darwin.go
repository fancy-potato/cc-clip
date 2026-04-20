//go:build darwin

package peer

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// readBootID returns a cross-boot discriminator on Darwin derived from
// `kern.boottime`. The sysctl returns a `Timeval` recording wall-clock
// time at kernel boot; formatting it as `<sec>.<usec>` matches Linux's
// /proc/sys/kernel/random/boot_id semantics closely enough for the
// recycled-PID check — any two boots produce distinct values so the
// boot-id mismatch arm in staleRegistryLock reaps a lock whose recorded
// PID was inherited across a reboot.
//
// A read failure returns ("", err); callers fall back to PID-only
// liveness checking when the boot-id is unavailable, so a
// sysctl-unavailable or sandboxed environment still degrades to the
// hard-ceiling path rather than the boot-id shortcut.
func readBootID() (string, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", err
	}
	// Zero values would compare as "always matching boot-id" and defeat
	// the discriminator. In practice the sysctl never returns zero on a
	// running system; guard defensively anyway.
	if tv.Sec == 0 && tv.Usec == 0 {
		return "", nil
	}
	return fmt.Sprintf("%d.%06d", tv.Sec, tv.Usec), nil
}
