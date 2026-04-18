package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shunmei/cc-clip/internal/exitcode"
	"github.com/shunmei/cc-clip/internal/setup"
	"github.com/shunmei/cc-clip/internal/token"
	"github.com/shunmei/cc-clip/internal/tunnel"
)

var errDaemonUnreachable = errors.New("daemon not reachable")
var errDaemonAuth = errors.New("daemon auth failed")
var errDaemonTunnelListUnavailable = errors.New("daemon tunnel list unavailable")
var errDaemonTunnelControlUnavailable = errors.New("daemon tunnel control unavailable")
var errDaemonManagerShuttingDown = errors.New("daemon tunnel manager is shutting down")

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
			os.Exit(1)
		}
		log.Fatalf("tunnel list failed: %v", err)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(states); err != nil {
			log.Fatalf("json encode: %v", err)
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
		log.Fatal(err)
	}
	daemonPort := getPort()
	daemonPortExplicit := daemonPortConfiguredExplicitly()

	remotePort := 0
	if v := getFlag("remote-port", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Fatalf("invalid --remote-port %q: %v", v, err)
		}
		if err := validateTunnelPort("--remote-port", n, false); err != nil {
			log.Fatal(err)
		}
		remotePort = n
	}

	remotePort, daemonPort, err = resolveTunnelUpPorts(host, remotePort, daemonPort, daemonPortExplicit)
	if err != nil {
		log.Fatalf("resolve tunnel ports: %v", err)
	}
	if remotePort == 0 {
		log.Fatalf("cannot determine remote port for %s. Either run `cc-clip connect %s` first (writes the managed SSH config block), or re-run with `--remote-port <PORT>` explicitly.", host, host)
	}
	if err := validateTunnelPort("--port", daemonPort, false); err != nil {
		log.Fatalf("resolve tunnel ports: %v", err)
	}
	if err := validateTunnelPort("--remote-port", remotePort, false); err != nil {
		log.Fatalf("resolve tunnel ports: %v", err)
	}

	if err := postTunnelUp(daemonPort, host, remotePort); err != nil {
		log.Fatalf("tunnel up failed: %v", err)
	}
	fmt.Printf("Tunnel to %s started (remote:%d -> local:%d)\n", host, remotePort, daemonPort)
}

// resolveTunnelUpPorts fills in missing remotePort / daemonPort values from
// the local managed SSH config block, falling back to saved tunnel state.
// When --port was not set explicitly the CLI adopts whatever daemon port
// the managed block (or the unambiguous saved tunnel) points at, so that
// `cc-clip tunnel up <host>` routes to the daemon that actually owns the
// forward for that host. If --port was explicit and diverges from the
// managed/saved value, the call errors rather than silently crossing
// daemons.
func resolveTunnelUpPorts(host string, remotePort, daemonPort int, daemonPortExplicit bool) (int, int, error) {
	var (
		managedPortErr error
		savedPortErr   error
		managedOwned   bool
	)

	managedPorts, err := setup.ReadManagedTunnelPorts(host)
	switch {
	case err == nil && managedPorts.LocalPort > 0:
		managedOwned = true
		switch {
		case !daemonPortExplicit:
			daemonPort = managedPorts.LocalPort
		case daemonPort != managedPorts.LocalPort:
			managedPortErr = fmt.Errorf("managed tunnel for %s uses local port %d (remote port %d); rerun with --port %d to use it, or pass --remote-port %d explicitly", host, managedPorts.LocalPort, managedPorts.RemotePort, managedPorts.LocalPort, managedPorts.RemotePort)
		}
		if remotePort == 0 && managedPorts.RemotePort > 0 {
			remotePort = managedPorts.RemotePort
		}
	case errors.Is(err, setup.ErrManagedRemotePortInvalid):
		return 0, daemonPort, err
	case err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, setup.ErrSSHHostBlockNotFound):
		managedPortErr = err
	}

	if remotePort == 0 || !managedOwned {
		s, err := loadSavedTunnelForUp(host)
		if err != nil {
			if errors.Is(err, tunnel.ErrAmbiguousTunnelState) {
				return 0, daemonPort, fmt.Errorf("%w; pass --port <local-port> to select one, or --remote-port <remote-port> to bypass saved-state lookup", err)
			}
			return 0, daemonPort, err
		}
		if s != nil && s.Config.LocalPort > 0 {
			if !daemonPortExplicit && !managedOwned {
				daemonPort = s.Config.LocalPort
			} else if daemonPortExplicit && daemonPort != s.Config.LocalPort {
				savedPortErr = fmt.Errorf("saved tunnel for %s uses local port %d (remote port %d); rerun with --port %d to use it, or pass --remote-port %d explicitly", host, s.Config.LocalPort, s.Config.RemotePort, s.Config.LocalPort, s.Config.RemotePort)
			}
		}
		if s != nil && remotePort == 0 && s.Config.RemotePort > 0 {
			switch {
			case !daemonPortExplicit:
				remotePort = s.Config.RemotePort
			case daemonPort == s.Config.LocalPort:
				remotePort = s.Config.RemotePort
			default:
				savedPortErr = fmt.Errorf("saved tunnel for %s uses local port %d (remote port %d); rerun with --port %d to use it, or pass --remote-port %d explicitly", host, s.Config.LocalPort, s.Config.RemotePort, s.Config.LocalPort, s.Config.RemotePort)
			}
		}
	}
	if managedPortErr != nil {
		return 0, daemonPort, managedPortErr
	}
	if !managedOwned && savedPortErr != nil {
		return 0, daemonPort, savedPortErr
	}
	return remotePort, daemonPort, nil
}

