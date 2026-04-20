package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shunmei/cc-clip/internal/exitcode"
	"github.com/shunmei/cc-clip/internal/token"
	"github.com/shunmei/cc-clip/internal/tunnel"
)

var errDaemonUnreachable = errors.New("daemon not reachable")
var errDaemonAuth = errors.New("daemon auth failed")

// errDaemonTunnelListUnavailable is the umbrella sentinel for "tunnel list
// could not run to completion". The two concrete sub-reasons below wrap it
// so `errors.Is(err, errDaemonTunnelListUnavailable)` continues to succeed
// while the classifier can now distinguish "local control token missing"
// from "daemon returned 404/405" — each gets its own exit code.
var errDaemonTunnelListUnavailable = errors.New("daemon tunnel list unavailable")

// errDaemonTunnelControlTokenUnavailable covers the case where the local
// tunnel-control token file is missing or malformed. Not the same as the
// remote clipboard token: this is a purely-local credential, so a scripted
// consumer can act on it with a dedicated exit code (daemon-never-ran hint
// vs. tunnel-unreachable retry loop).
var errDaemonTunnelControlTokenUnavailable = fmt.Errorf("%w: local tunnel control token unavailable", errDaemonTunnelListUnavailable)

// errDaemonTunnelListUnreachable covers the case where the daemon replied
// 404 or 405 to GET /tunnels — either an old build without tunnel routes
// or a non-cc-clip listener has taken the port. Classified as
// TunnelUnreachable so retry scripts already tuned for that code act on
// it without new branches.
var errDaemonTunnelListUnreachable = fmt.Errorf("%w: tunnel control endpoint not reachable", errDaemonTunnelListUnavailable)

var errDaemonTunnelControlUnavailable = errors.New("daemon tunnel control unavailable")
var errDaemonManagerShuttingDown = errors.New("daemon tunnel manager is shutting down")
var errTunnelUpPortResolutionUsage = errors.New("tunnel up port resolution requires operator input")

const maxTunnelPort = 65535

func cmdTunnel() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: cc-clip tunnel <list|up|down|remove> [args]")
		os.Exit(exitcode.UsageError)
	}
	switch os.Args[2] {
	case "list":
		cmdTunnelList()
	case "up":
		warnIfJSONFlagIgnored("up")
		cmdTunnelUp()
	case "down":
		warnIfJSONFlagIgnored("down")
		cmdTunnelDown()
	case "remove", "rm":
		warnIfJSONFlagIgnored("remove")
		cmdTunnelRemove()
	default:
		fmt.Fprintf(os.Stderr, "unknown tunnel subcommand: %s\n", os.Args[2])
		os.Exit(exitcode.UsageError)
	}
}

// warnIfJSONFlagIgnored emits a stderr note when a scripting user passes
// --json to a subcommand that doesn't emit structured output. Silently
// dropping the flag used to hide misconfigured automation — the JSON
// consumer would parse stdout as text forever without a hint.
func warnIfJSONFlagIgnored(subcommand string) {
	if hasFlag("json") {
		fmt.Fprintf(os.Stderr, "cc-clip: --json has no effect on `tunnel %s`; ignoring\n", subcommand)
	}
}

func cmdTunnelList() {
	port := getPort()
	asJSON := hasFlag("json")

	states, err := loadTunnelStatesForList(port)
	if err != nil {
		if asJSON {
			// Scripted consumers (SwiftBar, shell pipelines) parse stdout
			// as JSON before checking the exit code. If we exit non-zero
			// with an empty stdout here, `jq` and friends see "unexpected
			// end of input" and surface a misleading parse error instead
			// of the real failure. Emit a well-formed empty array so the
			// caller can still decode, and put the diagnostic on stderr
			// and in the exit code where it belongs.
			fmt.Fprintln(os.Stderr, "tunnel list failed:", err)
			fmt.Println("[]")
			os.Exit(exitCodeForTunnelError(err))
		}
		fatalWithTunnelExitCode("tunnel list failed", err)
	}

	if asJSON {
		// Encode into a buffer first so a partial / failed encode never
		// reaches stdout. A mid-slice failure during a direct
		// json.NewEncoder(os.Stdout).Encode(states) would emit an
		// invalid JSON prefix, and the fallback `[]\n` would append a
		// second document on top of it — scripted consumers (SwiftBar,
		// jq) would then choke on "extra data after JSON document" with
		// no way to detect the corruption.
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		if err := enc.Encode(states); err != nil {
			fmt.Fprintln(os.Stderr, "json encode:", err)
			fmt.Println("[]")
			os.Exit(exitcode.InternalError)
		}
		if _, err := os.Stdout.Write(buf.Bytes()); err != nil {
			fmt.Fprintln(os.Stderr, "stdout write:", err)
			os.Exit(exitcode.InternalError)
		}
		return
	}

	if len(states) == 0 {
		fmt.Println("No tunnels configured.")
		return
	}

	fmt.Printf("%-25s %-6s %-6s %-14s %s\n", "HOST", "LOCAL", "REMOTE", "STATUS", "PID")
	for _, s := range states {
		pid := "-"
		if s.PID > 0 {
			pid = fmt.Sprintf("%d", s.PID)
		}
		fmt.Printf("%-25s %-6d %-6d %-14s %s\n",
			s.Config.Host,
			s.Config.LocalPort,
			s.Config.RemotePort,
			s.Status,
			pid,
		)
	}
}

func loadTunnelStatesForList(daemonPort int) ([]*tunnel.TunnelState, error) {
	return loadTunnelStatesForListWith(daemonPort, fetchTunnelList, tunnel.LoadAllStates)
}

