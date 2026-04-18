//go:build windows

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shunmei/cc-clip/internal/win32"
)

// tunnelProcessInspectTimeout bounds the per-pid `Get-CimInstance` inspect
// so a wedged WMI/CIM probe cannot starve the stale-cleanup polling loop.
const tunnelProcessInspectTimeout = 10 * time.Second

// tunnelTaskkillTimeout bounds each `taskkill` invocation. On Win11 24H2
// taskkill may hang indefinitely when the target is in a non-signalable
// state; without this timeout the cleanup call holds the manager's opMu
// and blocks every subsequent Up/Down/Remove.
const tunnelTaskkillTimeout = 15 * time.Second

// tunnelCIMIndeterminateMax caps the number of CIM "indeterminate" results
// the cleanup polling loop will tolerate before giving up. An empty CIM
// response for a live process is usually transient (WMI restart, permission
// flicker), but a permanently wedged CIM would otherwise keep the loop
// calling PowerShell forever.
const tunnelCIMIndeterminateMax = 5

func tunnelSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

func cleanupStaleTunnelProcess(pid int, cfg TunnelConfig) error {
	cmdline, err := retryTunnelProcessLookup(tunnelProcessCommandLine, pid, tunnelCIMIndeterminateMax, 100*time.Millisecond)
	if err != nil {
		if errors.Is(err, errTunnelProcessNotRunning) {
			return nil
		}
		return fmt.Errorf("inspect pid %d: %w", pid, err)
	}
	if !matchesTunnelProcess(cmdline, cfg) {
		return nil
	}

	pidStr := strconv.Itoa(pid)
	if err := runTaskkill(pidStr, false); err != nil {
		exitedOrChanged, lookupErr := tunnelProcessExitedOrChangedWith(tunnelProcessCommandLine, pid, cfg)
		if lookupErr != nil {
			return fmt.Errorf("inspect pid %d after taskkill failure: %w", pid, lookupErr)
		}
		if !exitedOrChanged {
			return fmt.Errorf("taskkill pid %d: %w", pid, err)
		}
	}

	if gone, err := waitForTunnelProcessGone(pid, 5*time.Second); err != nil {
		return fmt.Errorf("verify pid %d exit after taskkill: %w", pid, err)
	} else if gone {
		return nil
	}

	// Re-validate before escalating to force-kill: during the 5s grace the
	// original ssh process may have exited and Windows may have recycled the
	// PID to an unrelated process. taskkill /F /T against a recycled PID
	// would terminate that other process tree.
	cmdline, err = retryTunnelProcessLookup(tunnelProcessCommandLine, pid, tunnelCIMIndeterminateMax, 100*time.Millisecond)
	if err != nil {
		if errors.Is(err, errTunnelProcessNotRunning) {
			return nil
		}
		return fmt.Errorf("inspect pid %d before forced taskkill: %w", pid, err)
	}
	if !matchesTunnelProcess(cmdline, cfg) {
		return nil
	}

	if err := runTaskkill(pidStr, true); err != nil {
		exitedOrChanged, lookupErr := tunnelProcessExitedOrChangedWith(tunnelProcessCommandLine, pid, cfg)
		if lookupErr != nil {
			return fmt.Errorf("inspect pid %d after forced taskkill failure: %w", pid, lookupErr)
		}
		if !exitedOrChanged {
			return fmt.Errorf("force taskkill pid %d: %w", pid, err)
		}
	}

	if gone, err := waitForTunnelProcessGone(pid, 2*time.Second); err != nil {
		return fmt.Errorf("verify pid %d exit after forced taskkill: %w", pid, err)
	} else if gone {
		return nil
	}

	cmdline, err = retryTunnelProcessLookup(tunnelProcessCommandLine, pid, tunnelCIMIndeterminateMax, 100*time.Millisecond)
	if err != nil {
		if errors.Is(err, errTunnelProcessNotRunning) {
			return nil
		}
		return fmt.Errorf("inspect pid %d after forced taskkill: %w", pid, err)
	}
	if !matchesTunnelProcess(cmdline, cfg) {
		return nil
	}
	return fmt.Errorf("pid %d still running after taskkill", pid)
}

