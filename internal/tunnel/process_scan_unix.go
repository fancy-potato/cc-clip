//go:build !windows

package tunnel

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// psCommandTimeout bounds the `ps axww` subprocess so a wedged scan
// (slow NFS /proc, frozen zombie) cannot pin opMu inside LoadAndStartAll
// for the lifetime of the daemon.
const psCommandTimeout = 10 * time.Second

func listRunningTunnelProcesses() ([]processInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "axww", "-o", "pid=,command=").Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ps timed out after %v: %w", psCommandTimeout, ctx.Err())
		}
		return nil, err
	}
	return parseRunningTunnelProcessesOutput(out), nil
}

func parseRunningTunnelProcessesOutput(out []byte) []processInfo {
	var procs []processInfo
	for _, rawLine := range bytes.Split(out, []byte{'\n'}) {
		line := strings.TrimSpace(string(rawLine))
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		cmdline := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		if cmdline == "" {
			continue
		}
		procs = append(procs, processInfo{pid: pid, cmdline: cmdline})
	}
	return procs
}
