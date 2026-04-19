package doctor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/shunmei/cc-clip/internal/exitcode"
	"github.com/shunmei/cc-clip/internal/peer"
	"github.com/shunmei/cc-clip/internal/shellutil"
	"github.com/shunmei/cc-clip/internal/shim"
	"github.com/shunmei/cc-clip/internal/sshconfig"
	"github.com/shunmei/cc-clip/internal/token"
	"github.com/shunmei/cc-clip/internal/tunnel"
)

// remoteExecTimeout bounds each SSH doctor probe. A firewalled or unreachable
// host would otherwise block on TCP SYN for the OS default (often 60–120s) per
// check, multiplied across ~12 checks.
const remoteExecTimeout = 15 * time.Second

// sshConnectTimeout is the -o ConnectTimeout value applied to doctor SSH
// invocations. Short enough that an unreachable host fails fast, long enough
// that a high-latency link or slow auth (e.g. pinentry, U2F touch) succeeds.
const sshConnectTimeout = 5

// loadTunnelStatesForHost is a package-level indirection so tests can stub
// the saved-state lookup without touching the user's real state directory.
// Nil slots (from malformed state files that somehow slipped past
// LoadStatesForHost) are filtered so callers can assume every slice element
// is dereferenceable.
var loadTunnelStatesForHost = func(host string) ([]*tunnel.TunnelState, error) {
	states, err := tunnel.LoadStatesForHost(tunnel.DefaultStateDir(), host)
	if err != nil {
		return nil, err
	}
	out := make([]*tunnel.TunnelState, 0, len(states))
	for _, s := range states {
		if s != nil {
			out = append(out, s)
		}
	}
	return out, nil
}

