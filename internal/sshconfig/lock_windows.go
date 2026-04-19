//go:build windows

package sshconfig

// acquireConfigLock is a no-op on Windows. The atomic rename in
// writeAtomic still prevents torn writes between two concurrent
// ApplyToFile calls, so the worst-case outcome is lost-update (one
// Apply's result wins, the other is discarded) — not a corrupt config.
// Cross-process advisory locking on Windows requires LockFileEx, which
// is not plumbed through Go stdlib cleanly; the multi-laptop concurrency
// contract is primarily a Unix concern (shared-account setups use SSH
// from a *nix laptop to a *nix server).
func acquireConfigLock(path string) (release func(), err error) {
	return func() {}, nil
}
