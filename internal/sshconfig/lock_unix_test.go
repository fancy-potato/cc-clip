//go:build !windows

package sshconfig

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestAcquireConfigLockChownsSidecarToOwner pins that the sidecar lock
// inherits the requested owner uid/gid. Under sudo (euid=0, owner=user),
// without this chown the next non-sudo run finds a root-owned
// `~/.ssh/config.cc-clip.lock` it cannot reopen — turning a routine
// `cc-clip setup` into an opaque "permission denied" the user has to
// debug by hand.
//
// We can't run as root in unit tests, so the test exercises the
// no-chown-needed branch (ownerUID == euid) and verifies the lock file
// ends up owner-readable. The non-trivial chown branch (ownerUID != euid)
// is covered by the error-path test below — it pins that we fail closed
// instead of silently leaving a wrong-owner sidecar.
func TestAcquireConfigLockChownsSidecarToOwner(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	if err := os.WriteFile(configPath, []byte("Host example\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	euid := os.Geteuid()
	egid := os.Getegid()

	release, err := acquireConfigLock(configPath, euid, egid, true)
	if err != nil {
		t.Fatalf("acquireConfigLock: %v", err)
	}
	defer release()

	lockPath := configPath + ".cc-clip.lock"
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("non-Unix file info; ownership not testable")
	}
	if int(st.Uid) != euid {
		t.Fatalf("lock file uid = %d, want euid %d", st.Uid, euid)
	}
}

// TestAcquireConfigLockFailsClosedOnChownError pins the security invariant
// that we propagate (rather than swallow) a chown failure on the sidecar
// lock. Pre-fix, a sudo'd setup left the sidecar root-owned silently; the
// fix opts to fail loudly so the operator can rm the leftover and retry,
// rather than ship a broken state. Skipped when the test process is root
// (in which case chown to an arbitrary uid would actually succeed).
func TestAcquireConfigLockFailsClosedOnChownError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chown to other uids would succeed; cannot exercise EPERM path")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	if err := os.WriteFile(configPath, []byte("Host example\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// Pick a uid that we definitely don't equal. Root (uid=0) is the
	// canonical target a sudo'd setup would chown to; any non-root user
	// trying to chown a freshly-created file to root yields EPERM.
	otherUID := 0
	if os.Geteuid() == 0 {
		otherUID = 1
	}
	release, err := acquireConfigLock(configPath, otherUID, 0, true)
	if err == nil {
		release()
		t.Fatalf("expected acquireConfigLock to fail when chown would EPERM")
	}
	if !strings.Contains(err.Error(), "chown ssh_config lock") {
		t.Fatalf("expected chown-wrapped error, got %v", err)
	}
}

// TestAcquireConfigLockSidecarPersistsAcrossRuns pins the contract
// documented at the top of lock_unix.go: "The lock file persists between
// runs (empty 0600 file)." An overzealous future cleanup (e.g. a release
// func that removed the sidecar instead of just unlocking) would break the
// two-process serialization contract — the second process would race the
// cleanup and create a fresh sidecar it locks independently of the first.
// Without this test, that regression would only surface as intermittent
// double-writes to ~/.ssh/config under `cc-clip setup` concurrency, which
// is exactly what the lock exists to prevent.
func TestAcquireConfigLockSidecarPersistsAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	if err := os.WriteFile(configPath, []byte("Host example\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	euid := os.Geteuid()
	egid := os.Getegid()

	lockPath := configPath + ".cc-clip.lock"

	for i := 0; i < 3; i++ {
		release, err := acquireConfigLock(configPath, euid, egid, true)
		if err != nil {
			t.Fatalf("iteration %d: acquireConfigLock: %v", i, err)
		}
		// Sidecar must exist WHILE the lock is held.
		if _, err := os.Stat(lockPath); err != nil {
			t.Fatalf("iteration %d: sidecar missing while lock held: %v", i, err)
		}
		release()
		// And sidecar must STILL exist after release — releasing must not
		// delete the file, or the next caller would race on a re-create.
		info, err := os.Stat(lockPath)
		if err != nil {
			t.Fatalf("iteration %d: sidecar deleted by release; two concurrent callers would race on re-create: %v", i, err)
		}
		// 0600 mode is the contract; confirm it wasn't loosened between runs.
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("iteration %d: sidecar mode = %o, want 0600", i, mode)
		}
	}
}