// readLocalSSHConfig is a package-level indirection so tests can stub the
// user's ~/.ssh/config without touching the real file.
var readLocalSSHConfig = func() ([]byte, error) {
	path, err := sshconfig.LocalConfigPath()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func RunRemote(host string, port int) []CheckResult {
	var results []CheckResult

	// Defense-in-depth: validate host before any exec.Command("ssh", host, …)
	// downstream. The CLI is the ostensible source of truth for --host, but
	// the tunnel manager already runs this check at every boundary; keeping
	// the doctor consistent means a typo or pasted "-oProxyCommand=…" yields
	// a clean error instead of a malformed ssh invocation.
	if err := tunnel.ValidateSSHHost(host); err != nil {
		results = append(results, CheckResult{"ssh", false, fmt.Sprintf("invalid --host %q: %v", host, err)})
		return results
	}

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

	// Passive advisory: surface a leftover "# >>> cc-clip managed host: …"
	// block in the local ~/.ssh/config. cc-clip no longer reads or writes
	// that file, but interactive `ssh <host>` sessions will still try to
	// bind the legacy reverse forward and print a confusing
	// "Warning: remote port forwarding failed" line. We do NOT auto-clean
	// — that is intentionally a manual step per CLAUDE.md.
	if r := checkLegacyManagedBlock(host); r != nil {
		results = append(results, *r)
	}

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

	tunnelStateResults, savedState := checkTunnelStateAlignment(host, reg, port)
	results = append(results, tunnelStateResults...)

	// Validate the SetEnv marker block in ~/.ssh/config matches the current
	// peer registration. A stale block (user ran `cc-clip connect` on a new
	// port without a subsequent rewrite, or hand-edited ~/.ssh/config) would
	// silently mis-route the remote shims on the next interactive ssh
	// session, so this check is strict (OK=false on mismatch) even though
	// the rest of the ssh-config-related advisories stay OK=true.
	if setEnvResult := checkSetEnvAlignment(host, reg); setEnvResult != nil {
		results = append(results, *setEnvResult)
	}

	// Check tunnel from remote side
	remotePort := 0
	if reg != nil && reg.ReservedPort != 0 {
		remotePort = reg.ReservedPort
	} else if savedState != nil && savedState.Config.RemotePort != 0 {
		remotePort = savedState.Config.RemotePort
	}
	// Use a POSIX-sh probe that tries, in order: curl (most portable, common
	// on Linux/macOS), nc (BSD or GNU), bash /dev/tcp (bash-only fallback).
	// The previous bash-only /dev/tcp probe silently reported failure on
	// dash/busybox/alpine hosts even when the tunnel was healthy.
	if remotePort == 0 {
		results = append(results, CheckResult{"tunnel", false, "cannot determine remote tunnel port (missing peer registration and no matching saved tunnel state)"})
	} else {
		probeScript := fmt.Sprintf(`sh -c '
p=%d
if command -v curl >/dev/null 2>&1; then
  if curl -sf -o /dev/null --max-time 3 "http://127.0.0.1:$p/health"; then echo tunnel ok; exit 0; fi
fi
if command -v nc >/dev/null 2>&1; then
  if nc -z -w 3 127.0.0.1 "$p" 2>/dev/null; then echo tunnel ok; exit 0; fi
fi
if command -v bash >/dev/null 2>&1; then
  if bash -c "exec 3<>/dev/tcp/127.0.0.1/$p" 2>/dev/null; then echo tunnel ok; exit 0; fi
fi
echo tunnel fail
'`, remotePort)
		out, err = remoteExecNoForward(host, probeScript)
		if strings.Contains(out, "tunnel ok") {
			results = append(results, CheckResult{"tunnel", true, fmt.Sprintf("port %d forwarded", remotePort)})
		} else {
			results = append(results, CheckResult{"tunnel", false, fmt.Sprintf("port %d not reachable from remote (%s)", remotePort, strings.TrimSpace(out))})
		}
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
//
// Every invocation is bounded by remoteExecTimeout and uses a short
// ConnectTimeout so an unreachable host fails fast instead of blocking the
// whole doctor run on OS-level TCP retry defaults.
func remoteExecNoForward(host string, args ...string) (string, error) {
	cmdStr := strings.Join(args, " ")
	ctx, cancel := context.WithTimeout(context.Background(), remoteExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "ClearAllForwardings=yes",
		"-o", "RemoteCommand=none",
		"-o", "RequestTTY=no",
		"-o", fmt.Sprintf("ConnectTimeout=%d", sshConnectTimeout),
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=2",
		host, cmdStr)
	hideConsoleWindow(cmd)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return strings.TrimSpace(string(out)), fmt.Errorf("ssh to %s timed out after %s", host, remoteExecTimeout)
	}
	return strings.TrimSpace(string(out)), err
}

func resolveInInteractiveShell(host, bin string) (string, error) {
	shellPath, _ := remoteExecNoForward(host, "echo $SHELL")
	// IMPORTANT: shellPath is attacker-influenceable (it comes from the
	// remote's $SHELL env). We deliberately do NOT interpolate shellPath
	// itself into the command line — only one of the three literal
	// constants "bash" / "zsh" / "fish" is ever placed in the argv. A
	// future "optimization" that passes shellPath directly to ssh would
	// let a crafted $SHELL (e.g. `bash -c pwned`) reach the remote shell
	// verbatim. If you need to support more shells, extend the switch
	// with new literal constants; do NOT widen to raw shellPath.
	shellName := "bash"
	switch {
	case strings.Contains(shellPath, "zsh"):
		shellName = "zsh"
	case strings.Contains(shellPath, "fish"):
		shellName = "fish"
	}

	// fish does not accept `-ic` — it uses `-i -c` with a different quoting
	// convention. Use a shell-appropriate form for each.
	var script string
	switch shellName {
	case "fish":
		script = fmt.Sprintf(`fish -i -c 'which %s 2>/dev/null; or echo "not in PATH"'`, bin)
	default:
		script = fmt.Sprintf(`%s -ic 'which %s 2>/dev/null || echo "not in PATH"'`, shellName, bin)
	}
	out, err := remoteExecNoForward(host, script)
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

	// Both tokens are already on the local filesystem so this comparison is
	// not across a network boundary, but the rest of the codebase routes
	// every token comparison through token.ConstantTimeEqual. Keeping the
	// doctor consistent avoids a future refactor forgetting the plain ==
	// and makes `grep -F '== localToken'` hits an audit signal.
	if token.ConstantTimeEqual(strings.TrimSpace(remoteToken), localToken) {
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
	// peerID is always generated by peer.LoadOrCreateLocalIdentity as a
	// hex string, and the caller (RunRemote) routes it through
	// peer.LoadOrCreateLocalIdentity before we ever see it. That said,
	// shellQuote is the only defense on this line, so if a future
	// contributor ever threads an operator-supplied peer ID in here they
	// MUST also validate it with peer.ValidateID first. Do not remove
	// the shellQuote wrapper.
	out, err := remoteExecNoForward(host, fmt.Sprintf("~/.local/bin/cc-clip peer show --peer-id %s", shellQuote(peerID)))
	out = strings.TrimSpace(out)
	if err != nil {
		if classifyDoctorPeerNotFound(err, out) {
			return nil, fmt.Errorf("peer lookup failed: %w", peer.ErrPeerNotFound)
		}
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

func classifyDoctorPeerNotFound(err error, out string) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	if exitErr.ExitCode() != exitcode.PeerNotFound {
		return false
	}
	return strings.Contains(out, exitcode.PeerNotFoundSentinel)
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
		strings.Contains(msg, "usage: cc-clip")
}

// checkTunnelStateAlignment validates that the locally-saved tunnel state for
// the host is consistent with the remote peer registration. The saved state
// is now the canonical local record of (host, remote-port, daemon local port);
// it replaces the old managed-block-in-~/.ssh/config check. The second return
// value is the matching tunnel state (if any) so the caller can derive the
// remote port from saved state even when the peer registration is missing.
func checkTunnelStateAlignment(host string, reg *peer.Registration, localPort int) ([]CheckResult, *tunnel.TunnelState) {
	states, err := loadTunnelStatesForHost(host)
	if err != nil {
		return []CheckResult{{"tunnel-state", false, fmt.Sprintf("cannot read local tunnel state: %v", err)}}, nil
	}
	if reg == nil || reg.ReservedPort == 0 {
		savedState, ok := selectSavedTunnelState(states, localPort)
		if ok {
			return []CheckResult{{"tunnel-state", true, fmt.Sprintf("peer SSH forwarding not configured; using saved tunnel state (remote:%d -> local:%d)", savedState.Config.RemotePort, savedState.Config.LocalPort)}}, savedState
		}
		if len(states) == 0 {
			return []CheckResult{{"tunnel-state", true, "peer SSH forwarding not configured; skipping local tunnel state check"}}, nil
		}
		if localPort > 0 {
			return []CheckResult{{"tunnel-state", false, fmt.Sprintf("peer SSH forwarding not configured and no saved tunnel state for %s on local port %d", host, localPort)}}, nil
		}
		return []CheckResult{{"tunnel-state", false, fmt.Sprintf("peer SSH forwarding not configured and multiple saved tunnel states exist for %s; rerun doctor with --port <local-port>", host)}}, nil
	}
	if len(states) == 0 {
		return []CheckResult{{"tunnel-state", false, fmt.Sprintf("no local tunnel state for %s; run 'cc-clip connect %s' to record it", host, host)}}, nil
	}
	if localPort > 0 {
		savedState, ok := selectSavedTunnelState(states, localPort)
		if !ok || savedState == nil {
			return []CheckResult{{"tunnel-state", false, fmt.Sprintf("no local tunnel state for %s on local port %d; rerun doctor with --port <local-port> or run 'cc-clip connect %s' to record it", host, localPort, host)}}, nil
		}
		if savedState.Config.RemotePort == reg.ReservedPort {
			return []CheckResult{{"tunnel-state", true, fmt.Sprintf("saved tunnel state matches remote register (remote:%d -> local:%d)", savedState.Config.RemotePort, savedState.Config.LocalPort)}}, savedState
		}
		return []CheckResult{{"tunnel-state", false, fmt.Sprintf("saved tunnel state for %s on local port %d uses remote port %d, but remote register uses %d; rerun 'cc-clip connect %s' to resync", host, savedState.Config.LocalPort, savedState.Config.RemotePort, reg.ReservedPort, host)}}, nil
	}
	for _, s := range states {
		if s != nil && s.Config.RemotePort == reg.ReservedPort {
			return []CheckResult{{"tunnel-state", true, fmt.Sprintf("saved tunnel state matches remote register (remote:%d -> local:%d)", s.Config.RemotePort, s.Config.LocalPort)}}, s
		}
	}
	// On mismatch, return nil rather than an arbitrary states[0]: picking an
	// unrelated saved tunnel's RemotePort here would mislead the downstream
	// reachability probe in RunRemote if the peer registration ever stops
	// overriding savedState (a change that is one-line away, see RunRemote's
	// "else if savedState != nil" branch).
	return []CheckResult{{"tunnel-state", false, fmt.Sprintf("remote register port %d not found in saved tunnel states for %s; rerun 'cc-clip connect %s' to resync", reg.ReservedPort, host, host)}}, nil
}

// checkSetEnvAlignment validates that the cc-clip-managed SetEnv marker
// block in the user's local ~/.ssh/config for `host` matches the current
// remote peer registration. The block pushes CC_CLIP_PORT /
// CC_CLIP_STATE_DIR into every interactive `ssh <host>` session, so a
// stale port or state-dir would silently mis-route the remote shims the
// next time the user types `ssh <host>`.
//
// Returns nil when there is nothing actionable to compare (no peer
// registration, unreadable config, or a legacy registration with no
// state_dir). When the managed block is absent but the current
// registration has enough data to reconstruct it, the check returns an
// OK advisory carrying the exact manual SetEnv line.
func checkSetEnvAlignment(host string, reg *peer.Registration) *CheckResult {
	if reg == nil || reg.ReservedPort == 0 {
		return nil
	}
	data, err := readLocalSSHConfig()
	if err != nil {
		return nil
	}
	env, err := sshconfig.ReadManagedEnvFromBytes(data, host)
	if err != nil {
		return &CheckResult{
			Name:    "ssh-config-setenv",
			OK:      false,
			Message: fmt.Sprintf("cannot parse SetEnv block for Host %s: %v", host, err),
		}
	}

	wantPort := fmt.Sprintf("%d", reg.ReservedPort)
	wantStateDir := strings.TrimSpace(reg.StateDir)
	expectedLine := ""
	if wantStateDir != "" {
		if line, lineErr := sshconfig.ManagedSetEnvLine(map[string]string{
			"CC_CLIP_PORT":      wantPort,
			"CC_CLIP_STATE_DIR": wantStateDir,
		}); lineErr == nil {
			expectedLine = line
		}
	}

	if env == nil {
		if expectedLine == "" {
			return nil
		}
		// A missing managed block when we already have a peer registration
		// is a real misconfiguration: the next interactive `ssh <host>`
		// session will not push CC_CLIP_PORT / CC_CLIP_STATE_DIR, so the
		// shared remote shims will mis-route. Report OK=false to match the
		// severity of the "block present but port stale" branch below — the
		// two states are equivalently broken from the operator's point of
		// view. Users who have not yet run `cc-clip connect` won't hit this
		// branch because the early nil return at the top of this function
		// shortcircuits when reg has no reserved port.
		return &CheckResult{
			Name:    "ssh-config-setenv",
			OK:      false,
			Message: fmt.Sprintf("no managed SetEnv block for Host %s; rerun 'cc-clip connect %s' to write it (exact manual line: %s)", host, host, expectedLine),
		}
	}

	gotPort := env["CC_CLIP_PORT"]
	gotStateDir := env["CC_CLIP_STATE_DIR"]

	if gotPort != wantPort {
		return &CheckResult{
			Name:    "ssh-config-setenv",
			OK:      false,
			Message: fmt.Sprintf("~/.ssh/config SetEnv CC_CLIP_PORT=%s for Host %s does not match remote register port %s; rerun 'cc-clip connect %s' to resync", gotPort, host, wantPort, host),
		}
	}
	// reg.StateDir may be empty for legacy registrations. Only compare
	// when the register actually carries a state dir.
	if wantStateDir != "" && gotStateDir != wantStateDir {
		return &CheckResult{
			Name:    "ssh-config-setenv",
			OK:      false,
			Message: fmt.Sprintf("~/.ssh/config SetEnv CC_CLIP_STATE_DIR=%q for Host %s does not match remote register state dir %q; rerun 'cc-clip connect %s' to resync", gotStateDir, host, wantStateDir, host),
		}
	}
	return &CheckResult{
		Name:    "ssh-config-setenv",
		OK:      true,
		Message: fmt.Sprintf("~/.ssh/config SetEnv block matches remote register for Host %s", host),
	}
}

// LegacyManagedBlockAdvisory returns the same leftover-block advisory message
// that the doctor emits, or "" if the user's ~/.ssh/config is clean. Exposed
// so `cc-clip uninstall` can print the same guidance without pulling the
// full CheckResult machinery into the uninstall path.
func LegacyManagedBlockAdvisory(host string) string {
	r := checkLegacyManagedBlock(host)
	if r == nil {
		return ""
	}
	return r.Message
}

// checkLegacyManagedBlock reads the local ~/.ssh/config and emits a passive
// advisory if a leftover "# >>> cc-clip managed host: …" block is still
// present. Returns nil when the file is absent, unreadable, or clean —
// emitting a passing check for a non-existent file would be noise.
//
// The check is intentionally non-fatal and reports OK=true: the presence of
// a legacy block does not mean anything is broken, only that an interactive
// `ssh <host>` will print a cosmetic "remote port forwarding failed"
// warning. Flipping `cc-clip doctor --host` to exit 1 on a cosmetic issue
// would break any CI/script that gates on doctor's exit code. We never
// auto-clean the file (per CLAUDE.md — migration is deliberately manual).
//
// When the block specifically names `host`, the message is precise. When it
// matches a different alias, the advisory still fires so users notice, but
// the wording is generic so they understand the mismatch.
func checkLegacyManagedBlock(host string) *CheckResult {
	data, err := readLocalSSHConfig()
	if err != nil {
		// Missing or unreadable ssh config — nothing to advise about.
		return nil
	}
	const markerPrefix = "# >>> cc-clip managed host:"
	s := string(data)
	if !strings.Contains(s, markerPrefix) {
		return nil
	}
	specific := false
	for _, alias := range legacyManagedBlockHosts(s) {
		if alias == host {
			specific = true
			break
		}
	}
	var message string
	if specific {
		message = fmt.Sprintf(
			"leftover '%s %s …' block in ~/.ssh/config; cc-clip only manages the newer SetEnv marker block now, so delete the block manually (it is a legacy tunnel block; see docs/troubleshooting.md 'Upgrade Leftover: Legacy Managed Block')",
			markerPrefix,
			host,
		)
	} else {
		message = fmt.Sprintf(
			"leftover '%s …' block in ~/.ssh/config for a different host alias; cc-clip only manages the newer SetEnv marker block now, so delete the block manually (it is a legacy tunnel block; see docs/troubleshooting.md 'Upgrade Leftover: Legacy Managed Block')",
			markerPrefix,
		)
	}
	// OK=true: cosmetic advisory. The doctor exit code must not flip on a
	// leftover block that does not actually break the daemon-owned tunnel.
	return &CheckResult{
		Name:    "ssh-config-legacy",
		OK:      true,
		Message: message,
	}
}

func legacyManagedBlockHosts(config string) []string {
	const markerPrefix = "# >>> cc-clip managed host:"
	var hosts []string
	scanner := bufio.NewScanner(strings.NewReader(config))
	// Default 64 KiB limit silently truncates pathologically long lines
	// and could miss a marker at the end of one. Lift to 1 MiB: any
	// single line in ~/.ssh/config beyond that is already malformed.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, markerPrefix) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, markerPrefix))
		if !strings.HasSuffix(rest, ">>>") {
			continue
		}
		alias := strings.TrimSpace(strings.TrimSuffix(rest, ">>>"))
		if alias != "" {
			hosts = append(hosts, alias)
		}
	}
	return hosts
}

// selectSavedTunnelState picks the saved state that best matches the caller's
// localPort, if any. loadTunnelStatesForHost guarantees no nil slots so this
// function does not defensively re-check for them.
func selectSavedTunnelState(states []*tunnel.TunnelState, localPort int) (*tunnel.TunnelState, bool) {
	if localPort > 0 {
		for _, s := range states {
			if s.Config.LocalPort == localPort {
				return s, true
			}
		}
		return nil, false
	}
	if len(states) == 1 {
		return states[0], true
	}
	return nil, false
}

func shellQuote(s string) string {
	return shellutil.ShellQuote(s)
}

func remotePathExpr(path string) string {
	return shellutil.RemoteShellPath(path)
}