func loadTunnelStatesForListWith(
	daemonPort int,
	fetchFn func(int) ([]*tunnel.TunnelState, error),
	loadStatesFn func(string) ([]*tunnel.TunnelState, error),
) ([]*tunnel.TunnelState, error) {
	primaryStates, primaryErr := fetchFn(daemonPort)
	if primaryErr != nil && !isRecoverableTunnelListError(primaryErr) {
		return nil, primaryErr
	}

	diskStates, loadErr := loadStatesFn(tunnel.DefaultStateDir())
	if loadErr != nil {
		if primaryErr == nil {
			// Daemon list succeeded but we could not read on-disk state;
			// surface a diagnostic so a user who expected offline tunnels
			// in the list knows why they are missing. The daemon-owned
			// tunnels still display, so this is a warn-and-continue rather
			// than a hard failure.
			fmt.Fprintf(os.Stderr, "warning: cannot read on-disk tunnel state (%v); list limited to tunnels the daemon owns\n", loadErr)
			merged := make(map[string]*tunnel.TunnelState, len(primaryStates))
			mergeOwnedTunnelStates(merged, daemonPort, primaryStates)
			return sortTunnelStates(merged), nil
		}
		return nil, loadErr
	}

	merged := make(map[string]*tunnel.TunnelState, len(diskStates))
	for _, s := range normalizeOfflineTunnelStates(diskStates) {
		if s == nil {
			continue
		}
		merged[tunnelStateKey(s)] = s
	}

	queriedAny := false
	if primaryErr == nil {
		queriedAny = true
		mergeOwnedTunnelStates(merged, daemonPort, primaryStates)
	}

	// Only the selected daemon port is queried over HTTP. Fanning the
	// tunnel-control token out to every saved local_port would leak it to
	// whatever process happens to be bound there — not necessarily cc-clip.
	// Entries owned by other daemon ports fall through to their on-disk
	// state (already merged above). Users who want live status for a
	// non-selected daemon can rerun `tunnel list --port <that-port>`.

	if !queriedAny {
		return normalizeOfflineTunnelStates(diskStates), nil
	}
	return sortTunnelStates(merged), nil
}

func tunnelStateKey(s *tunnel.TunnelState) string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf("%s\x00%d", s.Config.Host, s.Config.LocalPort)
}

func mergeOwnedTunnelStates(dst map[string]*tunnel.TunnelState, port int, states []*tunnel.TunnelState) {
	for _, s := range states {
		if s == nil {
			continue
		}
		if s.Config.LocalPort != 0 && s.Config.LocalPort != port {
			continue
		}
		dst[tunnelStateKey(s)] = s
	}
}

func sortTunnelStates(states map[string]*tunnel.TunnelState) []*tunnel.TunnelState {
	out := make([]*tunnel.TunnelState, 0, len(states))
	for _, s := range states {
		if s != nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Config.Host == out[j].Config.Host {
			return out[i].Config.LocalPort < out[j].Config.LocalPort
		}
		return out[i].Config.Host < out[j].Config.Host
	})
	return out
}

func cmdTunnelUp() {
	host, err := resolveTunnelHostArg(os.Args, 3, "cc-clip tunnel up <host> [--port PORT] [--remote-port PORT]", "--port", "--remote-port")
	if err != nil {
		fatalTunnelUsage("", err)
	}
	daemonPort := getPort()
	daemonPortExplicit := daemonPortConfiguredExplicitly()

	remotePort := 0
	if v := getFlag("remote-port", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			fatalTunnelUsage("", fmt.Errorf("invalid --remote-port %q: %v", v, err))
		}
		if err := validateTunnelPort("--remote-port", n, false); err != nil {
			fatalTunnelUsage("", err)
		}
		remotePort = n
	}

	remotePort, daemonPort, err = resolveTunnelUpPorts(host, remotePort, daemonPort, daemonPortExplicit)
	if err != nil {
		if isTunnelUpPortResolutionUsageError(err) {
			fatalTunnelUsage("resolve tunnel ports", err)
		}
		fatalWithTunnelExitCode("resolve tunnel ports", err)
	}
	if remotePort == 0 {
		fatalTunnelUsage("", errors.New(cannotDetermineRemotePortMessage(host)))
	}
	// --port / --remote-port validation after resolve is still operator-input:
	// the only way a bad value reaches this point is via the user's --port
	// flag or a resolvedrRemote-port that matched a legacy saved state.
	if err := validateTunnelPort("--port", daemonPort, false); err != nil {
		fatalTunnelUsage("resolve tunnel ports", err)
	}
	if err := validateTunnelPort("--remote-port", remotePort, false); err != nil {
		fatalTunnelUsage("resolve tunnel ports", err)
	}

	if err := postTunnelUp(daemonPort, host, remotePort); err != nil {
		fatalWithTunnelExitCode("tunnel up failed", err)
	}
	fmt.Printf("Tunnel to %s started (remote:%d -> local:%d)\n", host, remotePort, daemonPort)
}

// cannotDetermineRemotePortMessage formats the actionable error for
// `cc-clip tunnel up <host>` when no saved tunnel state exists. The
// wording is contractual — it names `cc-clip connect <host>` as the
// preferred fix and `--remote-port` as the manual override. Install
// scripts and docs reference both phrases, so this helper exists to make
// the wording directly testable without subprocess-running the binary
// past log.Fatal. Pinned by TestCannotDetermineRemotePortMessageWording.
func cannotDetermineRemotePortMessage(host string) string {
	return fmt.Sprintf(
		"cannot determine remote port for %s. Either run `cc-clip connect %s` first (records the remote port locally), or re-run with `--remote-port <PORT>` explicitly.",
		host, host,
	)
}