func cmdTunnelDown() {
	host, err := resolveTunnelHostArg(os.Args, 3, "cc-clip tunnel down <host> [--port PORT]", "--port")
	if err != nil {
		log.Fatal(err)
	}
	daemonPort := getPort()
	daemonPortExplicit := daemonPortConfiguredExplicitly()
	if err := validateTunnelPort("--port", daemonPort, false); err != nil {
		log.Fatal(err)
	}

	if err := stopTunnel(daemonPort, host, daemonPortExplicit); err != nil {
		log.Fatalf("tunnel down failed: %v", err)
	}
	fmt.Printf("Tunnel to %s stopped\n", host)
}

func cmdTunnelRemove() {
	host, err := resolveTunnelHostArg(os.Args, 3, "cc-clip tunnel remove <host> [--port PORT]", "--port")
	if err != nil {
		log.Fatal(err)
	}
	daemonPort := getPort()
	daemonPortExplicit := daemonPortConfiguredExplicitly()
	if err := validateTunnelPort("--port", daemonPort, false); err != nil {
		log.Fatal(err)
	}

	if err := removeTunnel(daemonPort, host, daemonPortExplicit); err != nil {
		log.Fatalf("tunnel remove failed: %v", err)
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

func hasNumericPortFlag(name string) bool {
	value := getFlag(name, "")
	if value == "" {
		return false
	}
	_, err := strconv.Atoi(value)
	return err == nil
}

func hasNumericEnvPort(name string) bool {
	value := os.Getenv(name)
	if value == "" {
		return false
	}
	_, err := strconv.Atoi(value)
	return err == nil
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
	return errors.Is(err, errDaemonUnreachable) || errors.Is(err, errDaemonManagerShuttingDown) || errors.Is(err, errDaemonTunnelListUnavailable)
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
		return nil, fmt.Errorf("%w: local tunnel control token unavailable; showing saved state only", errDaemonTunnelListUnavailable)
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
		return nil, fmt.Errorf("%w: daemon returned %d: %s", errDaemonTunnelListUnavailable, resp.StatusCode, msg)
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
// Ambiguity (multiple saved ports, none matching requestedPort) is
// surfaced as an error instructing the user to pick one via --port.
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
		return nil, false, fmt.Errorf("daemon does not appear to be running (tunnel-control token not found); start it with `cc-clip serve` and retry")
	case requireToken && token.IsOpaqueTokenInvalid(err):
		return nil, false, fmt.Errorf("daemon tunnel-control token is malformed; restart the daemon with `cc-clip serve --rotate-tunnel-token`")
	case requireToken:
		return nil, false, fmt.Errorf("cannot read daemon tunnel control token: %w", err)
	case errors.Is(err, os.ErrNotExist), token.IsOpaqueTokenInvalid(err):
		// The token file has not been created yet (daemon never ran / rotation
		// in progress) or is malformed. Silently send unauthenticated so the
		// caller can fall back to offline state updates. Any other read error
		// — permissions, I/O failure — is surfaced because that mode silently
		// dropping auth would hide real problems.
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
