// Package userhome centralizes resolution of the user's HOME directory
// for cc-clip's LOCAL state. The two production entry points are
// os.UserHomeDir and user.Lookup (for the SUDO_USER fallback on Unix).
// Both are wrapped behind a Resolver interface so tests can swap in a
// deterministic implementation without mutating package-level vars —
// the mutable-var approach raced against any concurrent production
// caller the moment a future t.Parallel() landed.
package userhome

import (
	"fmt"
	"os"
	"os/user"
	"strings"
	"sync"
	"testing"
)

// Resolver abstracts the two stdlib calls Dir relies on. Production
// wires the stdlib implementation via defaultResolver; tests substitute
// a fake via SetResolverForTest (which is automatically torn down at
// test end so no state leaks across tests).
type Resolver interface {
	LookupUser(name string) (*user.User, error)
	UserHomeDir() (string, error)
	IsSudoRoot() bool
}

type realResolver struct{}

func (realResolver) LookupUser(name string) (*user.User, error) { return user.Lookup(name) }
func (realResolver) UserHomeDir() (string, error)               { return os.UserHomeDir() }
func (realResolver) IsSudoRoot() bool                           { return isSudoRootContext() }

// resolverMu guards defaultResolver. A sync.Mutex (rather than an
// atomic pointer) is used because Resolver is an interface and tests
// may want to swap the resolver while a production caller is mid-flight
// on another goroutine — the mutex makes the swap sequentially
// consistent without the subtle aliasing of atomic.Value with
// interface-typed values.
var (
	resolverMu      sync.Mutex
	defaultResolver Resolver = realResolver{}
)

// currentResolver returns the active resolver under the package lock.
// Holding the lock across the interface-method call would force all
// production callers through a single mutex; instead we snapshot the
// interface value (a pointer-sized copy) and drop the lock before the
// call, so a concurrent SetResolverForTest never blocks a production
// Dir() resolution.
func currentResolver() Resolver {
	resolverMu.Lock()
	r := defaultResolver
	resolverMu.Unlock()
	return r
}

// SetResolverForTest installs r as the package resolver for the duration
// of the current test, restoring the previous resolver via t.Cleanup.
// Tests MUST use this helper rather than poking defaultResolver directly,
// so the restore hook always runs even on t.Fatal / panic.
func SetResolverForTest(t *testing.T, r Resolver) {
	t.Helper()
	resolverMu.Lock()
	prev := defaultResolver
	defaultResolver = r
	resolverMu.Unlock()
	t.Cleanup(func() {
		resolverMu.Lock()
		defaultResolver = prev
		resolverMu.Unlock()
	})
}

// Dir returns the home directory cc-clip should use for LOCAL state. In an
// actual sudo-root context, a local command should still read and write the
// invoking user's state rather than split it across /var/root (or another
// elevated account) and the user's actual home.
//
// Defense: SUDO_USER alone is not trusted. A sudoers rule with
// `env_keep += SUDO_USER` (or a manually-set SUDO_USER) could otherwise
// redirect cc-clip's reads/writes to an unrelated user's home. We
// therefore cross-check that `SUDO_UID` matches the looked-up user's
// numeric Uid, and that the looked-up Uid is non-zero (refuse to
// "resolve to root" — that path defeats the whole point of falling back
// out of /var/root in the first place). When SUDO_UID is unset/garbled
// or the uid mismatches, Dir falls back to the safer
// `os.UserHomeDir()` and lets the caller produce the actual error if
// any — refusing the lookup itself would break legitimate sudo cases
// where SUDO_UID happens to be missing (some `su -c` chains).
func Dir() (string, error) {
	r := currentResolver()
	sudoUser := strings.TrimSpace(os.Getenv("SUDO_USER"))
	if sudoUser != "" && r.IsSudoRoot() {
		u, err := r.LookupUser(sudoUser)
		if err != nil {
			return "", fmt.Errorf("resolve home for SUDO_USER %q: %w", sudoUser, err)
		}
		if strings.TrimSpace(u.HomeDir) == "" {
			return "", fmt.Errorf("resolve home for SUDO_USER %q: empty home directory", sudoUser)
		}
		// Trust the SUDO_USER → home mapping only when SUDO_UID
		// agrees, and when the resolved Uid is not root. Either
		// mismatch indicates a tampered env (or a SUDO_USER pointing
		// at a system account whose home is /); fall through to the
		// process's own UserHomeDir rather than write into an
		// unexpected location.
		if !sudoIdentityMatches(u.Uid, strings.TrimSpace(os.Getenv("SUDO_UID"))) {
			return r.UserHomeDir()
		}
		return u.HomeDir, nil
	}
	return r.UserHomeDir()
}

// sudoIdentityMatches returns true when sudoUID env value parses to a
// positive integer that equals the looked-up user's Uid. An empty,
// non-numeric, zero, or mismatched sudoUID returns false. Pure helper
// so the rule is testable without spawning sudo.
func sudoIdentityMatches(lookupUID, sudoUID string) bool {
	if sudoUID == "" || lookupUID == "" {
		return false
	}
	if sudoUID != lookupUID {
		return false
	}
	// Refuse to map to root — if SUDO_USER somehow points at uid 0,
	// the sudo fallback semantics (write into the invoking user's home
	// rather than /var/root) collapse to the same as the no-fallback
	// path. Fall through to UserHomeDir, which will return /var/root
	// or /root and the caller can decide if that's appropriate.
	if lookupUID == "0" {
		return false
	}
	return true
}