// resolveTunnelUpPorts fills in missing remotePort / daemonPort values from
// the locally saved tunnel state. When --port was not set explicitly the
// CLI adopts whatever daemon port the saved tunnel points at (the one daemon
// that owns the forward for that host). If --port was explicit and diverges
// from the saved value, the call errors rather than silently crossing to a
// different daemon. Passing --remote-port explicitly bypasses only the saved
// remote-port lookup: when daemon ownership is unambiguous, the CLI still
// adopts the saved local port so recovery requests reach the daemon that owns
// the tunnel.
func resolveTunnelUpPorts(host string, remotePort, daemonPort int, daemonPortExplicit bool) (int, int, error) {
	states, err := tunnel.LoadStatesForHost(tunnel.DefaultStateDir(), host)
	if err != nil {
		return 0, daemonPort, fmt.Errorf("load tunnel state for %s: %w", host, err)
	}
	ownerStates := tunnelOwnerStates(states)
	switch len(ownerStates) {
	case 0:
		if len(states) == 0 {
			return remotePort, daemonPort, nil
		}
		// Legacy or corrupt states with local_port == 0 cannot identify an
		// owning daemon. Explicit --remote-port can only bypass the saved
		// remote-port lookup after the caller has explicitly selected a daemon
		// with --port; otherwise the default/current daemon might be wrong on a
		// multi-daemon machine.
		if remotePort != 0 {
			if !daemonPortExplicit {
				return 0, daemonPort, fmt.Errorf("%w: saved tunnel for %s has no local_port, so daemon ownership is ambiguous; pass --port <local-port> explicitly when using --remote-port <port>", errTunnelUpPortResolutionUsage, host)
			}
			return remotePort, daemonPort, nil
		}
		return 0, daemonPort, fmt.Errorf("%w: saved tunnel for %s has no local_port; re-run `cc-clip connect %s` to rewrite the state file, or pass both --port <local-port> and --remote-port <remote-port> explicitly (both are required here because legacy state cannot identify the owning daemon)", errTunnelUpPortResolutionUsage, host, host)
	case 1:
		s := ownerStates[0]
		if !daemonPortExplicit {
			daemonPort = s.Config.LocalPort
		} else if daemonPort != s.Config.LocalPort {
			return 0, daemonPort, fmt.Errorf("%w: saved tunnel for %s uses local port %d (remote port %d); rerun with --port %d to target the owning daemon", errTunnelUpPortResolutionUsage, host, s.Config.LocalPort, s.Config.RemotePort, s.Config.LocalPort)
		}
		if remotePort == 0 && s.Config.RemotePort > 0 {
			remotePort = s.Config.RemotePort
		}
		return remotePort, daemonPort, nil
	default:
		if !daemonPortExplicit {
			if remotePort != 0 {
				return 0, daemonPort, fmt.Errorf("%w: %s; multiple saved daemon owners exist, so pass --port <local-port> explicitly along with --remote-port <remote-port>", tunnel.ErrAmbiguousTunnelState, host)
			}
			return 0, daemonPort, fmt.Errorf("%w: %s; pass --port <local-port> to select the owning daemon", tunnel.ErrAmbiguousTunnelState, host)
		}
		var match *tunnel.TunnelState
		for _, s := range ownerStates {
			if s != nil && s.Config.LocalPort == daemonPort {
				match = s
				break
			}
		}
		if match == nil {
			return 0, daemonPort, fmt.Errorf("%w: saved tunnels for %s use local ports %s; rerun with --port one of those values", errTunnelUpPortResolutionUsage, host, joinTunnelStatePorts(ownerStates))
		}
		if remotePort == 0 && match.Config.RemotePort > 0 {
			remotePort = match.Config.RemotePort
		}
		return remotePort, daemonPort, nil
	}
}

func isTunnelUpPortResolutionUsageError(err error) bool {
	return errors.Is(err, errTunnelUpPortResolutionUsage) || errors.Is(err, tunnel.ErrAmbiguousTunnelState)
}

func tunnelOwnerStates(states []*tunnel.TunnelState) []*tunnel.TunnelState {
	owners := make([]*tunnel.TunnelState, 0, len(states))
	for _, s := range states {
		if s != nil && s.Config.LocalPort > 0 {
			owners = append(owners, s)
		}
	}
	return owners
}

func joinTunnelStatePorts(states []*tunnel.TunnelState) string {
	ports := make([]int, 0, len(states))
	for _, s := range states {
		if s != nil && s.Config.LocalPort > 0 {
			ports = append(ports, s.Config.LocalPort)
		}
	}
	return joinPorts(ports)
}

func cmdTunnelDown() {
	host, err := resolveTunnelHostArg(os.Args, 3, "cc-clip tunnel down <host> [--port PORT]", "--port")
	if err != nil {
		fatalTunnelUsage("", err)
	}
	daemonPort := getPort()
	daemonPortExplicit := daemonPortConfiguredExplicitly()
	if err := validateTunnelPort("--port", daemonPort, false); err != nil {
		fatalTunnelUsage("", err)
	}

	if err := stopTunnel(daemonPort, host, daemonPortExplicit); err != nil {
		fatalWithTunnelExitCode("tunnel down failed", err)
	}
	fmt.Printf("Tunnel to %s stopped\n", host)
}

func cmdTunnelRemove() {
	host, err := resolveTunnelHostArg(os.Args, 3, "cc-clip tunnel remove <host> [--port PORT]", "--port")
	if err != nil {
		fatalTunnelUsage("", err)
	}
	daemonPort := getPort()
	daemonPortExplicit := daemonPortConfiguredExplicitly()
	if err := validateTunnelPort("--port", daemonPort, false); err != nil {
		fatalTunnelUsage("", err)
	}

	if err := removeTunnel(daemonPort, host, daemonPortExplicit); err != nil {
		fatalWithTunnelExitCode("tunnel remove failed", err)
	}
	fmt.Printf("Tunnel to %s removed\n", host)
}

func removeTunnel(daemonPort int, host string, daemonPortExplicit bool) error {
	return removeTunnelWithRetryPolicy(daemonPort, host, !daemonPortExplicit, postTunnelRemove, persistTunnelRemoveOffline)
}

func removeTunnelWith(
	daemonPort int,
	host string,
	postFn func(int, string) error,
	persistFn func(string, int) error,
) error {
	return removeTunnelWithRetryPolicy(daemonPort, host, true, postFn, persistFn)
}

