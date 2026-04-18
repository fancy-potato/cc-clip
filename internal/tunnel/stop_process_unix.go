//go:build !windows

package tunnel

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// isTunnelSessionLeader reports whether cmd was spawned with Setsid so it
// sits in its own session/process group. Group signalling (kill(-pid, …))
// is only safe when this holds; otherwise kill(-pid, …) would signal the
// daemon's own process group and tear down cc-clip itself.
func isTunnelSessionLeader(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.SysProcAttr == nil {
		return false
	}
	return cmd.SysProcAttr.Setsid
}

// pidStillMatchesTunnelCmd checks whether pid still belongs to the exact ssh
// tunnel we spawned. If the PID has been recycled — e.g. the tunnel already
// exited and the kernel reused the pid for an unrelated process — we must
// not signal it. The check deliberately relies on live process inspection
// instead of exec.Cmd.ProcessState: cmd.Wait updates ProcessState from a
// separate goroutine, and reading it here races under `go test -race`.
// Uses the same signature-based match as Windows: extract the `-R <spec>`
// and trailing host from cmd.Args and require *both* tokens to appear in the
// live cmdline. A bare "is it ssh -N -R"? check would be far too permissive
// and would happily kill an unrelated `ssh -N -R` owned by the same user
// after PID recycling.
func pidStillMatchesTunnelCmd(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	forward, host, ok := tunnelCommandSignature(cmd)
	if !ok {
		return false
	}
	pid := cmd.Process.Pid
	cmdline, err := tunnelProcessCommandLine(pid)
	if err != nil {
		if errors.Is(err, errTunnelProcessNotRunning) {
			return false
		}
		// Inspect failure is ambiguous. Log and skip rather than risk
		// signalling a recycled pid that may own an unrelated process.
		log.Printf("tunnel-manager: pid %d inspect failed before signal: %v", pid, err)
		return false
	}
	return liveCommandLineContainsToken(cmdline, forward) &&
		liveCommandLineContainsToken(cmdline, host)
}

// tunnelCommandSignature extracts the `-R <spec>` value and the trailing host
// argument from cmd.Args. These two tokens uniquely identify a tunnel's ssh
// command line; both must appear in the live process cmdline.
func tunnelCommandSignature(cmd *exec.Cmd) (string, string, bool) {
	if cmd == nil || len(cmd.Args) == 0 {
		return "", "", false
	}
	args := cmd.Args
	forward := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-R" {
			forward = args[i+1]
			break
		}
	}
	host := strings.TrimSpace(args[len(args)-1])
	if forward == "" || host == "" {
		return "", "", false
	}
	return forward, host, true
}

// liveCommandLineContainsToken reports whether `token` appears as a complete
// whitespace/argument boundary in `live`. Using strings.Contains would let a
// short SSH alias like `p` match an unrelated cmdline that happens to contain
// the letter "p"; tokenizing avoids the false positive.
func liveCommandLineContainsToken(live, token string) bool {
	if token == "" {
		return false
	}
	for _, field := range splitCommandLineTokens(live) {
		if strings.Trim(field, `"'`) == token {
			return true
		}
	}
	return false
}

func stopTunnelProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if !pidStillMatchesTunnelCmd(cmd) {
		log.Printf("tunnel-manager: skipping SIGTERM to pid %d (no longer a cc-clip ssh tunnel)", cmd.Process.Pid)
		return nil
	}
	if isTunnelSessionLeader(cmd) {
		return normalizeProcessGoneError(signalTunnelProcessGroup(cmd.Process.Pid, syscall.SIGTERM))
	}
	return normalizeProcessGoneError(signalTunnelProcessLeader(cmd.Process.Pid, syscall.SIGTERM))
}

// forceKillTunnelCommand SIGKILLs the entire process group rather than only
// the leader, matching stopTunnelProcess' group-wide SIGTERM so ProxyCommand
// / ssh-agent helper children do not outlive the tunnel. Falls back to a
// leader-only kill when the child was not started as a session leader, so a
// regression that removes Setsid never torpedoes the daemon's own group.
func forceKillTunnelCommand(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if !pidStillMatchesTunnelCmd(cmd) {
		log.Printf("tunnel-manager: skipping SIGKILL to pid %d (no longer a cc-clip ssh tunnel)", cmd.Process.Pid)
		return nil
	}
	if isTunnelSessionLeader(cmd) {
		return normalizeProcessGoneError(signalTunnelProcessGroup(cmd.Process.Pid, syscall.SIGKILL))
	}
	return normalizeProcessGoneError(signalTunnelProcessLeader(cmd.Process.Pid, syscall.SIGKILL))
}

// normalizeProcessGoneError maps ESRCH (the kernel's "no such process" code
// for kill(2)) to os.ErrProcessDone so callers can distinguish "process is
// already gone" from a real signalling failure with errors.Is.
func normalizeProcessGoneError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
