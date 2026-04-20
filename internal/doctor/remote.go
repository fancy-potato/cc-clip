package doctor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

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

var loadDoctorLocalIdentity = peer.LoadLocalIdentity

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

	ident, identErr := loadDoctorLocalIdentity()
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

	managedEnv, managedEnvErr := readManagedSetEnv(host)

	// Validate the SetEnv marker block in ~/.ssh/config matches the current
	// peer registration. A stale block (user ran `cc-clip connect` on a new
	// port without a subsequent rewrite, or hand-edited ~/.ssh/config) would
	// silently mis-route the remote shims on the next interactive ssh
	// session, so this check is strict (OK=false on mismatch) even though
	// the rest of the ssh-config-related advisories stay OK=true.
	if setEnvResult := checkSetEnvAlignment(host, reg, managedEnv, managedEnvErr); setEnvResult != nil {
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
		// The probe emits one of three distinct sentinels:
		//   tunnel ok               — verified HTTP 2xx from /health (real)
		//   tunnel ok (tcp-only)    — bare TCP listener, no HTTP verified
		//   tunnel fail             — nothing listens at all
		//
		// Previously, `nc -z` and `bash /dev/tcp` returning success were
		// reported as "tunnel ok" indistinguishably from a verified
		// HTTP round-trip. On a curl-less remote (alpine/busybox), a
		// dead daemon with a leftover SSH reverse forward would still
		// TCP-accept and be reported as healthy — defeating the whole
		// purpose of the check. Tag the degraded branches so operators
		// can see when the probe couldn't fully verify.
		probeScript := fmt.Sprintf(`sh -c '
p=%d
if command -v curl >/dev/null 2>&1; then
  if curl -sf -o /dev/null --max-time 3 "http://127.0.0.1:$p/health"; then echo tunnel ok; exit 0; fi
  # curl ran but could not reach /health — daemon is down or /health
  # endpoint is unexpectedly disabled. Do NOT fall through to the
  # TCP-only probe: if curl is installed, it is authoritative.
  echo tunnel fail
  exit 0
fi
if command -v nc >/dev/null 2>&1; then
  if nc -z -w 3 127.0.0.1 "$p" 2>/dev/null; then echo tunnel ok tcp-only; exit 0; fi
fi
if command -v bash >/dev/null 2>&1; then
  if bash -c "exec 3<>/dev/tcp/127.0.0.1/$p" 2>/dev/null; then echo tunnel ok tcp-only; exit 0; fi
fi
echo tunnel fail
'`, remotePort)
		out, err = remoteExecNoForward(host, probeScript)
		trimmed := strings.TrimSpace(out)
		switch {
		case strings.Contains(trimmed, "tunnel ok tcp-only"):
			results = append(results, CheckResult{"tunnel", true, fmt.Sprintf("port %d forwarded (TCP-only; install curl on remote for HTTP health verification)", remotePort)})
		case strings.Contains(trimmed, "tunnel ok"):
			results = append(results, CheckResult{"tunnel", true, fmt.Sprintf("port %d forwarded", remotePort)})
		default:
			results = append(results, CheckResult{"tunnel", false, fmt.Sprintf("port %d not reachable from remote (%s)", remotePort, trimmed)})
		}
	}

	// Check token on remote
	stateDir := resolveDoctorStateDir(reg, managedEnv)
	stateDirExpr := remotePathExpr(stateDir)
	// P2-25: distinguish "file missing" from "SSH failed". The old code
	// treated any non-"present" output as missing, which silently hid SSH
	// transport errors. Check err first so operators can tell the two apart.
	// P2-C: include sanitized stderr in the "cannot probe" message so the
	// operator can distinguish a network/auth failure ("Permission denied
	// (publickey)") from a harmless path-expansion issue. stderr is already
	// captured by runRemoteSSHCommand; without surfacing it here, the
	// advertised "SSH-failure vs file-missing" distinction provided no
	// extra signal beyond the bare Go error string.
	out, stderr, err := remoteExecNoForwardWithStderr(host, fmt.Sprintf("test -f %s/session.token && echo 'present' || echo 'missing'", stateDirExpr))
	if err != nil {
		msg := fmt.Sprintf("cannot probe session.token over ssh: %v", err)
		if s := strings.TrimSpace(sanitizeRemoteOutput(stderr)); s != "" {
			msg = fmt.Sprintf("%s (stderr: %s)", msg, s)
		}
		results = append(results, CheckResult{"remote-token", false, msg})
	} else if strings.Contains(out, "present") {
		results = append(results, CheckResult{"remote-token", true, "token file present"})
	} else {
		results = append(results, CheckResult{"remote-token", false, "token file missing"})
	}

	out, stderr, err = remoteExecNoForwardWithStderr(host, fmt.Sprintf("test -f %s/notify.nonce && echo 'present' || echo 'missing'", stateDirExpr))
	if err != nil {
		msg := fmt.Sprintf("cannot probe notify.nonce over ssh: %v", err)
		if s := strings.TrimSpace(sanitizeRemoteOutput(stderr)); s != "" {
			msg = fmt.Sprintf("%s (stderr: %s)", msg, s)
		}
		results = append(results, CheckResult{"remote-nonce", false, msg})
	} else {
		results = append(results, remoteNonceResult(deployState, strings.Contains(out, "present")))
	}

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

