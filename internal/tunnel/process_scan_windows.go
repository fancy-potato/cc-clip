//go:build windows

package tunnel

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/shunmei/cc-clip/internal/win32"
)

// powershellScanTimeout bounds the CIM enumeration so a wedged WMI / CIM
// probe cannot pin opMu inside LoadAndStartAll for the life of the daemon.
// Mirrors psCommandTimeout on unix.
const powershellScanTimeout = 10 * time.Second

func listRunningTunnelProcesses() ([]processInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), powershellScanTimeout)
	defer cancel()
	// CommandLine is base64-encoded so embedded CR/LF or our own separator
	// token cannot break the row format. parsePowershellTunnelProcessOutput
	// decodes it after splitting on the sentinel.
	psCmd := `Get-CimInstance Win32_Process | Where-Object { $_.CommandLine } | ForEach-Object { "{0}` + powershellProcessSeparator + `{1}" -f $_.ProcessId, [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($_.CommandLine)) }`
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", psCmd)
	win32.HideConsoleWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("enumerate processes: timed out after %v: %w", powershellScanTimeout, ctx.Err())
		}
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return nil, fmt.Errorf("enumerate processes: %w (%s)", err, msg)
		}
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	return parsePowershellTunnelProcessOutput(out), nil
}
