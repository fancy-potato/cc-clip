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

// TestApplyOwnershipIgnoresEPERMWhenUIDMatches pins the P1-1 review fix
// (and the P3-D narrowing to EPERM-ONLY swallow). On Linux,
// chown(uid, supplementary_gid) fails EPERM for a non-privileged
// process even when uid == euid. If we propagated that EPERM we would
// abort the atomic rewrite in the common multi-user-lab scenario
// (~/.ssh/config has a gid other than the user's primary gid). The
// fallback accepts the gid drift in this case because the temp file
// was created with our egid so ownership is still user-readable; the
// alternative — aborting the rewrite — would disable the entire
// SetEnv feature for those users. After P3-D the swallow is strictly
// scoped to errno EPERM — every other error class from the fallback
// gid-only chown propagates, see TestApplyOwnershipPropagatesNonEPERMErrors.
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

// TestApplyOwnershipPropagatesNonEPERMErrors pins P3-D: applyOwnership
// must only swallow errno EPERM. Every other errno class from either
// the primary chown or the gid-only fallback (EIO, ENOENT, EROFS, …)
// indicates a real filesystem problem and must propagate — otherwise
// writeAtomic would rename a temp file whose ownership is wrong in a
// way we never diagnosed.
//
// We cover this at the primary-chown level by pointing applyOwnership
// at a non-existent path: the first os.Chown returns ENOENT, not
// EPERM, so the function must NOT enter the EPERM-fallback branch —
// it must propagate the ENOENT directly. A regression that widened
// the EPERM mask to "any err" would silently turn this into a nil
// return and fail the test.
//
// The gid-only fallback's non-EPERM branch is hard to trigger
// deterministically (the only way to reach it is an EPERM from the
// combined chown followed by a non-EPERM from the gid-only chown on
// the same path, which would require racing a concurrent unlink). The
// code path mirrors the primary chown's errors.Is(err, syscall.EPERM)
// gate exactly, so the primary-chown coverage plus direct code review
// pins the fallback's behavior.
func TestApplyOwnershipPropagatesNonEPERMErrors(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")
	// No WriteFile — path doesn't exist. The primary os.Chown will
	// return ENOENT (wrapped by os).
	err := applyOwnership(missing, os.Geteuid(), os.Getegid())
	if err == nil {
		t.Fatal("expected ENOENT from chown on missing path, got nil")
	}
	if errors.Is(err, syscall.EPERM) {
		t.Fatalf("expected non-EPERM error, got EPERM: %v", err)
	}
	if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ENOENT, got %v", err)
	}
}