func readManagedSetEnv(host string) (map[string]string, error) {
	data, err := readLocalSSHConfig()
	if err != nil {
		return nil, nil
	}
	return sshconfig.ReadManagedEnvFromBytes(data, host)
}

func resolveDoctorStateDir(reg *peer.Registration, managedEnv map[string]string) string {
	if reg != nil && strings.TrimSpace(reg.StateDir) != "" {
		return strings.TrimSpace(reg.StateDir)
	}
	if managedEnv != nil {
		if stateDir := strings.TrimSpace(managedEnv["CC_CLIP_STATE_DIR"]); stateDir != "" {
			return stateDir
		}
	}
	return "~/.cache/cc-clip"
}

// remoteExecNoForward runs an SSH command without applying RemoteForward from ssh config.
// Doctor checks should inspect the existing tunnel, not compete with it by opening a new one.
//
// Every invocation is bounded by remoteExecTimeout and uses a short
// ConnectTimeout so an unreachable host fails fast instead of blocking the
// whole doctor run on OS-level TCP retry defaults.
//
// stdout and stderr are captured separately (P1-5). stdout is sanitized via
// sanitizeRemoteOutput: control characters (including ANSI escape sequences)
// are stripped and the string is length-capped. Callers embed the output
// directly into CheckResult messages shown in the user's terminal — without
// this sanitization, a compromised remote could emit terminal-manipulating
// escape sequences that rewrite the operator's view.
//
// Why stdout/stderr are split (P1-5): earlier code used CombinedOutput(),
// which concatenated SSH client warnings ("Warning: Permanently added …",
// "mux_client_forward", etc.) with the remote command's stdout. Callers
// that parse stdout as JSON (readDeployState, lookupPeer) would then fail
// to unmarshal. stderr is now returned inside the error message on
// failure but not merged into stdout on success.
func remoteExecNoForward(host string, args ...string) (string, error) {
	out, _, err := remoteExecNoForwardWithStderr(host, args...)
	return out, err
}

// remoteExecNoForwardRaw is the same as remoteExecNoForward but SKIPS
// sanitizeRemoteOutput on stdout — intended for JSON/structured parsers
// (readDeployState, lookupPeer). ANSI/control-character stripping can drop
// characters that are legitimate inside a JSON string, and the length cap
// would clip a large-enough deploy.json or peer list mid-byte. The raw
// stdout is ONLY safe to pass through a structured parser; do not echo it
// directly into CheckResult messages shown in the terminal.
//
// stderr is still captured separately and surfaced in the error message on
// failure, so SSH-client warnings never pollute the JSON body.
func remoteExecNoForwardRaw(host string, args ...string) (string, error) {
	out, _, err := remoteExecNoForwardRawWithStderr(host, args...)
	return out, err
}

