package tunnel

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// ErrAmbiguousRunningTunnelProcess reports that `findRunningTunnelProcessWith`
// saw more than one live ssh process matching the tunnel config. Callers
// that hold a recorded PID should prefer that PID; callers without one
// should surface the ambiguity to the operator rather than silently
// skipping cleanup (which would leave the orphans running).
var ErrAmbiguousRunningTunnelProcess = errors.New("multiple running tunnel processes match tunnel config")
var errTunnelProcessNotRunning = errors.New("tunnel process not running")
var errTunnelProcessLookupIndeterminate = errors.New("tunnel process lookup indeterminate")

type processInfo struct {
	pid     int
	cmdline string
}

// CleanupStaleTunnelProcess terminates a leftover tunnel process after
// verifying the pid still matches the recorded tunnel configuration.
func CleanupStaleTunnelProcess(pid int, cfg TunnelConfig) error {
	return cleanupStaleTunnelProcess(pid, cfg)
}

// FindRunningTunnelProcess locates a running ssh process that matches cfg.
func FindRunningTunnelProcess(cfg TunnelConfig) (int, bool, error) {
	return findRunningTunnelProcessWith(listRunningTunnelProcesses, cfg)
}

func findRunningTunnelProcessWith(listFn func() ([]processInfo, error), cfg TunnelConfig) (int, bool, error) {
	if listFn == nil {
		return 0, false, nil
	}
	procs, err := listFn()
	if err != nil {
		return 0, false, err
	}
	matchPID := 0
	for _, proc := range procs {
		// Adoption is restricted to *managed* tunnels (ssh invocations that
		// carry cc-clip's `-F <cc-clip-ssh-config-*>` anchor plus the full
		// set of managed `-o` options). This is identical to the predicate
		// cleanup uses, so an adopted PID can always be validated the same
		// way on stop.
		if proc.pid <= 0 || !isCCClipManagedCmdline(proc.cmdline, cfg) {
			continue
		}
		if matchPID != 0 && matchPID != proc.pid {
			return 0, false, fmt.Errorf("%w for %s on local port %d", ErrAmbiguousRunningTunnelProcess, cfg.Host, cfg.LocalPort)
		}
		matchPID = proc.pid
	}
	if matchPID == 0 {
		return 0, false, nil
	}
	return matchPID, true, nil
}

// isCCClipManagedCmdline reports whether cmdline is the exact shape that
// sshTunnelArgs emits. matchesTunnelProcess delegates to this predicate;
// the named helper is kept for call sites that want to make the strict
// contract visible in code.
func isCCClipManagedCmdline(cmdline string, cfg TunnelConfig) bool {
	return matchesTunnelProcess(cmdline, cfg)
}

func tunnelProcessExitedOrChangedWith(lookupFn func(int) (string, error), pid int, cfg TunnelConfig) (bool, error) {
	cmdline, err := lookupFn(pid)
	if err != nil {
		if errors.Is(err, errTunnelProcessNotRunning) {
			return true, nil
		}
		return false, err
	}
	return !matchesTunnelProcess(cmdline, cfg), nil
}

func retryTunnelProcessLookup(lookupFn func(int) (string, error), pid int, attempts int, delay time.Duration) (string, error) {
	if lookupFn == nil {
		return "", nil
	}
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		cmdline, err := lookupFn(pid)
		if err == nil {
			return cmdline, nil
		}
		if !errors.Is(err, errTunnelProcessLookupIndeterminate) {
			return "", err
		}
		lastErr = err
		if attempt+1 >= attempts {
			break
		}
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	return "", lastErr
}

func normalizedProcessToken(token string) string {
	token = strings.TrimSpace(token)
	token = strings.Trim(token, `"'`)
	token = strings.ReplaceAll(token, `\`, "/")
	return strings.ToLower(path.Base(token))
}
