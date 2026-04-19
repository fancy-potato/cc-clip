//go:build windows

package sshconfig

// acquireConfigLock is a no-op on Windows.
//
// Contract — read before changing this file:
//
//   - The atomic rename in writeAtomic still prevents TORN writes between
//     two concurrent ApplyToFile calls. ~/.ssh/config will never be a
//     truncated or partially-written file as a result of concurrent cc-clip
//     processes on Windows.
//   - But two concurrent Applies that both read the same baseline can each
//     produce a fresh snapshot and race the rename. Whichever rename loses
//     the race is silently DROPPED (lost-update). The losing caller still
//     succeeds without error; the winning caller's marker block is the one
//     that ends up on disk.
//
// This degrade is accepted because:
//
//   - The shared-account multi-laptop concurrency contract that the SetEnv
//     marker exists for (see internal/sshconfig/Apply doc) is fundamentally
//     a Unix-server scenario. Windows hosts are clipboard-source clients.
//   - Cross-process advisory locking on Windows requires LockFileEx via
//     golang.org/x/sys/windows; pulling that dependency in to defend a
//     scenario the product doesn't target was deferred.
//   - The Unix path uses real flock(LOCK_EX) so the primary deployment
//     target is unaffected.
//
// Future contributors: do NOT delete this file or treat the no-op as a
// stub-to-be-filled. If a real LockFileEx implementation is added, keep
// the function signature compatible and update lock_unix.go's contract
// comment to match. Tests that simulate concurrent Apply on Windows must
// document the lost-update window or skip on runtime.GOOS=="windows".
func acquireConfigLock(path string) (release func(), err error) {
	return func() {}, nil
}
