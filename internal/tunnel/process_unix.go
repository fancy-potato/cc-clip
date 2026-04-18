//go:build !windows

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// tunnelProcessInspectTimeout bounds the per-pid `ps -ww` subprocess so a
// wedged ps cannot starve signalAndWait's 100ms polling loop and blow past
// the 5s/2s escalation budgets.
const tunnelProcessInspectTimeout = 5 * time.Second

func tunnelSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func cleanupStaleTunnelProcess(pid int, cfg TunnelConfig) error {
	cmdline, err := tunnelProcessCommandLine(pid)
	if err != nil {
		if errors.Is(err, errTunnelProcessNotRunning) {
			return nil
		}
		return fmt.Errorf("inspect pid %d: %w", pid, err)
	}
	if !matchesTunnelProcess(cmdline, cfg) {
		return nil
	}
	if done, err := signalAndWait(pid, cfg, syscall.SIGTERM, 5*time.Second); err != nil {
		return err
	} else if done {
		return nil
	}
	// Re-validate before escalating to SIGKILL: during the 5s grace the
	// original ssh process may have exited and the kernel may have recycled
	// the PID to an unrelated process. Matches the Windows behaviour in
	// cleanupStaleTunnelProcess (process_windows.go) — without this check a
	// bare SIGKILL could land on whatever the kernel handed the recycled pid.
	cmdline, err = tunnelProcessCommandLine(pid)
	if err != nil {
		if errors.Is(err, errTunnelProcessNotRunning) {
			return nil
		}
		return fmt.Errorf("inspect pid %d before SIGKILL: %w", pid, err)
	}
	if !matchesTunnelProcess(cmdline, cfg) {
		return nil
	}
	if done, err := signalAndWait(pid, cfg, syscall.SIGKILL, 2*time.Second); err != nil {
		return err
	} else if done {
		return nil
	}
	return fmt.Errorf("pid %d still running after SIGTERM/SIGKILL", pid)
}

