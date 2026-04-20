//go:build windows

package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/shunmei/cc-clip/internal/tunnel"
)

// connectLockAcquireDeadline / connectLockRetryInterval mirror the Unix
// constants in connect_lock_unix.go so the platform behavior is uniform.
// 30s comfortably covers a normal connect (~10–20s of ssh handshake +
// deploy) while still giving the user a prompt error if the existing
// holder is stuck. Both are package-level vars so tests can shrink them.
var (
	connectLockAcquireDeadline = 30 * time.Second
	connectLockRetryInterval   = 250 * time.Millisecond
)

// acquireConnectStateLock takes an exclusive Windows file lock via
// LockFileEx. The previous implementation was a no-op that returned
// (release, nil), so the caller could not distinguish "lock acquired"
// from "lock unimplemented" — concurrent `cc-clip connect` runs on
// Windows would silently race the tunnel-state file.
//
// Lock granularity matches the Unix path: derived from the
// state-file path so two SSH aliases that sanitize to the same slug
// disambiguate via the hash suffix and don't spuriously serialize.
//
// On timeout the diagnostic mirrors Unix: the lock file's PID line is
// surfaced so operators can `taskkill /pid …` a wedged holder. Note
// that Windows recycles PIDs aggressively; the reported PID is a hint,
// not a contract.
func acquireConnectStateLock(host string, localPort int) (release func(), err error) {
	dir := tunnel.DefaultStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return func() {}, err
	}
	stateBase := filepath.Base(tunnel.StateFilePath(dir, host, localPort))
	stem := strings.TrimSuffix(stateBase, ".json")
	lockName := fmt.Sprintf("connect-%s.lock", stem)
	lockPath := filepath.Join(dir, lockName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, err
	}
	h := windows.Handle(f.Fd())
	deadline := time.Now().Add(connectLockAcquireDeadline)
	for {
		var ol windows.Overlapped
		flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
		lockErr := windows.LockFileEx(h, flags, 0, math.MaxUint32, math.MaxUint32, &ol)
		if lockErr == nil {
			break
		}
		// ERROR_LOCK_VIOLATION = "another process has locked a portion of
		// the file" — equivalent to EWOULDBLOCK on flock. Retry until
		// deadline. Any other error is fatal.
		if lockErr != windows.ERROR_LOCK_VIOLATION && lockErr != windows.ERROR_IO_PENDING {
			f.Close()
			return func() {}, lockErr
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
	// Record the owning PID so `type` on the lock file diagnoses stale
	// locks. Truncate first so a shorter PID doesn't leave trailing
	// bytes from a prior holder. Best-effort writes — the lock itself
	// is already held; PID is just a hint.
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	return func() {
		var ol windows.Overlapped
		windows.UnlockFileEx(h, 0, math.MaxUint32, math.MaxUint32, &ol)
		f.Close()
	}, nil
}

// readLockHolderPID reads the PID line written into the lock file by the
// current holder. Best-effort: empty file or malformed integer returns 0.
// Rejects pid <= 1 (pid 0 is never valid; pid 4 is System on Windows
// but pid 1 is not used — nonetheless <=1 is the cross-platform guard
// for the "corrupt lock contains `1`" diagnostic case).
func readLockHolderPID(f *os.File) int {
	if _, err := f.Seek(0, 0); err != nil {
		return 0
	}
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
