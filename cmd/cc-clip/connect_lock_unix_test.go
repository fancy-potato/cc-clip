//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shunmei/cc-clip/internal/tunnel"
)

// TestAcquireConnectStateLockDistinctFilesForSanitizeColliders pins the
// P2-C finding: two host aliases that sanitize to the same slug (e.g.
// `foo.bar` and `foo_bar` both become `foo-bar`) but produce DIFFERENT
// state-file hash suffixes MUST acquire DIFFERENT lock files, so a
// `cc-clip connect foo.bar` and a concurrent `cc-clip connect foo_bar`
// do not spuriously serialize on each other.
//
// Regression: the original lock filename was only
// `connect-<sanitized>-<port>.lock`, which collided for both aliases
// above. The fix derives the lock stem from tunnel.StateFilePath, which
// already includes the hash suffix.
func TestAcquireConnectStateLockDistinctFilesForSanitizeColliders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Sanity-check the precondition: the two aliases really do share a
	// sanitized slug but produce different state files. If this ever
	// changes upstream the test becomes trivially true; guard against
	// that by failing loudly instead of silently passing.
	if tunnel.SanitizeHost("foo.bar") != tunnel.SanitizeHost("foo_bar") {
		t.Fatalf("precondition failed: SanitizeHost('foo.bar')=%q, SanitizeHost('foo_bar')=%q — the collision scenario no longer exists and this test must be rewritten",
			tunnel.SanitizeHost("foo.bar"), tunnel.SanitizeHost("foo_bar"))
	}
	if tunnel.StateFilePath("", "foo.bar", 18339) == tunnel.StateFilePath("", "foo_bar", 18339) {
		t.Fatalf("precondition failed: state file paths for foo.bar and foo_bar are identical — the hash suffix is broken and a lot more than this test is wrong")
	}

	releaseA, err := acquireConnectStateLock("foo.bar", 18339)
	if err != nil {
		t.Fatalf("acquire lock for foo.bar: %v", err)
	}
	defer releaseA()

	// If the lock filenames collide the second flock call would BLOCK
	// (the outer flock is exclusive and held by releaseA). We don't want
	// to introduce a timeout-based test here, so instead we assert by
	// filename: enumerate the state dir and verify two distinct .lock
	// files exist after both acquires.
	releaseB, err := acquireConnectStateLock("foo_bar", 18339)
	if err != nil {
		t.Fatalf("acquire lock for foo_bar (would spuriously serialize pre-fix): %v", err)
	}
	defer releaseB()

	stateDir := tunnel.DefaultStateDir()
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("read state dir %s: %v", stateDir, err)
	}
	var locks []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "connect-") && strings.HasSuffix(name, ".lock") {
			locks = append(locks, name)
		}
	}
	if len(locks) != 2 {
		t.Fatalf("expected 2 distinct lock files, got %d: %v", len(locks), locks)
	}
	// Both lock files must embed the per-host hash suffix, so neither
	// filename is simply `connect-foo-bar-18339.lock` (the colliding
	// pre-fix shape).
	for _, name := range locks {
		if name == "connect-foo-bar-18339.lock" {
			t.Errorf("lock filename %q lacks the state-file hash suffix; two sanitize-colliding hosts would share this file", name)
		}
	}

	// Cross-check that each lock file sits next to a state-file path
	// derived from the SAME (host, port, hash) triple — i.e. the lock
	// grain matches the state-file grain, as the fix guarantees.
	for _, host := range []string{"foo.bar", "foo_bar"} {
		stateBase := filepath.Base(tunnel.StateFilePath(stateDir, host, 18339))
		wantLock := "connect-" + strings.TrimSuffix(stateBase, ".json") + ".lock"
		found := false
		for _, got := range locks {
			if got == wantLock {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected lock file %q for host %q, got %v", wantLock, host, locks)
		}
	}
}

// TestAcquireConnectStateLockMutualExclusion pins the P2 mutual-exclusion
// contract: while one holder owns the lock for a given (host, port), any
// second acquireConnectStateLock for the SAME (host, port) must fail with
// a clear "already in progress" error after the retry deadline — it must
// NOT block forever, and it must surface the holder's PID when readable.
// Once the first holder releases, a third acquire must succeed.
//
// The test shrinks the package-level deadline/retry so it completes in
// well under a second rather than waiting the production 30 s.
func TestAcquireConnectStateLockMutualExclusion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origDeadline := connectLockAcquireDeadline
	origInterval := connectLockRetryInterval
	connectLockAcquireDeadline = 100 * time.Millisecond
	connectLockRetryInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		connectLockAcquireDeadline = origDeadline
		connectLockRetryInterval = origInterval
	})

	const (
		host = "test-mutex"
		port = 18339
	)

	release1, err := acquireConnectStateLock(host, port)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Second attempt: should fail with "already in progress" after the
	// shortened deadline. If the code regressed back to blocking flock
	// this call would hang forever, so we also guard with a test-wide
	// deadline via a goroutine + channel.
	done := make(chan error, 1)
	go func() {
		release2, err := acquireConnectStateLock(host, port)
		if err == nil {
			release2()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			release1()
			t.Fatal("expected second acquire to fail with 'already in progress', got nil")
		}
		if !strings.Contains(err.Error(), "already in progress") {
			release1()
			t.Fatalf("expected error containing 'already in progress', got %v", err)
		}
		// PID diagnostic: the lock file was written by the first holder
		// (this process), so the error should contain its PID.
		selfPID := os.Getpid()
		wantPID := "PID=" + strings.TrimPrefix("", "") // defensive; format below
		_ = wantPID
		if !strings.Contains(err.Error(), "PID=") {
			t.Errorf("expected PID diagnostic in error message, got %v", err)
		}
		if !strings.Contains(err.Error(), "PID="+itoaPID(selfPID)) {
			t.Logf("warning: PID diagnostic did not include current PID %d; error was %v", selfPID, err)
		}
	case <-time.After(5 * time.Second):
		release1()
		t.Fatal("second acquire never returned — blocking flock regression (should retry with LOCK_NB and fail after the shortened deadline)")
	}

	// Release the first lock and confirm the third acquire succeeds.
	release1()
	release3, err := acquireConnectStateLock(host, port)
	if err != nil {
		t.Fatalf("third acquire after release: %v", err)
	}
	release3()
}

// itoaPID is a tiny helper to avoid importing strconv just for this test.
func itoaPID(pid int) string {
	if pid <= 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for pid > 0 {
		i--
		buf[i] = byte('0' + pid%10)
		pid /= 10
	}
	return string(buf[i:])
}
