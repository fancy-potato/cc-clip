//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shunmei/cc-clip/internal/tunnel"
)

// connectLockAcquireDeadline bounds how long acquireConnectStateLock waits
// for a concurrently-held lock. 30 seconds comfortably covers a normal
// connect (~10-20s of ssh handshake + deploy) while still giving the user
// a prompt error if the existing holder is stuck. connectLockRetryInterval
// is the sleep between LOCK_NB retries.
//
// Both values are package-level `var`s so tests can temporarily shrink the
// deadline (via t.Cleanup(restore)) and avoid hanging for 30s when
// asserting the mutual-exclusion contract. They are NOT intended to be
// changed at runtime outside tests.
var (
	connectLockAcquireDeadline = 30 * time.Second
	connectLockRetryInterval   = 250 * time.Millisecond
)

// acquireConnectStateLock serializes concurrent `cc-clip connect
// <same-host> --port <same-port>` runs via an exclusive flock on a
// sidecar file next to the tunnel state file. It does NOT prevent
// concurrent connects against DIFFERENT hosts or different local ports
// (that is intentional — independent tunnels run in parallel).
//
// Lock granularity MUST match tunnel-state-file granularity. The state
// file name is `<sanitized-host>-<localPort>-<hash>.json` (see
// tunnel.StateFilePath); two distinct aliases can collapse to the same
// sanitized slug (e.g. `foo.bar` and `foo_bar` both sanitize to
// `foo-bar`) but get disambiguated by the hash suffix. If the lock file
// omitted the hash, those two hosts would spuriously serialize on each
// other even though they own separate state files. We therefore derive
// the lock filename directly from StateFilePath so the lock grain can
// never drift from the state-file grain.
//
// Best-effort: on error, callers fall through to a non-locked save,
// which is the pre-fix behavior (with a warning).
//
// Once the exclusive lock is acquired, the owning process's PID is
// written into the lock file so operators can diagnose a stale lock by
// inspecting
// `cat ~/.cache/cc-clip/tunnels/connect-<host>-<port>-<hash>.lock`.
func acquireConnectStateLock(host string, localPort int) (release func(), err error) {
	dir := tunnel.DefaultStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return func() {}, err
	}
	// Derive the lock stem from StateFilePath so the lock granularity
	// cannot silently diverge from the state-file granularity. We take
	// the filename and swap the `.json` suffix for `.lock`, prefixing
	// `connect-` so the sidecar is obviously not a state file.
	stateBase := filepath.Base(tunnel.StateFilePath(dir, host, localPort))
	stem := strings.TrimSuffix(stateBase, ".json")
	lockName := fmt.Sprintf("connect-%s.lock", stem)
	lockPath := filepath.Join(dir, lockName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return func() {}, err
	}
	// Non-blocking retry loop: LOCK_EX|LOCK_NB returns EWOULDBLOCK
	// immediately when another process holds the lock, so we can bound
	// total wait to connectLockAcquireDeadline and report a useful PID
	// diagnostic on timeout instead of hanging forever. A truly blocking
	// flock would hide which PID is wedged, and operators debugging
	// "cc-clip connect" stalls would have to strace the process to find
	// the holder.
	deadline := time.Now().Add(connectLockAcquireDeadline)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return func() {}, err
		}
		if time.Now().After(deadline) {
			holderPID := readLockHolderPID(f)
			f.Close()
			if holderPID > 0 {
				return func() {}, fmt.Errorf("connect already in progress (holder PID=%d); lock file %s", holderPID, lockPath)
			}
			return func() {}, fmt.Errorf("connect already in progress; lock file %s (holder PID unknown)", lockPath)
		}
		time.Sleep(connectLockRetryInterval)
	}
	// Record the owning PID so `cat` on the lock file diagnoses stale
	// locks. Truncate first so a shorter PID doesn't leave trailing
	// bytes from a prior holder. Failures here are non-fatal — the
	// lock itself is already held, PID is just diagnostic.
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// readLockHolderPID reads the PID line written into the lock file by the
// current holder. Best-effort: a short-read, empty file, or malformed
// integer returns 0 so the caller can still surface a generic "already in
// progress" error without the PID.
//
// Rejects pid <= 1 like the peer registry's readLockHolderFull does:
// pid 0 is never a real process, pid 1 is init/launchd on Unix and is
// never a cc-clip holder. Without this guard, a corrupt lock file
// containing `1\n` would produce a misleading "already in progress
// (holder PID=1)" diagnostic and send the operator chasing init.
func readLockHolderPID(f *os.File) int {
	if _, err := f.Seek(0, 0); err != nil {
		return 0
	}
	// 64 bytes is plenty for any realistic PID plus newline.
	buf := make([]byte, 64)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return 0
	}
	s := strings.TrimSpace(string(buf[:n]))
	if s == "" {
		return 0
	}
	pid, err := strconv.Atoi(s)
	if err != nil || pid <= 1 {
		return 0
	}
	return pid
}