func removeTunnelWithRetryPolicy(
	daemonPort int,
	host string,
	allowFallback bool,
	postFn func(int, string) error,
	persistFn func(string, int) error,
) error {
	targetPort := daemonPort

	err := postFn(daemonPort, host)
	if err == nil {
		// Defense in depth: the daemon's /tunnels/remove handler should
		// have deleted the state file, but if it logged a RemoveState
		// error and returned 200 anyway we would leave the file on disk
		// and the next `tunnel list` would surface a phantom entry.
		// tunnel.RemoveState is idempotent against missing files.
		_ = tunnel.RemoveState(tunnel.DefaultStateDir(), host, targetPort)
		return nil
	}
	if !isRecoverableTunnelDownError(err) && !errors.Is(err, tunnel.ErrTunnelNotFound) {
		return err
	}
	if errors.Is(err, errDaemonManagerShuttingDown) {
		return fmt.Errorf("daemon is shutting down; retry `cc-clip tunnel remove %s` in a moment", host)
	}
	retry := false
	if allowFallback && (isRecoverableTunnelDownError(err) || errors.Is(err, tunnel.ErrTunnelNotFound)) {
		retryPort, shouldRetry, fallbackErr := fallbackDaemonPort(host, daemonPort)
		if fallbackErr != nil {
			return fallbackErr
		}
		if shouldRetry {
			retry = true
			retryErr := postFn(retryPort, host)
			switch {
			case retryErr == nil:
				_ = tunnel.RemoveState(tunnel.DefaultStateDir(), host, retryPort)
				return nil
			case errors.Is(retryErr, errDaemonManagerShuttingDown):
				return fmt.Errorf("daemon is shutting down; retry `cc-clip tunnel remove %s` in a moment", host)
			case !isRecoverableTunnelDownError(retryErr) && !errors.Is(retryErr, tunnel.ErrTunnelNotFound):
				return retryErr
			default:
				err = retryErr
				targetPort = retryPort
			}
		}
	}
	if persistErr := persistFn(host, targetPort); persistErr != nil {
		if errors.Is(persistErr, os.ErrNotExist) {
			if errors.Is(err, tunnel.ErrTunnelNotFound) {
				return nil
			}
			if retry {
				return fmt.Errorf("daemon not reachable on requested port %d or saved port %d; rerun with --port %d or start the daemon", daemonPort, targetPort, targetPort)
			}
			// Daemon class-recoverable error (unreachable / shutting down /
			// tunnel-control unavailable) combined with a missing state file
			// means we cannot confirm the tunnel exists on this machine and
			// have nothing to delete offline. Surface an actionable hint
			// instead of "offline remove failed: file does not exist", which
			// reads as a filesystem error when the real issue is the daemon.
			return fmt.Errorf("daemon not reachable on port %d and no saved state for %s on port %d; start the daemon or verify the host", daemonPort, host, targetPort)
		}
		return fmt.Errorf("%v; offline remove failed: %w", err, persistErr)
	}
	return nil
}

func postTunnelRemove(daemonPort int, host string) error {
	// The daemon refuses zero local_port on /tunnels/remove, so the CLI
	// always sends daemonPort — a persistent tunnel's local_port is
	// structurally the owning daemon's HTTP port (see manager's
	// validateOwnedLocalPort).
	payload := map[string]any{"host": host, "local_port": daemonPort}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("http://127.0.0.1:%d/tunnels/remove", daemonPort)
	// Up/Down/Remove can block on SSH handshake (BatchMode probe, forward
	// bind). The daemon drives the actual SSH asynchronously but the HTTP
	// call still waits for initial ack; 30s is a generous upper bound so
	// slow networks don't return errDaemonUnreachable to the CLI while the
	// daemon is still doing the right thing.
	client := newTunnelControlHTTPClient(30 * time.Second)
	req, authed, err := newDaemonTunnelJSONRequest(http.MethodPost, url, bytes.NewReader(body), false)
	if err != nil {
		return err
	}
	if !authed {
		return fmt.Errorf("%w: local tunnel control token unavailable", errDaemonTunnelControlUnavailable)
	}
	if err := preflightCCClipDaemon(daemonPort); err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w (%v)", errDaemonUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return tunnelControlHTTPError(resp)
	}
	if resp.StatusCode != http.StatusOK {
		return daemonHTTPError(resp)
	}
	return nil
}

func persistTunnelRemoveOffline(host string, localPort int) error {
	return persistTunnelRemoveOfflineWithDir(tunnel.DefaultStateDir(), host, localPort)
}

func persistTunnelRemoveOfflineWithDir(stateDir, host string, localPort int) error {
	// Stop any stale process first so we don't leak a running SSH process when
	// deleting the state file. Missing state is treated as a no-op.
	if err := persistTunnelDownOfflineWithDir(stateDir, host, localPort, offlineDownDeps{}); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := tunnel.RemoveState(stateDir, host, localPort); err != nil {
		return err
	}
	return nil
}

func loadSavedTunnelForUp(host string) (*tunnel.TunnelState, error) {
	s, err := tunnel.LoadStateByHost(tunnel.DefaultStateDir(), host)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return s, nil
}

func validateTunnelPort(name string, port int, allowZero bool) error {
	if allowZero && port == 0 {
		return nil
	}
	if port < 1 || port > maxTunnelPort {
		return fmt.Errorf("%s must be between 1 and %d", name, maxTunnelPort)
	}
	return nil
}

func validateManagedTunnelLocalPort(daemonPort, localPort int) error {
	if daemonPort <= 0 || localPort <= 0 || daemonPort == localPort {
		return nil
	}
	return fmt.Errorf("persistent tunnel local port %d must match daemon port %d", localPort, daemonPort)
}

func daemonPortConfiguredExplicitly() bool {
	_, explicit, err := configuredPort()
	return explicit && err == nil
}

// exitCodeForTunnelError maps well-known tunnel-command error sentinels to
// segmented exit codes in the `exitcode` package. Shell wrappers (SwiftBar,
// retry loops, install scripts) can then act on the code without grepping
// stderr. Anything not classified falls through to exitcode.InternalError.
//
// Ordering matters: errDaemonTunnelControlTokenUnavailable and
// errDaemonTunnelListUnreachable both wrap errDaemonTunnelListUnavailable,
// so the specific arms MUST come before the umbrella arm — otherwise every
// wrapped error would short-circuit into the generic classification.
func exitCodeForTunnelError(err error) int {
	switch {
	case err == nil:
		return exitcode.Success
	case errors.Is(err, errDaemonManagerShuttingDown):
		return exitcode.DaemonShuttingDown
	case errors.Is(err, tunnel.ErrAmbiguousTunnelState):
		return exitcode.AmbiguousTunnelState
	case errors.Is(err, tunnel.ErrTunnelNotFound):
		// Asking about (or down/removing) a tunnel that does not exist
		// on this machine is an operator-input problem — the user named
		// a host that has no saved state and no live forward. Classify
		// it as a usage error so scripted consumers can distinguish
		// "you typed the wrong host" from a genuine runtime failure.
		return exitcode.UsageError
	case errors.Is(err, errDaemonTunnelControlTokenUnavailable),
		errors.Is(err, errDaemonTunnelControlUnavailable):
		// Local tunnel-control token missing/malformed — distinct from
		// "cannot reach daemon". Guides users to `cc-clip serve` / a
		// token rotate without a misleading "tunnel unreachable" hint.
		return exitcode.DaemonTunnelControlTokenUnavailable
	case errors.Is(err, errDaemonTunnelListUnreachable):
		// GET /tunnels returned 404/405: the daemon is up but does not
		// expose tunnel-control routes (older build, mux misconfigured,
		// or a rogue listener squatting on the port). Surface as
		// TunnelUnreachable — the runtime mapping of "I can't reach the
		// tunnel management surface" that retry loops already handle.
		return exitcode.TunnelUnreachable
	case errors.Is(err, errDaemonTunnelListUnavailable):
		// Umbrella wrapper that callers could wrap directly (no sub-
		// sentinel). Treat as the safe default for a failed list probe:
		// the daemon-side cause is unknown. Maps to the token-unavailable
		// code because that is the larger class of things this wrapper
		// used to represent before P2-C split it.
		return exitcode.DaemonTunnelControlTokenUnavailable
	case errors.Is(err, errDaemonUnreachable):
		return exitcode.TunnelUnreachable
	case errors.Is(err, errDaemonAuth):
		return exitcode.TokenInvalid
	default:
		return exitcode.InternalError
	}
}