// remoteExecNoForwardWithStderr returns (sanitized-stdout, raw-stderr, error).
// The stderr return value lets callers distinguish "file missing" from
// "SSH transport failed" (P2-25): both paths might return a stdout that
// does not contain "present", but only the latter has a populated stderr.
func remoteExecNoForwardWithStderr(host string, args ...string) (string, string, error) {
	stdout, stderr, err := runRemoteSSHCommand(host, args...)
	return sanitizeRemoteOutput(stdout), stderr, err
}

// remoteExecNoForwardRawWithStderr returns (raw-stdout, raw-stderr, error)
// and ALSO wraps stderr into the error message when err != nil. This lets
// structured-JSON consumers (readDeployState, lookupPeer) both:
//  1. Inspect raw stderr bytes directly — required by
//     classifyDoctorPeerNotFound, which looks for exitcode.PeerNotFoundSentinel
//     that the remote emits on stderr (see cmd/cc-clip/main.go ~line 3903).
//     Go's *exec.ExitError.Stderr is ONLY populated by cmd.Output(), never by
//     cmd.Run() with an explicit cmd.Stderr sink (which is what
//     runRemoteSSHCommand uses), so without this return channel the sentinel
//     would be unreachable from the classifier.
//  2. Get a human-readable error with stderr spliced in, so the caller's
//     fallback path (no classification match) still surfaces the remote's
//     complaint instead of a bare "exit status 22".
func remoteExecNoForwardRawWithStderr(host string, args ...string) (string, string, error) {
	stdout, stderr, err := runRemoteSSHCommand(host, args...)
	if err != nil {
		if strings.TrimSpace(stderr) != "" {
			return stdout, stderr, fmt.Errorf("%w (stderr: %s)", err, sanitizeRemoteOutput(stderr))
		}
		return stdout, stderr, err
	}
	return stdout, stderr, nil
}

// maxRemoteCommandOutput caps each of stdout and stderr captured from a
// single SSH doctor probe. Without this cap, a runaway remote command
// (e.g. `cat /dev/urandom`) or a transport layer dribbling infinite
// diagnostics on stderr could grow the in-process buffers without bound.
// 1 MiB is generously above any legitimate doctor probe output
// (deploy.json is typically under a KiB, the largest check is peer list
// which grows with the number of peers but stays well under this).
const maxRemoteCommandOutput = 1 << 20 // 1 MiB per stream

// runRemoteSSHCommand is the shared primitive. It returns (raw-stdout,
// raw-stderr, error). Callers are responsible for sanitization. Both
// stdout and stderr are each capped at maxRemoteCommandOutput bytes; if
// the cap is hit, the buffer is truncated and a stable marker is appended
// so callers (and operators reading the doctor output) can see that data
// was discarded rather than silently received a partial read.
func runRemoteSSHCommand(host string, args ...string) (string, string, error) {
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
	stdout := &cappedBuffer{cap: maxRemoteCommandOutput}
	stderr := &cappedBuffer{cap: maxRemoteCommandOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.String(), stderr.String(), fmt.Errorf("ssh to %s timed out after %s", host, remoteExecTimeout)
	}
	return stdout.String(), stderr.String(), err
}

