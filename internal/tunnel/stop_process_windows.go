//go:build windows

package tunnel

import (
	"os/exec"
	"strconv"
	"strings"
)

func stopTunnelProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if !pidStillMatchesTunnelCommand(cmd.Process.Pid, cmd) {
		// PID was recycled after cmd.Wait() returned, or the process is
		// already gone — taskkill blindly on a recycled PID tree would
		// terminate an unrelated process.
		return nil
	}
	// runTaskkill applies the bounded ctx timeout. Without it, a wedged
	// taskkill (documented Win11 24H2 failure mode where an unsignalable
	// target hangs the syscall) would pin the manager's opMu indefinitely.
	return runTaskkill(strconv.Itoa(cmd.Process.Pid), false)
}

// forceKillTunnelCommand escalates stopTunnelProcess to /F so the whole
// tree including ProxyCommand / jump helpers is terminated. Uses the same
// bounded-timeout wrapper as stopTunnelProcess so Down/Shutdown cannot be
// held up by a taskkill that refuses to return.
func forceKillTunnelCommand(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if !pidStillMatchesTunnelCommand(cmd.Process.Pid, cmd) {
		return nil
	}
	return runTaskkill(strconv.Itoa(cmd.Process.Pid), true)
}

// pidStillMatchesTunnelCommand confirms that pid on the live system still
// belongs to the ssh tunnel we spawned, by comparing the process's current
// command line against the unique `-R <spec>` and target host tokens from
// cmd.Args. Returns false if the process has exited, the PID has been
// recycled, or the cmdline can't be read. This intentionally avoids
// exec.Cmd.ProcessState: cmd.Wait() writes that field from another goroutine,
// and reading it here races under `go test -race`.
func pidStillMatchesTunnelCommand(pid int, cmd *exec.Cmd) bool {
	forward, host, ok := tunnelCommandSignature(cmd)
	if !ok {
		return false
	}
	live, err := tunnelProcessCommandLine(pid)
	if err != nil {
		return false
	}
	if !liveCommandLineContainsToken(live, forward) {
		return false
	}
	return liveCommandLineContainsToken(live, host)
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