// runTaskkill invokes `taskkill` with a bounded context timeout. Without
// the bound a hung taskkill would pin the manager's opMu and block every
// concurrent Up/Down/Remove call.
func runTaskkill(pidStr string, force bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), tunnelTaskkillTimeout)
	defer cancel()
	args := []string{"/T", "/PID", pidStr}
	if force {
		args = append([]string{"/F"}, args...)
	}
	cmd := exec.CommandContext(ctx, "taskkill", args...)
	win32.HideConsoleWindow(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("taskkill pid %s timed out after %v: %w", pidStr, tunnelTaskkillTimeout, ctx.Err())
	}
	return err
}

// waitForTunnelProcessGone polls tunnelProcessCommandLine until either the
// process is reported gone, the deadline elapses, or CIM returns too many
// consecutive indeterminate responses. The boolean indicates whether the
// process was confirmed gone; the error distinguishes a real lookup
// failure from a timed-out but still-live process.
func waitForTunnelProcessGone(pid int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	indeterminate := 0
	for time.Now().Before(deadline) {
		_, err := tunnelProcessCommandLine(pid)
		if err == nil {
			indeterminate = 0
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if errors.Is(err, errTunnelProcessNotRunning) {
			return true, nil
		}
		if errors.Is(err, errTunnelProcessLookupIndeterminate) {
			indeterminate++
			if indeterminate >= tunnelCIMIndeterminateMax {
				return false, err
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return false, err
	}
	return false, nil
}

func tunnelProcessCommandLine(pid int) (string, error) {
	// Guard against malformed PID values before formatting them into the
	// PowerShell command body. Even though current call sites only pass
	// positive ints sourced from our own state files, a corrupted state
	// file with a negative or overlarge PID would otherwise embed an
	// attacker-influenced value into a shell-like string.
	if pid <= 0 || pid >= 1<<31 {
		return "", fmt.Errorf("inspect pid %d: pid out of range", pid)
	}
	// Probe existence with Get-Process before querying CIM: an empty CIM
	// response is ambiguous — it can mean "process gone" OR "CIM transient
	// failure / permission denied". Treating empty as gone led to skipped
	// cleanup of still-running processes.
	const sentinelGone = "__CC_CLIP_PROCESS_GONE__"
	psCmd := fmt.Sprintf(
		`$p = Get-Process -Id %d -ErrorAction SilentlyContinue; `+
			`if (-not $p) { Write-Output '%s' } `+
			`else { (Get-CimInstance Win32_Process -Filter "ProcessId=%d" -ErrorAction SilentlyContinue).CommandLine }`,
		pid, sentinelGone, pid,
	)
	ctx, cancel := context.WithTimeout(context.Background(), tunnelProcessInspectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", psCmd)
	win32.HideConsoleWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("inspect pid %d: timed out after %v: %w", pid, tunnelProcessInspectTimeout, ctx.Err())
		}
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return "", fmt.Errorf("inspect pid %d: %w (%s)", pid, err, msg)
		}
		return "", fmt.Errorf("inspect pid %d: %w", pid, err)
	}
	cmdline := strings.TrimSpace(string(out))
	if cmdline == sentinelGone {
		return "", fmt.Errorf("%w: pid %d", errTunnelProcessNotRunning, pid)
	}
	if cmdline == "" {
		// Process exists per Get-Process but CIM returned nothing. Treat
		// as indeterminate rather than "gone" so the caller retries or
		// surfaces the ambiguity.
		return "", fmt.Errorf("%w: inspect pid %d: CIM returned empty output for live process", errTunnelProcessLookupIndeterminate, pid)
	}
	return cmdline, nil
}