// fatalWithTunnelExitCode mirrors log.Fatalf's "print to stderr + exit" but
// emits the exit code for the err's sentinel class instead of the default
// Go-runtime 1. Kept narrow — call it from tunnel-subcommand leaf handlers
// where the error path is already fatal and the code knows the error came
// from the tunnel HTTP / state layer.
func fatalWithTunnelExitCode(prefix string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", prefix, err)
	os.Exit(exitCodeForTunnelError(err))
}

// fatalTunnelUsage is the usage-error sibling of fatalWithTunnelExitCode.
// It surfaces operator-input errors (bad --remote-port, missing host arg,
// extra positional) to stderr and exits with exitcode.UsageError so a
// wrapper script can distinguish "you typed it wrong" from a runtime
// classifier failure. The prefix is optional: pass "" to emit the error
// verbatim (used when err already reads as a full usage line, e.g.
// resolveTunnelHostArg's "usage: …" message).
func fatalTunnelUsage(prefix string, err error) {
	if err == nil {
		os.Exit(exitcode.UsageError)
	}
	if prefix == "" {
		fmt.Fprintln(os.Stderr, err)
	} else {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prefix, err)
	}
	os.Exit(exitcode.UsageError)
}

func resolveTunnelHostArg(args []string, index int, usage string, valueFlags ...string) (string, error) {
	if len(args) <= index {
		return "", fmt.Errorf("usage: %s", usage)
	}
	host := strings.TrimSpace(args[index])
	if host == "" {
		return "", fmt.Errorf("usage: %s", usage)
	}
	if strings.HasPrefix(host, "-") {
		return "", fmt.Errorf("host argument must not start with '-' (got %q); place flags after the host\nusage: %s", host, usage)
	}
	// Validate the host locally using the same rules the daemon handler
	// enforces (tunnel.ValidateSSHHost). Without this the CLI would
	// happily round-trip an invalid host to /tunnels/* and surface the
	// daemon's 400 as a generic "daemon returned 400" message. Local
	// rejection gives a clearer error and keeps CLI/handler symmetric.
	if err := tunnel.ValidateSSHHost(host); err != nil {
		return "", fmt.Errorf("%w\nusage: %s", err, usage)
	}
	if err := rejectExtraTunnelPositionals(args, index+1, usage, valueFlags...); err != nil {
		return "", err
	}
	return host, nil
}

func rejectExtraTunnelPositionals(args []string, start int, usage string, valueFlags ...string) error {
	flagsWithValues := make(map[string]struct{}, len(valueFlags))
	for _, flag := range valueFlags {
		flagsWithValues[flag] = struct{}{}
	}
	for i := start; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			if _, ok := flagsWithValues[arg]; ok && i+1 < len(args) {
				i++
			}
			continue
		}
		return fmt.Errorf("unexpected extra argument %q\nusage: %s", arg, usage)
	}
	return nil
}

func isRecoverableTunnelStateError(err error) bool {
	return errors.Is(err, errDaemonUnreachable) || errors.Is(err, errDaemonManagerShuttingDown)
}

func isRecoverableTunnelDownError(err error) bool {
	return isRecoverableTunnelStateError(err) || errors.Is(err, errDaemonTunnelControlUnavailable)
}

func isRecoverableTunnelListError(err error) bool {
	return errors.Is(err, errDaemonUnreachable) ||
		errors.Is(err, errDaemonTunnelControlTokenUnavailable) ||
		errors.Is(err, errDaemonTunnelListUnreachable)
}

func daemonHTTPError(resp *http.Response) error {
	msg := readTunnelErrorBody(resp)
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	// 401 is an auth failure (missing/incorrect token). 403 is the daemon
	// refusing a request it considers non-loopback or non-cc-clip — that is
	// a misconfiguration, not an auth failure, and calling it "auth failed"
	// sends the user chasing the wrong fix. Distinguish the two explicitly.
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: daemon returned %d: %s", errDaemonAuth, resp.StatusCode, msg)
	}
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("daemon returned 403: %s (loopback/User-Agent rejected; not an auth-token issue)", msg)
	}
	if resp.StatusCode == http.StatusServiceUnavailable && strings.Contains(msg, tunnel.ErrManagerShuttingDown.Error()) {
		return fmt.Errorf("%w: daemon returned %d: %s", errDaemonManagerShuttingDown, resp.StatusCode, msg)
	}
	return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, msg)
}

// readTunnelErrorBody extracts a human-readable error message from a daemon
// response body. It transparently handles both structured JSON envelopes
// ({"error": "..."}) emitted by the new handler and legacy plain-text
// bodies from older daemons or httptest stubs, so callers never need to
// branch on content type. Body read is capped at 64 KiB so a buggy or
// compromised mux — or a remote attacker who tricked us into hitting a
// non-daemon listener — cannot stream gigabytes into the CLI. The returned
// value is additionally truncated to maxTunnelErrorMessageChars so a
// valid-but-verbose daemon response does not drown the user's terminal
// (and, importantly, does not splat kilobytes of arbitrary bytes into
// structured logs when the CLI emits it as part of an error message).
func readTunnelErrorBody(resp *http.Response) string {
	msg, _ := readTunnelErrorBodyAndCode(resp)
	return msg
}

