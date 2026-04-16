package doctor

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/shunmei/cc-clip/internal/peer"
	"github.com/shunmei/cc-clip/internal/shellutil"
	"github.com/shunmei/cc-clip/internal/shim"
	"github.com/shunmei/cc-clip/internal/token"
)

var sshConfigQuery = func(candidate string) (string, error) {
	cmd := exec.Command("ssh", "-G", candidate)
	hideConsoleWindow(cmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func RunRemote(host string, port int) []CheckResult {
	var results []CheckResult
	ident, identErr := peer.LoadOrCreateLocalIdentity()
	var reg *peer.Registration
	var (
		deployState *shim.DeployState
		deployErr   error
	)

	// Check SSH connectivity
	out, err := remoteExecNoForward(host, "echo ok")
	if err != nil {
		results = append(results, CheckResult{"ssh", false, fmt.Sprintf("cannot connect to %s: %v", host, err)})
		return results
	}
	if strings.TrimSpace(out) != "ok" {
		results = append(results, CheckResult{"ssh", false, fmt.Sprintf("unexpected output: %s", out)})
		return results
	}
	results = append(results, CheckResult{"ssh", true, fmt.Sprintf("connected to %s", host)})

	// Check remote binary
	out, err = remoteExecNoForward(host, "~/.local/bin/cc-clip version")
	if err != nil {
		results = append(results, CheckResult{"remote-bin", false, "cc-clip not found at ~/.local/bin/cc-clip"})
	} else {
		results = append(results, CheckResult{"remote-bin", true, strings.TrimSpace(out)})
	}
	deployState, deployErr = readDeployState(host)

	if identErr != nil {
		results = append(results, CheckResult{"peer", false, fmt.Sprintf("cannot load local peer identity: %v", identErr)})
	} else {
		reg, err = lookupPeer(host, ident.ID)
		results = append(results, peerLookupCheckResult(reg, err))
	}

	// Check shim installation — detect which target (xclip or wl-paste)
	shimTarget := ""
	for _, target := range []string{"xclip", "wl-paste"} {
		out, err = remoteExecNoForward(host, fmt.Sprintf("head -2 ~/.local/bin/%s 2>/dev/null || echo 'not found'", target))
		if err == nil && strings.Contains(out, "cc-clip") {
			shimTarget = target
			break
		}
	}
	if shimTarget != "" {
		results = append(results, CheckResult{"shim", true, fmt.Sprintf("%s shim installed", shimTarget)})
	} else {
		results = append(results, CheckResult{"shim", false, "no cc-clip shim found (checked xclip and wl-paste)"})
	}

	// Check PATH priority for the detected shim target
	checkTarget := "xclip"
	if shimTarget != "" {
		checkTarget = shimTarget
	}
	out, err = resolveInInteractiveShell(host, checkTarget)
	if err == nil && strings.Contains(out, ".local/bin") {
		results = append(results, CheckResult{"path-order", true, fmt.Sprintf("%s resolves to %s", checkTarget, strings.TrimSpace(out))})
	} else {
		results = append(results, CheckResult{"path-order", false, fmt.Sprintf("%s resolves to %s (shim not first)", checkTarget, strings.TrimSpace(out))})
	}

	results = append(results, checkAliasPort(host, reg, port)...)

	// Check tunnel from remote side
	remotePort := port
	if reg != nil && reg.ReservedPort != 0 {
		remotePort = reg.ReservedPort
	}
	out, err = remoteExecNoForward(host, fmt.Sprintf(
		"bash -c 'echo >/dev/tcp/127.0.0.1/%d' 2>&1 && echo 'tunnel ok' || echo 'tunnel fail'", remotePort))
	if strings.Contains(out, "tunnel ok") {
		results = append(results, CheckResult{"tunnel", true, fmt.Sprintf("port %d forwarded", remotePort)})
	} else {
		results = append(results, CheckResult{"tunnel", false, fmt.Sprintf("port %d not reachable from remote", remotePort)})
	}

	// Check token on remote
	stateDir := "~/.cache/cc-clip"
	if reg != nil && strings.TrimSpace(reg.StateDir) != "" {
		stateDir = reg.StateDir
	}
	stateDirExpr := remotePathExpr(stateDir)
	out, err = remoteExecNoForward(host, fmt.Sprintf("test -f %s/session.token && echo 'present' || echo 'missing'", stateDirExpr))
	if strings.Contains(out, "present") {
		results = append(results, CheckResult{"remote-token", true, "token file present"})
	} else {
		results = append(results, CheckResult{"remote-token", false, "token file missing"})
	}

	out, err = remoteExecNoForward(host, fmt.Sprintf("test -f %s/notify.nonce && echo 'present' || echo 'missing'", stateDirExpr))
	results = append(results, remoteNonceResult(deployState, strings.Contains(out, "present")))

	// Check remote token matches local token
	results = append(results, checkTokenMatch(host, stateDir)...)

	// Check deploy state file
	results = append(results, checkDeployStateResult(deployState, deployErr)...)

	// Check PATH fix (rc file marker)
	results = append(results, checkPathFix(host)...)

	// End-to-end image round-trip (only if tunnel is up)
	if tunnelOK(results) {
		results = append(results, runImageProbe(host, remotePort, stateDir)...)
	}

	return results
}

// remoteExecNoForward runs an SSH command without applying RemoteForward from ssh config.
// Doctor checks should inspect the existing tunnel, not compete with it by opening a new one.
func remoteExecNoForward(host string, args ...string) (string, error) {
	cmdStr := strings.Join(args, " ")
	cmd := exec.Command("ssh",
		"-o", "ClearAllForwardings=yes",
		"-o", "RemoteCommand=none",
		"-o", "RequestTTY=no",
		host, cmdStr)
	hideConsoleWindow(cmd)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func resolveInInteractiveShell(host, bin string) (string, error) {
	shellPath, _ := remoteExecNoForward(host, "echo $SHELL")
	shellName := "bash"
	if strings.Contains(shellPath, "zsh") {
		shellName = "zsh"
	}

	out, err := remoteExecNoForward(host, fmt.Sprintf(
		`%s -ic 'which %s 2>/dev/null || echo "not in PATH"'`,
		shellName,
		bin,
	))
	return strings.TrimSpace(out), err
}

func tunnelOK(results []CheckResult) bool {
	for _, r := range results {
		if r.Name == "tunnel" && r.OK {
			return true
		}
	}
	return false
}

func runImageProbe(host string, port int, stateDir string) []CheckResult {
	// Run the probe FROM the remote host through the tunnel, not from local.
	// This validates the full chain: remote -> tunnel -> daemon.
	stateDirExpr := remotePathExpr(stateDir)
	cmd := fmt.Sprintf(
		`TOKEN=$(cat %s/session.token 2>/dev/null) && `+
			`curl -sf --max-time 5 `+
			`-H "Authorization: Bearer ${TOKEN}" `+
			`-H "User-Agent: cc-clip/0.1" `+
			`"http://127.0.0.1:%d/clipboard/type"`,
		stateDirExpr, port)

	out, err := remoteExecNoForward(host, cmd)
	if err != nil {
		return []CheckResult{{"image-probe", false, fmt.Sprintf("remote probe failed: %v (%s)", err, strings.TrimSpace(out))}}
	}

	out = strings.TrimSpace(out)
	if strings.Contains(out, `"type":"image"`) {
		return []CheckResult{{"image-probe", true, "clipboard has image (verified from remote)"}}
	}
	if strings.Contains(out, `"type":`) {
		return []CheckResult{{"image-probe", true, fmt.Sprintf("remote response: %s (copy an image to test)", out)}}
	}
	return []CheckResult{{"image-probe", false, fmt.Sprintf("unexpected response: %s", out)}}
}

// checkTokenMatch verifies the remote token matches the local daemon token.
func checkTokenMatch(host string, stateDir string) []CheckResult {
	localToken, err := token.ReadTokenFile()
	if err != nil {
		return []CheckResult{{"token-match", false, "cannot read local token to compare"}}
	}

	remoteToken, err := remoteExecNoForward(host, fmt.Sprintf("cat %s/session.token 2>/dev/null", remotePathExpr(stateDir)))
	if err != nil || strings.TrimSpace(remoteToken) == "" {
		return []CheckResult{{"token-match", false, "cannot read remote token"}}
	}

	if strings.TrimSpace(remoteToken) == localToken {
		return []CheckResult{{"token-match", true, "remote token matches local"}}
	}
	return []CheckResult{{"token-match", false, "remote token differs from local (re-run 'cc-clip connect')"}}
}

// checkDeployState checks if the deploy state file exists on the remote.
func readDeployState(host string) (*shim.DeployState, error) {
	out, err := remoteExecNoForward(host, "cat ~/.cache/cc-clip/deploy.json 2>/dev/null || echo '__NOTFOUND__'")
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "__NOTFOUND__" || out == "" {
		return nil, nil
	}
	var state shim.DeployState
	if err := json.Unmarshal([]byte(out), &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func checkDeployStateResult(state *shim.DeployState, err error) []CheckResult {
	if err != nil {
		return []CheckResult{{"deploy-state", false, fmt.Sprintf("cannot read deploy.json: %v", err)}}
	}
	if state == nil {
		return []CheckResult{{"deploy-state", false, "deploy.json not found (deploy state not tracked)"}}
	}
	if state.BinaryHash != "" {
		return []CheckResult{{"deploy-state", true, "deploy.json present and valid"}}
	}
	return []CheckResult{{"deploy-state", false, "deploy.json exists but may be malformed"}}
}

func remoteNonceResult(state *shim.DeployState, noncePresent bool) CheckResult {
	if state != nil && state.Notify != nil && !state.Notify.Enabled {
		return CheckResult{"remote-nonce", true, "notifications disabled by deploy config"}
	}
	if noncePresent {
		return CheckResult{"remote-nonce", true, "notify nonce present"}
	}
	return CheckResult{"remote-nonce", false, "notify nonce missing"}
}

// checkPathFix verifies the PATH marker block exists in the remote shell rc file.
func checkPathFix(host string) []CheckResult {
	fixed, err := shim.IsPathFixed(host)
	if err != nil {
		return []CheckResult{{"path-fix", false, fmt.Sprintf("cannot check PATH marker: %v", err)}}
	}
	if fixed {
		return []CheckResult{{"path-fix", true, "PATH marker present in shell rc file"}}
	}
	return []CheckResult{{"path-fix", false, "PATH marker not found in shell rc file"}}
}

func lookupPeer(host, peerID string) (*peer.Registration, error) {
	out, err := remoteExecNoForward(host, fmt.Sprintf("~/.local/bin/cc-clip peer show --peer-id %s", shellQuote(peerID)))
	out = strings.TrimSpace(out)
	if err != nil {
		if out != "" {
			return nil, fmt.Errorf("%s", out)
		}
		return nil, err
	}
	var reg peer.Registration
	if err := json.Unmarshal([]byte(out), &reg); err != nil {
		return nil, fmt.Errorf("unexpected peer registry output: %s", out)
	}
	return &reg, nil
}

func peerLookupCheckResult(reg *peer.Registration, err error) CheckResult {
	switch {
	case err == nil && reg != nil:
		return CheckResult{"peer", true, fmt.Sprintf("%s -> port %d", reg.Label, reg.ReservedPort)}
	case err == nil:
		return CheckResult{"peer", true, "peer registry not configured on remote; using legacy state path"}
	case isLegacyPeerLookupError(err):
		return CheckResult{"peer", true, "peer registry not configured on remote; using legacy state path"}
	default:
		return CheckResult{"peer", false, fmt.Sprintf("peer registry lookup failed: %v", err)}
	}
}

func isLegacyPeerLookupError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "unknown command: peer") ||
		strings.Contains(msg, "usage: cc-clip") ||
		strings.Contains(msg, "peer show failed: peer ") && strings.Contains(msg, " not found")
}

func checkAliasPort(host string, reg *peer.Registration, localPort int) []CheckResult {
	if reg == nil || reg.ReservedPort == 0 {
		return []CheckResult{{"ssh-alias", true, "peer SSH forwarding not configured; skipping SSH config port check"}}
	}

	out, err := sshConfigQuery(host)
	if err != nil {
		return []CheckResult{{"ssh-alias", false, fmt.Sprintf("ssh -G %s failed: %v; cannot verify RemoteForward", host, err)}}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if matchesRemoteForward(line, reg.ReservedPort, "127.0.0.1", localPort) {
			return []CheckResult{{"ssh-alias", true, fmt.Sprintf("%s forwards %d 127.0.0.1:%d", host, reg.ReservedPort, localPort)}}
		}
	}
	return []CheckResult{{"ssh-alias", false, fmt.Sprintf("ssh config missing RemoteForward %d 127.0.0.1:%d", reg.ReservedPort, localPort)}}
}

func matchesRemoteForward(line string, listenPort int, targetHost string, targetPort int) bool {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 3 || fields[0] != "remoteforward" {
		return false
	}

	if fields[1] != fmt.Sprintf("%d", listenPort) {
		return false
	}

	host, port, ok := parseForwardTarget(fields[2])
	return ok && host == targetHost && port == targetPort
}

func parseForwardTarget(s string) (string, int, bool) {
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end == -1 || end+2 > len(s) || s[end+1] != ':' {
			return "", 0, false
		}
		host := s[1:end]
		port, err := strconv.Atoi(s[end+2:])
		if err != nil {
			return "", 0, false
		}
		return host, port, true
	}

	idx := strings.LastIndex(s, ":")
	if idx <= 0 || idx+1 >= len(s) {
		return "", 0, false
	}
	host := s[:idx]
	port, err := strconv.Atoi(s[idx+1:])
	if err != nil {
		return "", 0, false
	}
	return host, port, true
}

func shellQuote(s string) string {
	return shellutil.ShellQuote(s)
}

func remotePathExpr(path string) string {
	return shellutil.RemoteShellPath(path)
}
