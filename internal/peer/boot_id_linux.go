//go:build linux

package peer

import (
	"os"
	"strings"
)

// readBootID returns the kernel-reported boot identifier on Linux. The file
// at /proc/sys/kernel/random/boot_id is regenerated on every boot, so its
// value is a perfect cross-boot discriminator — we record it alongside the
// lock-holder PID and treat any later PID with a different recorded boot-id
// as a recycled-from-a-prior-boot collision (i.e. the original holder is
// dead, even if the same numeric PID happens to be alive again under the
// current boot). A read failure returns ("", err); callers fall back to
// PID-only liveness checking when the boot-id is unavailable.
func readBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