// readTunnelErrorBodyAndCode is like readTunnelErrorBody but also surfaces
// the optional structured `code` field emitted alongside `error`. Callers
// that need to distinguish failure kinds (e.g. tunnel-not-found from a
// generic 404) should match on the code instead of string-prefixing the
// human-readable message.
func readTunnelErrorBodyAndCode(resp *http.Response) (string, string) {
	if resp == nil || resp.Body == nil {
		return "", ""
	}
	const maxErrorBody = 64 * 1024
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", ""
	}
	if strings.HasPrefix(trimmed, "{") {
		var envelope struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if err := json.Unmarshal([]byte(trimmed), &envelope); err == nil && envelope.Error != "" {
			return truncateTunnelErrorMessage(envelope.Error), envelope.Code
		}
	}
	return truncateTunnelErrorMessage(trimmed), ""
}

// maxTunnelErrorMessageChars bounds what the CLI is willing to surface from a
// daemon response body. A buggy listener that returns a 64 KiB HTML error
// page would otherwise flood the user's terminal and any downstream log
// collectors. 1024 characters is enough for multi-line diagnostics from the
// tunnel manager without turning into a page of output.
const maxTunnelErrorMessageChars = 1024

func truncateTunnelErrorMessage(s string) string {
	if len(s) <= maxTunnelErrorMessageChars {
		return s
	}
	return s[:maxTunnelErrorMessageChars] + "… (truncated)"
}

// --- HTTP helpers ---

func fetchTunnelList(daemonPort int) ([]*tunnel.TunnelState, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/tunnels", daemonPort)
	client := newTunnelControlHTTPClient(3 * time.Second)
	req, authed, err := newDaemonTunnelJSONRequest(http.MethodGet, url, nil, false)
	if err != nil {
		return nil, err
	}
	if !authed {
		return nil, fmt.Errorf("%w; showing saved state only", errDaemonTunnelControlTokenUnavailable)
	}
	if err := preflightCCClipDaemon(daemonPort); err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", errDaemonUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		msg := readTunnelErrorBody(resp)
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("%w: daemon returned %d: %s", errDaemonTunnelListUnreachable, resp.StatusCode, msg)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, daemonHTTPError(resp)
	}
	// Cap the response body we are willing to decode. The daemon is local
	// but a buggy or compromised mux could still produce an unbounded stream;
	// the list response rarely exceeds a few KiB in practice.
	const maxListBody = 1 << 20 // 1 MiB
	var states []*tunnel.TunnelState
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxListBody)).Decode(&states); err != nil {
		return nil, err
	}
	return states, nil
}

func normalizeOfflineTunnelStates(states []*tunnel.TunnelState) []*tunnel.TunnelState {
	if len(states) == 0 {
		return []*tunnel.TunnelState{}
	}

	out := make([]*tunnel.TunnelState, 0, len(states))
	for _, s := range states {
		if s == nil {
			continue
		}
		cp := *s
		if cp.Config.Enabled {
			cp.Status = tunnel.StatusDisconnected
			cp.PID = 0
		} else {
			cp.Status = tunnel.StatusStopped
			cp.PID = 0
		}
		out = append(out, &cp)
	}
	return out
}

func postTunnelUp(daemonPort int, host string, remotePort int) error {
	// A persistent tunnel's local_port is structurally the owning
	// daemon's HTTP port; the daemon validates this in
	// validateOwnedLocalPort, so we always send daemonPort here.
	payload, _ := json.Marshal(map[string]any{
		"host":        host,
		"remote_port": remotePort,
		"local_port":  daemonPort,
	})
	url := fmt.Sprintf("http://127.0.0.1:%d/tunnels/up", daemonPort)
	// Up/Down/Remove can block on SSH handshake (BatchMode probe, forward
	// bind). The daemon drives the actual SSH asynchronously but the HTTP
	// call still waits for initial ack; 30s is a generous upper bound so
	// slow networks don't return errDaemonUnreachable to the CLI while the
	// daemon is still doing the right thing.
	client := newTunnelControlHTTPClient(30 * time.Second)
	req, _, err := newDaemonTunnelJSONRequest(http.MethodPost, url, bytes.NewReader(payload), true)
	if err != nil {
		return err
	}
	// Preflight after reading the token but before sending the request: we
	// still want missing-token failures to be local-only, but we must not
	// send the bearer token to an unknown listener.
	if err := preflightCCClipDaemon(daemonPort); err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w (%v)", errDaemonUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return tunnelControlHTTPError(resp)
	}
	if resp.StatusCode != http.StatusOK {
		return daemonHTTPError(resp)
	}
	return nil
}

func postTunnelDown(daemonPort int, host string) error {
	// A persistent tunnel's local_port is structurally the owning
	// daemon's HTTP port; the daemon refuses zero on /tunnels/down so
	// the CLI always sends daemonPort.
	payload := map[string]any{"host": host, "local_port": daemonPort}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("http://127.0.0.1:%d/tunnels/down", daemonPort)
	// Up/Down/Remove can block on SSH handshake (BatchMode probe, forward
	// bind). The daemon drives the actual SSH asynchronously but the HTTP
	// call still waits for initial ack; 30s is a generous upper bound so
	// slow networks don't return errDaemonUnreachable to the CLI while the
	// daemon is still doing the right thing.
	client := newTunnelControlHTTPClient(30 * time.Second)
	req, authed, err := newDaemonTunnelJSONRequest(http.MethodPost, url, bytes.NewReader(body), false)
	if err != nil {
		return err
	}
	if !authed {
		return fmt.Errorf("%w: local tunnel control token unavailable", errDaemonTunnelControlUnavailable)
	}
	if err := preflightCCClipDaemon(daemonPort); err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w (%v)", errDaemonUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return tunnelControlHTTPError(resp)
	}
	if resp.StatusCode != http.StatusOK {
		return daemonHTTPError(resp)
	}
	return nil
}

