//go:build !windows

package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestApplyOwnershipReturnsNilWhenChownSucceeds covers the straight-through
// success path. writeAtomic only invokes applyOwnership when meta.hasOwnerID
// is true, so we call the helper directly rather than driving Apply — the
// real chown behavior in CI depends on the sandbox's uid/gid which we
// cannot predict, and the two-line helper is the unit under test anyway.
func TestApplyOwnershipReturnsNilWhenChownSucceeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// chown to our own uid/gid is always a legal no-op.
	self := os.Geteuid()
	gid := os.Getegid()
	if err := applyOwnership(path, self, gid); err != nil {
		t.Fatalf("applyOwnership self→self should succeed, got %v", err)
	}
}

// TestApplyOwnershipIgnoresEPERMWhenUIDMatches pins the P1-1 review fix. On
// Linux, chown(uid, supplementary_gid) fails EPERM for a non-privileged
// process even when uid == euid. If we propagated that EPERM we would abort
// the atomic rewrite in the common multi-user-lab scenario (~/.ssh/config
// has a gid other than the user's primary gid). The fallback accepts the
// gid drift in this case because the temp file was created with our egid
// so ownership is still user-readable; the alternative — aborting the
// rewrite — would disable the entire SetEnv feature for those users.
func TestApplyOwnershipIgnoresEPERMWhenUIDMatches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Simulate what os.Chown does on EPERM by calling the helper with a
	// gid we are definitely NOT a member of. Most systems do not have a
	// gid 0xFFFF set up as a supplementary group for the test user, so
	// this chown should fail EPERM on Linux for the non-root case.
	// When running as root this test becomes a no-op assertion (chown
	// succeeds and we take the err==nil branch) — that's still correct.
	self := os.Geteuid()
	err := applyOwnership(path, self, 0xFFFF)
	if err == nil {
		// Running as root (or on a platform that permits this chown).
		// Either way the helper returned the expected nil for "success".
		return
	}
	// Non-root and the kernel rejected the gid change. The helper must
	// have swallowed the EPERM because uid still matches our euid.
	if errors.Is(err, syscall.EPERM) {
		t.Fatalf("applyOwnership should swallow EPERM when uid == euid; got %v", err)
	}
	t.Fatalf("unexpected error: %v", err)
}

// TestApplyOwnershipPropagatesEPERMWhenUIDDiffers pins the sudo-case half
// of the P1-1 fix: if the target uid is NOT our euid we MUST return the
// EPERM, because renaming the temp file over the user's config would
// leave it owned by the wrong user. Only exercisable under test
// environments that can observe a uid we don't own — skip otherwise.
func TestApplyOwnershipPropagatesEPERMWhenUIDDiffers(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can chown to any uid; EPERM path not reachable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// uid 0 (root) is a uid we definitely don't own. Non-root chown to
	// uid 0 always returns EPERM.
	err := applyOwnership(path, 0, os.Getegid())
	if err == nil {
		t.Fatal("expected EPERM chown-ing to root as non-root user")
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("expected EPERM, got %v", err)
	}
}