// cappedBuffer is an io.Writer that accepts at most `cap` bytes into its
// underlying buffer. Writes beyond the cap are silently dropped but the
// first time the cap is crossed a stable marker is appended so callers can
// detect truncation. The Write signature still returns len(p) and nil
// error (rather than short-writing) because the ssh exec path treats a
// short write on stdout/stderr as a broken pipe — which would then cause
// ssh to exit non-zero even though the remote command succeeded.
type cappedBuffer struct {
	buf       bytes.Buffer
	cap       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := c.cap - c.buf.Len()
	if remaining <= 0 {
		if !c.truncated {
			c.truncated = true
			c.buf.WriteString("\n... [output truncated after ")
			c.buf.WriteString(fmt.Sprintf("%d", c.cap))
			c.buf.WriteString(" bytes] ...")
		}
		return n, nil
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		if !c.truncated {
			c.truncated = true
			c.buf.WriteString("\n... [output truncated after ")
			c.buf.WriteString(fmt.Sprintf("%d", c.cap))
			c.buf.WriteString(" bytes] ...")
		}
		return n, nil
	}
	c.buf.Write(p)
	return n, nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// sanitizeRemoteOutput strips ANSI escape sequences and non-printable
// control characters from remote command output before it is embedded in
// user-facing check messages. Output longer than maxRemoteOutputLen is
// truncated with an explicit marker so tokens or secrets accidentally
// printed by a compromised daemon cannot be flooded into the terminal.
// Newlines and tabs are preserved because doctor messages legitimately
// span multiple lines (e.g. `head -2 ~/.local/bin/xclip`).
//
// C1 escape notes: we strip 7-bit CSI (ESC '[' …) and OSC (ESC ']' …)
// explicitly, but do NOT special-case the 8-bit C1 controls CSI (0x9B)
// or DCS (0x90). In a UTF-8 terminal (the baseline assumption for
// cc-clip output), lone bytes in 0x80-0xBF are invalid UTF-8 continuation
// bytes; Go's []rune conversion replaces each with U+FFFD (REPLACEMENT
// CHARACTER), which is a visible glyph but is not interpreted as a
// terminal-escape trigger. The 8-bit forms only activate on VT100/VT220
// terminals configured for 8-bit C1 mode, which modern terminals almost
// never are. If that assumption changes, add an explicit strip for
// 0x80-0x9F before the UTF-8 decode.
func sanitizeRemoteOutput(s string) string {
	const maxRemoteOutputLen = 4096
	var b strings.Builder
	b.Grow(len(s))
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		// Strip ESC-prefixed sequences (CSI, OSC, etc.). ANSI CSI is ESC
		// followed by '[' and parameter bytes ending in a byte in 0x40-0x7e;
		// we conservatively consume ESC + the next non-alphanumeric run.
		if r == 0x1b {
			// skip ESC and up to 32 following bytes of an ANSI/OSC sequence
			j := i + 1
			if j < len(runes) && (runes[j] == '[' || runes[j] == ']') {
				j++
				for j < len(runes) && j-i < 32 {
					c := runes[j]
					j++
					// Terminators for CSI (final byte 0x40-0x7e) or OSC (BEL/ST).
					if (c >= 0x40 && c <= 0x7e) || c == 0x07 {
						break
					}
				}
			} else if j < len(runes) {
				j++
			}
			i = j - 1
			continue
		}
		if r == '\n' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	// Rune-safe truncation (P2-26): cutting at a byte boundary could split
	// a multi-byte UTF-8 rune, leaving an invalid trailing sequence that
	// renders as a mojibake glyph in the operator's terminal. Walk back to
	// the nearest rune boundary via utf8.DecodeLastRuneInString before
	// appending the "[truncated]" marker.
	if len(out) > maxRemoteOutputLen {
		cut := maxRemoteOutputLen
		for cut > 0 {
			r, size := utf8.DecodeLastRuneInString(out[:cut])
			if r == utf8.RuneError && size == 1 {
				// Landed on a continuation byte; back up one more and
				// retry. In the worst case (3-byte rune split), this
				// iterates at most 3 times.
				cut--
				continue
			}
			break
		}
		out = out[:cut] + " …[truncated]"
	}
	return out
}

func resolveInInteractiveShell(host, bin string) (string, error) {
	shellPath, _ := remoteExecNoForward(host, "echo $SHELL")
	script := buildInteractiveShellProbe(shellPath, bin)
	out, err := remoteExecNoForward(host, script)
	return strings.TrimSpace(out), err
}

// buildInteractiveShellProbe is a pure helper extracted from
// resolveInInteractiveShell so tests can pin the literal-only contract:
// shellPath is attacker-influenceable (the remote's $SHELL env), so the
// returned script must NEVER interpolate shellPath verbatim — only one of
// the three literal constants "bash" / "zsh" / "fish" is ever placed in
// the script. A future "optimization" that passes shellPath directly to
// ssh would let a crafted $SHELL (e.g. `/bin/bash -c pwned`) reach the
// remote shell verbatim. To support more shells, extend the switch with
// new literal constants; do NOT widen to raw shellPath.
//
// fish does not accept `-ic` — it uses `-i -c` with a different quoting
// convention. The script form is shell-appropriate for each.
func buildInteractiveShellProbe(shellPath, bin string) string {
	shellName := "bash"
	switch {
	case strings.Contains(shellPath, "zsh"):
		shellName = "zsh"
	case strings.Contains(shellPath, "fish"):
		shellName = "fish"
	}
	if shellName == "fish" {
		return fmt.Sprintf(`fish -i -c 'which %s 2>/dev/null; or echo "not in PATH"'`, bin)
	}
	return fmt.Sprintf(`%s -ic 'which %s 2>/dev/null || echo "not in PATH"'`, shellName, bin)
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
	//
	// Timeout layering: curl --max-time 5 is the inner deadline for the
	// HTTP round-trip itself; the surrounding remoteExecNoForward applies
	// remoteExecTimeout (15 s) for the SSH connect + command lifecycle.
	// The two are intentionally distinct because the failure modes are
	// distinct: curl >5s means "the daemon is up but slow / the tunnel is
	// degrading the request"; ssh >15s means "we cannot even reach the
	// remote host". Aligning them to the same value would conflate those
	// signals and either report SSH issues as image-probe failures (if
	// curl swallows the SSH delay) or vice versa.
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
//
// Uses the raw-output variant (NOT remoteExecNoForward, which strips
// control bytes via sanitizeRemoteOutput). Today's token formats are
// base64/hex and survive the sanitizer intact, but a future format
// that contains any byte <0x20 (a literal NUL terminator, a UTF-8
// encoding with a trailing BOM, etc.) would be corrupted by the
// sanitizer and produce a misleading "remote token differs" report —
// telling the operator to rerun `cc-clip connect` when in fact the
// tokens are byte-identical. The token comparison must be byte-exact.
func checkTokenMatch(host string, stateDir string) []CheckResult {
	localToken, err := token.ReadTokenFile()
	if err != nil {
		return []CheckResult{{"token-match", false, "cannot read local token to compare"}}
	}

	remoteToken, err := remoteExecNoForwardRaw(host, fmt.Sprintf("cat %s/session.token 2>/dev/null", remotePathExpr(stateDir)))
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
//
// The deploy state file is intentionally at the canonical path
// `~/.cache/cc-clip/deploy.json` rather than a per-peer stateDir location:
// deploy.json tracks binary hash / shim install / path-fix state which is
// shared across all peers on the same Unix account (each peer does NOT
// re-upload the binary or re-install the shim — those are one-shot, shared
// remote assets). Per-peer state lives under the peer's stateDir
// (session.token, notify.nonce, notify-health.log). Keeping deploy.json
// canonical is what lets a second-laptop `cc-clip connect` skip the binary
// upload entirely via the hash check.
//
// Uses the raw-output variant so ANSI sanitizer never corrupts a
// structured JSON body. stderr is surfaced in the error message rather
// than spliced into stdout.
//
// The remote command intentionally splits "missing" from "unreadable":
// the previous form `cat … 2>/dev/null || echo __NOTFOUND__` swallowed
// permission/quota/IO errors as `__NOTFOUND__`, so doctor reported a
// healthy host as "deploy.json not found" and steered users toward
// `cc-clip connect --force` instead of investigating. Now `test -e`
// distinguishes the two: file absent → __NOTFOUND__; file present →
// `cat` runs WITHOUT stderr redirection so any read failure surfaces as
// a non-zero ssh exit and is propagated as an error. Pinned by
// TestReadDeployStateRemoteCommandSurfacesReadErrors.
const readDeployStateRemoteCommand = "if [ ! -e ~/.cache/cc-clip/deploy.json ]; then printf '__NOTFOUND__\\n'; else cat ~/.cache/cc-clip/deploy.json; fi"

func readDeployState(host string) (*shim.DeployState, error) {
	out, err := remoteExecNoForwardRaw(host, readDeployStateRemoteCommand)
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
	// peerID reaches this function through the local-identity loader, so it
	// should already satisfy peer.ValidateID. That said, shellQuote is the
	// only defense on this line, so if a future contributor ever threads an
	// operator-supplied peer ID in here they MUST also validate it with
	// peer.ValidateID first. Do not remove the shellQuote wrapper.
	//
	// Uses the raw-output variant with explicit stderr capture (P2-27 +
	// P1-A): the stdout must round-trip through json.Unmarshal unchanged,
	// and the ANSI/control-character sanitizer in the default
	// remoteExecNoForward would strip bytes that are legitimate inside a
	// JSON string (e.g. an escape sequence inside a peer label). The raw
	// stderr is required separately because classifyDoctorPeerNotFound
	// matches on exitcode.PeerNotFoundSentinel that the remote emits to
	// stderr — cmd.Run() with an explicit cmd.Stderr sink (our path) does
	// NOT populate *exec.ExitError.Stderr, so the classifier cannot read
	// the sentinel from the error object; it must be passed in explicitly.
	out, stderr, err := remoteExecNoForwardRawWithStderr(host, fmt.Sprintf("~/.local/bin/cc-clip peer show --peer-id %s", shellQuote(peerID)))
	out = strings.TrimSpace(out)
	if err != nil {
		if classifyDoctorPeerNotFound(err, out, stderr) {
			return nil, fmt.Errorf("peer lookup failed: %w", peer.ErrPeerNotFound)
		}
		// Sanitize before embedding in the operator-facing error message:
		// the raw JSON path intentionally skipped sanitization, so ANSI
		// escapes from a compromised remote could otherwise rewrite the
		// operator's terminal. Keep the raw `out` for the JSON-parse step
		// above; only the error-message version goes through sanitation.
		if out != "" {
			return nil, fmt.Errorf("%s", sanitizeRemoteOutput(out))
		}
		return nil, err
	}
	var reg peer.Registration
	if err := json.Unmarshal([]byte(out), &reg); err != nil {
		return nil, fmt.Errorf("unexpected peer registry output: %s", sanitizeRemoteOutput(out))
	}
	return &reg, nil
}

// classifyDoctorPeerNotFound returns true iff the remote cc-clip peer
// command exited with exitcode.PeerNotFound AND emitted
// exitcode.PeerNotFoundSentinel on stderr (or in the fallback path, on
// stdout — see below). BOTH signals are required as defense in depth: a
// transport layer exiting 22 for unrelated reasons (ssh plugin, sandbox)
// would otherwise be misclassified as an idempotent "peer already
// released".
//
// The stderr argument is the raw stderr captured by runRemoteSSHCommand —
// callers MUST pass it explicitly. Reading exitErr.Stderr here does NOT
// work because our SSH exec path uses cmd.Run() with cmd.Stderr = &buf,
// which leaves exitErr.Stderr nil (Go only populates that field when the
// caller uses cmd.Output()). The stdout-sentinel branch remains as a
// belt-and-suspenders fallback for a remote wrapper that redirects stderr
// into stdout.
func classifyDoctorPeerNotFound(err error, stdout, stderr string) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	if exitErr.ExitCode() != exitcode.PeerNotFound {
		return false
	}
	if strings.Contains(stderr, exitcode.PeerNotFoundSentinel) {
		return true
	}
	// Belt-and-suspenders: exitErr.Stderr is populated only when the caller
	// used cmd.Output(). We do NOT in production, but a future refactor or
	// a different test harness might, so honor it if it's populated.
	if bytes.Contains(exitErr.Stderr, []byte(exitcode.PeerNotFoundSentinel)) {
		return true
	}
	return strings.Contains(stdout, exitcode.PeerNotFoundSentinel)
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

// isLegacyPeerLookupError detects the narrow case where the REMOTE binary
// is too old to know about the `peer` subcommand at all. The signal we trust
// is the dispatcher's `unknown command: peer\n` line (cmd/cc-clip/main.go).
//
// We deliberately do NOT match the broader substring `usage: cc-clip`: a
// modern remote that fails on a `peer show` argument prints
// `usage: cc-clip peer show ...` to stderr too. Treating that as "remote is
// legacy" silently hides real broken-arg / version-mismatch failures behind
// a green check.
func isLegacyPeerLookupError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "unknown command: peer")
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
func checkSetEnvAlignment(host string, reg *peer.Registration, managedEnv map[string]string, managedEnvErr error) *CheckResult {
	if reg == nil || reg.ReservedPort == 0 {
		return nil
	}
	if managedEnv == nil && managedEnvErr == nil {
		managedEnv, managedEnvErr = readManagedSetEnv(host)
	}
	if managedEnvErr != nil {
		return &CheckResult{
			Name:    "ssh-config-setenv",
			OK:      false,
			Message: fmt.Sprintf("cannot parse SetEnv block for Host %s: %v", host, managedEnvErr),
		}
	}

	wantPort := fmt.Sprintf("%d", reg.ReservedPort)
	wantStateDir := strings.TrimSpace(reg.StateDir)
	expectedEnv := map[string]string{
		"CC_CLIP_PORT": wantPort,
	}
	if wantStateDir != "" {
		expectedEnv["CC_CLIP_STATE_DIR"] = wantStateDir
	}
	expectedLine := ""
	if line, lineErr := sshconfig.ManagedSetEnvLine(expectedEnv); lineErr == nil {
		expectedLine = line
	}

	if managedEnv == nil {
		if expectedLine == "" {
			return nil
		}
		layoutReason := ""
		data, err := readLocalSSHConfig()
		if err != nil {
			// Surface "skipped because config is unreadable" as an advisory
			// rather than silently returning nil. A missing/permission-denied
			// ~/.ssh/config is not a hard failure (cc-clip can still operate
			// with manual env in the user's shell), but the operator should
			// see WHY the SetEnv-alignment check did not run.
			return &CheckResult{
				Name:    "ssh-config-setenv",
				OK:      true,
				Message: fmt.Sprintf("skipped: cannot read ~/.ssh/config (%v); add SetEnv manually if needed: %s", err, expectedLine),
			}
		}
		switch statusErr := sshconfig.HostBlockStatusFromBytes(data, host); {
		case errors.Is(statusErr, sshconfig.ErrOnlyGlobMatch):
			layoutReason = fmt.Sprintf("Host %s is matched only by wildcard/negation patterns, so cc-clip will not auto-write the SetEnv block; add a literal `Host %s` entry or merge this line manually", host, host)
		case errors.Is(statusErr, sshconfig.ErrHostBlockInInclude):
			layoutReason = fmt.Sprintf("Host %s exists only behind an Include, so cc-clip will not auto-write the SetEnv block there; inline a literal `Host %s` entry or merge this line manually", host, host)
		case errors.Is(statusErr, sshconfig.ErrHostBlockMissing):
			layoutReason = fmt.Sprintf("no literal `Host %s` block exists in ~/.ssh/config; add one or merge this line manually", host)
		}
		if layoutReason != "" {
			return &CheckResult{
				Name:    "ssh-config-setenv",
				OK:      false,
				Message: fmt.Sprintf("no managed SetEnv block for Host %s; %s (exact manual line: %s)", host, layoutReason, expectedLine),
			}
		}
		return &CheckResult{
			Name:    "ssh-config-setenv",
			OK:      false,
			Message: fmt.Sprintf("no managed SetEnv block for Host %s; rerun 'cc-clip connect %s' to write it (exact manual line: %s)", host, host, expectedLine),
		}
	}

	gotPort := managedEnv["CC_CLIP_PORT"]
	gotStateDir := managedEnv["CC_CLIP_STATE_DIR"]

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
	// bufio.Scanner silently drops its final error if the caller never
	// checks Err() — e.g. an unterminated last line past the 1 MiB buffer
	// (bufio.ErrTooLong) would truncate the scan without any signal. Log
	// so a user whose ssh_config is corrupt or pathologically long sees
	// why the doctor check appears to miss a legacy block.
	if err := scanner.Err(); err != nil {
		log.Printf("doctor: warning: ssh_config scan truncated: %v", err)
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