func tunnelControlHTTPError(resp *http.Response) error {
	msg, code := readTunnelErrorBodyAndCode(resp)
	// Prefer the structured code when present: the daemon emits
	// "tunnel-not-found" for ErrTunnelNotFound, so we can classify without
	// string-prefixing the human message. Fall back to the legacy prefix
	// match only when an older daemon (or a plain-text stub) has no code.
	switch {
	case code == tunnelErrCodeNotFound:
		detail := strings.TrimSpace(strings.TrimPrefix(msg, tunnel.ErrTunnelNotFound.Error()))
		detail = strings.TrimPrefix(detail, ":")
		detail = strings.TrimSpace(detail)
		if detail == "" {
			return tunnel.ErrTunnelNotFound
		}
		return fmt.Errorf("%w: %s", tunnel.ErrTunnelNotFound, detail)
	case resp.StatusCode == http.StatusNotFound && strings.HasPrefix(msg, tunnel.ErrTunnelNotFound.Error()):
		// Legacy daemon: no `code` field. Keep the prefix match so a
		// mixed-version pair (old daemon, new CLI) still classifies
		// correctly.
		detail := strings.TrimSpace(strings.TrimPrefix(msg, tunnel.ErrTunnelNotFound.Error()))
		detail = strings.TrimPrefix(detail, ":")
		detail = strings.TrimSpace(detail)
		if detail == "" {
			return tunnel.ErrTunnelNotFound
		}
		return fmt.Errorf("%w: %s", tunnel.ErrTunnelNotFound, detail)
	default:
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("%w: daemon returned %d: %s", errDaemonTunnelControlUnavailable, resp.StatusCode, msg)
	}
}

func stopTunnel(daemonPort int, host string, daemonPortExplicit bool) error {
	return stopTunnelWithRetryPolicy(daemonPort, host, !daemonPortExplicit, postTunnelDown, persistTunnelDownOffline)
}

func stopTunnelWith(
	daemonPort int,
	host string,
	postFn func(int, string) error,
	persistFn func(string, int) error,
) error {
	return stopTunnelWithRetryPolicy(daemonPort, host, true, postFn, persistFn)
}

func stopTunnelWithRetryPolicy(
	daemonPort int,
	host string,
	allowFallback bool,
	postFn func(int, string) error,
	persistFn func(string, int) error,
) error {
	targetPort := daemonPort

	if err := postFn(daemonPort, host); err != nil {
		if !isRecoverableTunnelDownError(err) && !errors.Is(err, tunnel.ErrTunnelNotFound) {
			return err
		}
		retry := false
		retryPort := 0
		if allowFallback && (isRecoverableTunnelDownError(err) || errors.Is(err, tunnel.ErrTunnelNotFound)) {
			var fallbackErr error
			retryPort, retry, fallbackErr = fallbackDaemonPort(host, daemonPort)
			if fallbackErr != nil {
				return fallbackErr
			}
			if retry {
				retryErr := postFn(retryPort, host)
				switch {
				case retryErr == nil:
					return nil
				case errors.Is(retryErr, tunnel.ErrTunnelNotFound):
					err = retryErr
					targetPort = retryPort
				case !isRecoverableTunnelDownError(retryErr) && !errors.Is(retryErr, tunnel.ErrTunnelNotFound):
					return retryErr
				default:
					err = retryErr
					targetPort = retryPort
				}
			}
		}
		if persistErr := persistFn(host, targetPort); persistErr != nil {
			if errors.Is(persistErr, os.ErrNotExist) {
				if errors.Is(err, tunnel.ErrTunnelNotFound) {
					return nil
				}
			}
			if retry && errors.Is(persistErr, os.ErrNotExist) {
				return fmt.Errorf("daemon not reachable on requested port %d or saved port %d; rerun with --port %d or start the daemon", daemonPort, retryPort, retryPort)
			}
			return fmt.Errorf("%v; offline state update failed: %w", err, persistErr)
		}
	}
	return nil
}

// fallbackDaemonPort returns an alternative daemon port to retry against
// when the CLI was pointed at requestedPort but no saved tunnel exists
// there. It inspects the saved-state directory for other tunnels owned by
// the same host and returns the single-candidate port if unambiguous.
//
// INVARIANT: fallbackDaemonPort is only called when `--port` was implicit
// (see stopTunnel/removeTunnel, which gate the fallback branch on
// `!daemonPortExplicit`). If one saved state already uses requestedPort,
// keep targeting that daemon and let the caller continue with offline
// persistence there. Only error when multiple saved ports exist and none
// matches requestedPort — that is the genuinely ambiguous case.
func fallbackDaemonPort(host string, requestedPort int) (int, bool, error) {
	states, err := tunnel.LoadStatesForHost(tunnel.DefaultStateDir(), host)
	if err != nil {
		return 0, false, fmt.Errorf("load saved tunnels for %s: %w", host, err)
	}
	if len(states) == 0 {
		return 0, false, nil
	}

	ports := make([]int, 0, len(states))
	hasRequestedPort := false
	for _, s := range states {
		port := s.Config.LocalPort
		if port <= 0 {
			continue
		}
		ports = append(ports, port)
		if port == requestedPort {
			hasRequestedPort = true
		}
	}

	if len(ports) == 1 {
		if ports[0] == requestedPort {
			return 0, false, nil
		}
		return ports[0], true, nil
	}

	if hasRequestedPort {
		return 0, false, nil
	}
	if len(ports) == 0 {
		return 0, false, fmt.Errorf("saved tunnels for %s have no local_port; re-run `cc-clip connect %s` to rewrite the state file, or pass --port <local-port> explicitly if you know the owning daemon", host, host)
	}

	return 0, false, fmt.Errorf("multiple saved tunnels for %s use local ports %s; rerun with --port <PORT>", host, joinPorts(ports))
}

func joinPorts(ports []int) string {
	if len(ports) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, strconv.Itoa(port))
	}
	return strings.Join(parts, ", ")
}

// offlineDownDeps lets tests stub process lookup/kill without threading
// multiple wrapper levels. Defaults populate in persistTunnelDownOffline.
type offlineDownDeps struct {
	cleanupFn func(int, tunnel.TunnelConfig) error
	findFn    func(tunnel.TunnelConfig) (int, bool, error)
}

func persistTunnelDownOffline(host string, localPort int) error {
	return persistTunnelDownOfflineWithDir(tunnel.DefaultStateDir(), host, localPort, offlineDownDeps{})
}