// signalAndWait sends sig to pid and polls until the process either exits,
// morphs into a different command (PID recycled by the kernel), or the
// deadline fires. Returns done=true when the caller should stop escalating.
func signalAndWait(pid int, cfg TunnelConfig, sig syscall.Signal, wait time.Duration) (bool, error) {
	if err := signalTunnelProcess(pid, cfg, sig); err != nil {
		return false, fmt.Errorf("signal pid %d with %v: %w", pid, sig, err)
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		cmdline, err := tunnelProcessCommandLine(pid)
		if err != nil {
			if errors.Is(err, errTunnelProcessNotRunning) {
				return true, nil
			}
			return false, fmt.Errorf("verify pid %d after %v: %w", pid, sig, err)
		}
		if !matchesTunnelProcess(cmdline, cfg) {
			return true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false, nil
}

// signalTunnelProcess sends sig to the given pid in leader-only mode. Used
// for stale cleanup where the PID was persisted to disk by a previous daemon
// and may have been recycled since: targeting the leader only ensures that if
// the PID now belongs to an unrelated process group, we do not tear down
// everything in that group.
func signalTunnelProcess(pid int, cfg TunnelConfig, sig syscall.Signal) error {
	return signalTunnelProcessWith(func(sent os.Signal) error {
		unixSig, ok := sent.(syscall.Signal)
		if !ok {
			return fmt.Errorf("unsupported signal %T", sent)
		}
		return signalTunnelProcessLeader(pid, unixSig)
	}, tunnelProcessCommandLine, pid, cfg, sig)
}

// signalTunnelProcessGroup targets the whole process group. Safe only when
// the caller holds a live *exec.Cmd for the leader (i.e. the PID cannot have
// been recycled yet because Go still holds a handle / waitpid slot).
func signalTunnelProcessGroup(pid int, sig syscall.Signal) error {
	return signalTunnelProcessGroupWith(syscall.Kill, pid, sig)
}

func signalTunnelProcessGroupWith(killFn func(int, syscall.Signal) error, pid int, sig syscall.Signal) error {
	// pid <= 1 is never a legitimate tunnel child:
	//   pid == 0 → kill(0, sig) targets our own process group (suicide).
	//   pid == 1 → kill(-1, sig) is the kernel's "broadcast to every process
	//              we can signal except init", which would SIGTERM/SIGKILL
	//              the user's entire session.
	// Either is catastrophic if a malformed state file or a recycled PID
	// ever reaches here, so refuse rather than trust the input.
	if pid <= 1 {
		return syscall.ESRCH
	}
	return killFn(-pid, sig)
}

func signalTunnelProcessLeader(pid int, sig syscall.Signal) error {
	return signalTunnelProcessLeaderWith(syscall.Kill, pid, sig)
}

func signalTunnelProcessLeaderWith(killFn func(int, syscall.Signal) error, pid int, sig syscall.Signal) error {
	// Same rationale as signalTunnelProcessGroupWith: refuse pid 0 (self)
	// and pid 1 (init) even in leader-only mode. ESRCH mirrors the kernel
	// response for "no such process" so callers' existing logic still works.
	if pid <= 1 {
		return syscall.ESRCH
	}
	return killFn(pid, sig)
}

func signalTunnelProcessWith(
	signalFn func(os.Signal) error,
	lookupFn func(int) (string, error),
	pid int,
	cfg TunnelConfig,
	sig os.Signal,
) error {
	if err := signalFn(sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		exitedOrChanged, lookupErr := tunnelProcessExitedOrChangedWith(lookupFn, pid, cfg)
		if lookupErr != nil {
			return fmt.Errorf("inspect pid %d after signal failure: %w", pid, lookupErr)
		}
		if exitedOrChanged {
			return nil
		}
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	return nil
}

func tunnelProcessCommandLine(pid int) (string, error) {
	// On Linux, prefer /proc/<pid>/cmdline directly: it is faster (no fork),
	// it doesn't go through `ps -ww` which still truncates very long command
	// lines on some distros, and it returns NUL-separated argv which we can
	// join deterministically. Fall back to `ps -ww` only when the /proc read
	// hits a transient I/O error that is not "process does not exist". A
	// permission error (EACCES on a hardened /proc, container restriction)
	// is surfaced as a log line so operators notice that the fast path is
	// broken rather than having every inspect silently fork a ps subprocess.
	if runtime.GOOS == "linux" {
		cmdline, err := readLinuxCmdline(pid)
		if err == nil {
			return cmdline, nil
		}
		if errors.Is(err, errTunnelProcessNotRunning) {
			return "", err
		}
		log.Printf("tunnel-manager: /proc/%d/cmdline unreadable, falling back to ps: %v", pid, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), tunnelProcessInspectTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", tunnelProcessCommandLineArgs(pid)...).CombinedOutput()
	cmdline := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("inspect pid %d: ps timed out after %v: %w", pid, tunnelProcessInspectTimeout, ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && cmdline == "" {
			return "", fmt.Errorf("%w: pid %d", errTunnelProcessNotRunning, pid)
		}
		if cmdline != "" {
			return "", fmt.Errorf("inspect pid %d: %w (%s)", pid, err, cmdline)
		}
		return "", fmt.Errorf("inspect pid %d: %w", pid, err)
	}
	if cmdline == "" {
		return "", fmt.Errorf("%w: pid %d", errTunnelProcessNotRunning, pid)
	}
	return cmdline, nil
}

// readLinuxCmdline reads /proc/<pid>/cmdline and returns the NUL-separated
// argv string unchanged. Downstream `splitCommandLineTokens` treats NUL as a
// token boundary, so argv entries that themselves contain whitespace (e.g.
// a tempfile path under a home directory with a space) survive tokenization
// intact. Returns errTunnelProcessNotRunning for missing /proc entries so
// callers branch identically to the `ps -ww` fallback.
func readLinuxCmdline(pid int) (string, error) {
	path := fmt.Sprintf("/proc/%d/cmdline", pid)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: pid %d", errTunnelProcessNotRunning, pid)
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	// /proc/<pid>/cmdline is NUL-separated and usually NUL-terminated. Drop
	// the trailing NUL so the final argv entry is not followed by an empty
	// token after splitting.
	if len(raw) > 0 && raw[len(raw)-1] == 0 {
		raw = raw[:len(raw)-1]
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("%w: pid %d", errTunnelProcessNotRunning, pid)
	}
	return string(raw), nil
}

func tunnelProcessCommandLineArgs(pid int) []string {
	// Request wide output so macOS/BSD ps does not truncate long ssh command lines.
	return []string{"-ww", "-o", "command=", "-p", strconv.Itoa(pid)}
}