// persistTunnelDownOfflineWithDir persists a "stopped" state for the saved
// tunnel identified by (host, localPort). If localPort is 0 the caller
// means "the one saved tunnel for this host"; LoadStateByHost is tried
// before LoadState so hosts saved under a different local port than the
// caller's current daemon port are still found.
func persistTunnelDownOfflineWithDir(stateDir, host string, localPort int, deps offlineDownDeps) error {
	if deps.cleanupFn == nil {
		deps.cleanupFn = tunnel.CleanupStaleTunnelProcess
	}
	if deps.findFn == nil {
		deps.findFn = tunnel.FindRunningTunnelProcess
	}

	var (
		s   *tunnel.TunnelState
		err error
	)
	if localPort > 0 {
		s, err = tunnel.LoadState(stateDir, host, localPort)
	} else {
		s, err = tunnel.LoadStateByHost(stateDir, host)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return err
		}
		return fmt.Errorf("cannot persist tunnel state for %s: %w", host, err)
	}
	cleanupPID, err := resolveTunnelCleanupPID(s, deps.findFn)
	if err != nil {
		return fmt.Errorf("cannot locate stale tunnel process for %s: %w", host, err)
	}
	if cleanupPID > 0 {
		if err := deps.cleanupFn(cleanupPID, s.Config); err != nil {
			return fmt.Errorf("cannot stop stale tunnel process for %s: %w", host, err)
		}
	}
	s.Config.Enabled = false
	s.Status = tunnel.StatusStopped
	now := time.Now()
	s.StoppedAt = &now
	s.PID = 0
	s.LastError = ""
	return tunnel.SaveState(stateDir, s)
}

func resolveTunnelCleanupPID(s *tunnel.TunnelState, findFn func(tunnel.TunnelConfig) (int, bool, error)) (int, error) {
	if s == nil {
		return 0, nil
	}
	if s.PID > 0 {
		return s.PID, nil
	}
	if !s.Config.Enabled || findFn == nil {
		return 0, nil
	}
	pid, found, err := findFn(s.Config)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	return pid, nil
}

func newTunnelControlHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// preflightCCClipDaemon runs a short-timeout unauthenticated probe against
// GET /tunnels to confirm the listener on daemonPort is a cc-clip daemon
// before the caller attaches the tunnel-control bearer token. A real
// cc-clip daemon responds 401 to a cc-clip User-Agent without the token.
// Anything else means either an older daemon without tunnel routes or a
// different listener on that port, so the caller refuses to send the token.
//
// No token is attached to this probe. The response body is NOT inspected
// beyond the status code: the purpose is purely "will the listener
// challenge an unauthenticated tunnel-control request the way cc-clip
// does?"
func preflightCCClipDaemon(daemonPort int) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/tunnels", daemonPort)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("preflight: build request: %w", err)
	}
	req.Header.Set("User-Agent", "cc-clip/tunnel-preflight")
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w (%v)", errDaemonUnreachable, err)
	}
	defer resp.Body.Close()
	// 401 is the expected challenge from a real cc-clip daemon when the
	// request has the right User-Agent but omits the local-only tunnel
	// token. 404/405 means the tunnel-control surface is absent, so treat it
	// as the same "tunnel list unreachable" class the real GET /tunnels
	// call would return. Anything else is not a trustworthy cc-clip daemon.
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusMethodNotAllowed:
		msg := readTunnelErrorBody(resp)
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("%w: daemon returned %d: %s", errDaemonTunnelListUnreachable, resp.StatusCode, msg)
	default:
		return fmt.Errorf("daemon on port %d did not respond as cc-clip (HTTP %d on GET /tunnels without auth); refusing to send tunnel-control token to an unknown listener", daemonPort, resp.StatusCode)
	}
}

// newDaemonTunnelJSONRequest builds an authenticated /tunnels request.
// When requireToken is false the caller tolerates a missing token file
// (daemon never ran, rotation in progress) and silently sends unauthenticated
// so the CLI can fall back to offline state updates.
func newDaemonTunnelJSONRequest(method, url string, body io.Reader, requireToken bool) (*http.Request, bool, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, false, err
	}
	tok, err := token.ReadTunnelControlToken()
	switch {
	case err == nil:
		req.Header.Set(tunnelControlAuthHeader, tok)
	case requireToken && errors.Is(err, os.ErrNotExist):
		// ENOENT on the tunnel-control token file almost always means the
		// daemon has never run on this machine (the token is generated on
		// first `cc-clip serve`). Surface that root cause instead of the
		// generic "cannot read token" wording, which makes users think the
		// file is corrupt or permissions are wrong.
		return nil, false, fmt.Errorf("%w: daemon does not appear to be running (tunnel-control token not found); start it with `cc-clip serve` and retry", errDaemonTunnelControlTokenUnavailable)
	case requireToken && token.IsOpaqueTokenInvalid(err):
		return nil, false, fmt.Errorf("%w: daemon tunnel-control token is malformed; restart the daemon with `cc-clip serve --rotate-tunnel-token`", errDaemonTunnelControlTokenUnavailable)
	case requireToken:
		return nil, false, fmt.Errorf("%w: cannot read daemon tunnel control token: %v", errDaemonTunnelControlUnavailable, err)
	case errors.Is(err, os.ErrNotExist), token.IsOpaqueTokenInvalid(err):
		// The token file has not been created yet (daemon never ran / rotation
		// in progress) or is malformed. Silently send unauthenticated so the
		// caller can fall back to offline state updates. Any other read error
		// — permissions, I/O failure — is surfaced because that mode silently
		// dropping auth would hide real problems.
		//
		// For the malformed-token branch specifically, emit a stderr warning
		// so the operator sees that the offline fallback is a DEGRADED mode
		// rather than the normal path; ENOENT (daemon never ran) stays
		// silent because it is a legitimate first-run shape. Matches the
		// `requireToken=true` case's `--rotate-tunnel-token` hint so the
		// user has an obvious fix.
		if token.IsOpaqueTokenInvalid(err) {
			fmt.Fprintf(os.Stderr, "warning: tunnel-control token file is corrupt; showing saved state only (rotate with `cc-clip serve --rotate-tunnel-token`)\n")
		}
		req.Header.Set("User-Agent", "cc-clip/tunnel")
		req.Header.Set("Content-Type", "application/json")
		return req, false, nil
	default:
		return nil, false, fmt.Errorf("cannot read daemon tunnel control token: %w", err)
	}
	req.Header.Set("User-Agent", "cc-clip/tunnel")
	req.Header.Set("Content-Type", "application/json")
	return req, true, nil
}
