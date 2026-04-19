package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shunmei/cc-clip/internal/daemon"
	"github.com/shunmei/cc-clip/internal/peer"
	"github.com/shunmei/cc-clip/internal/session"
	"github.com/shunmei/cc-clip/internal/shim"
	"github.com/shunmei/cc-clip/internal/token"
	"github.com/shunmei/cc-clip/internal/tunnel"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	defer r.Close()

	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("Close writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(out)
}

func TestStopLocalProcessDoesNotKillUnexpectedCommand(t *testing.T) {
	cmd := helperSleepProcess(t)
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start helper process: %v", err)
	}

	// Use sync.Once to ensure cmd.Wait() is called exactly once,
	// preventing a data race between the cleanup and the goroutine.
	var waitOnce sync.Once
	var waitErr error
	doWait := func() { waitOnce.Do(func() { waitErr = cmd.Wait() }) }

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		doWait()
	})

	pidFile := filepath.Join(t.TempDir(), "helper.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		t.Fatalf("failed to write pid file: %v", err)
	}

	stopLocalProcess(pidFile, "Xvfb")

	waitDone := make(chan struct{}, 1)
	go func() {
		doWait()
		waitDone <- struct{}{}
	}()

	select {
	case <-waitDone:
		t.Fatalf("unexpected command should still be running, but exited early: %v", waitErr)
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("pid file should be removed after stale pid detection, got err=%v", err)
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	if len(os.Args) < 3 || os.Args[len(os.Args)-1] != "sleep-helper" {
		os.Exit(0)
	}
	// Block on stdin rather than a wall-clock sleep so the parent can
	// terminate the helper deterministically by closing the pipe. Without
	// this, a panic in the parent between setup and t.Cleanup would leave
	// the helper running for the full timeout window.
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func helperSleepProcess(t *testing.T) *exec.Cmd {
	t.Helper()

	args := []string{"-test.run=TestHelperProcess", "--", "sleep-helper"}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	if runtime.GOOS == "windows" {
		cmd.Env = append(cmd.Env, "SystemRoot="+os.Getenv("SystemRoot"))
	}
	// Wire stdin to an anonymous pipe so the parent can force an immediate
	// helper exit by closing it. The pipe is held for the lifetime of the
	// cmd; closing it signals EOF to the helper's `io.Copy`.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	return cmd
}

func TestReleaseVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"0.3.0", "0.3.0"},
		{"0.3.0-1-g99b1298", "0.3.0"},
		{"0.3.0-15-gabcdef0", "0.3.0"},
		{"1.0.0-rc1", "1.0.0-rc1"},              // pre-release tag, not git describe
		{"1.0.0-rc1-3-g1234567", "1.0.0-rc1"},   // git describe from pre-release tag
		{"0.3.0-beta-2-gabcdef0", "0.3.0-beta"}, // git describe from tag with dash
		{"dev", "dev"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := releaseVersion(tt.input)
			if got != tt.want {
				t.Errorf("releaseVersion(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNotifyFromCodexParsesLastAssistantMessage(t *testing.T) {
	payload := `{"last-assistant-message":"Bridge implementation complete"}`
	msg, err := parseCodexNotifyPayload(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Body != "Bridge implementation complete" {
		t.Fatalf("unexpected body %q", msg.Body)
	}
	if msg.Title != "Codex" {
		t.Fatalf("expected title %q, got %q", "Codex", msg.Title)
	}
}

func TestNotifyFromCodexRejectsInvalidJSON(t *testing.T) {
	_, err := parseCodexNotifyPayload(`{invalid`)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestNotifyFromCodexHandlesEmptyMessage(t *testing.T) {
	payload := `{"last-assistant-message":""}`
	msg, err := parseCodexNotifyPayload(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Body != "" {
		t.Fatalf("expected empty body, got %q", msg.Body)
	}
}

func TestNotifyFromCodexHandlesMissingField(t *testing.T) {
	payload := `{"some-other-field":"value"}`
	msg, err := parseCodexNotifyPayload(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Missing field => empty body
	if msg.Body != "" {
		t.Fatalf("expected empty body for missing field, got %q", msg.Body)
	}
}

func TestParseCodexNotifyPayloadReturnType(t *testing.T) {
	payload := `{"last-assistant-message":"test"}`
	msg, err := parseCodexNotifyPayload(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Verify the return type is GenericMessagePayload
	var _ daemon.GenericMessagePayload = msg
}

func TestRegisterNonceWithDaemonIntegration(t *testing.T) {
	tm := token.NewManager(time.Hour)
	sess, err := tm.Generate()
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	port := extractPort(t, ts.URL)
	testNonce := "test-nonce-0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab"

	if err := registerNonceWithDaemon(port, sess.Token, testNonce); err != nil {
		t.Fatalf("registerNonceWithDaemon failed: %v", err)
	}

	// Verify the nonce works by sending a health probe
	if err := runNotificationHealthProbe(port, testNonce); err != nil {
		t.Fatalf("health probe failed after nonce registration: %v", err)
	}
}

func TestRunNotificationHealthProbeFailsWithBadNonce(t *testing.T) {
	tm := token.NewManager(time.Hour)
	_, _ = tm.Generate()
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	port := extractPort(t, ts.URL)

	err := runNotificationHealthProbe(port, "bad-nonce")
	if err == nil {
		t.Fatal("expected health probe to fail with unregistered nonce")
	}
}

func TestRunRemoteNotificationHealthProbeUsesStrictRemoteHookPath(t *testing.T) {
	session := &fixedRemoteExecutor{}

	if err := runRemoteNotificationHealthProbe(session, 19001, "~/.cache/cc-clip/peers/peer-a"); err != nil {
		t.Fatalf("runRemoteNotificationHealthProbe: %v", err)
	}
	if len(session.commands) != 1 {
		t.Fatalf("expected one remote command, got %v", session.commands)
	}

	cmd := session.commands[0]
	for _, needle := range []string{
		`printf %s '{"hook_event_name":"notification","type":"idle_prompt","title":"cc-clip","body":"Notification bridge connected"}'`,
		`CC_CLIP_PORT=19001`,
		`CC_CLIP_STATE_DIR="$HOME/.cache/cc-clip/peers/peer-a"`,
		`CC_CLIP_STRICT=1`,
		`"$HOME/.local/bin/cc-clip-hook"`,
	} {
		if !strings.Contains(cmd, needle) {
			t.Fatalf("expected remote health probe to contain %q, got %q", needle, cmd)
		}
	}
}

func TestRunRemoteNotificationHealthProbeSurfacesRemoteFailure(t *testing.T) {
	session := &fixedRemoteExecutor{
		out: "cc-clip-hook health probe failed: http=000",
		err: errors.New("exit status 1"),
	}

	err := runRemoteNotificationHealthProbe(session, 19001, "~/.cache/cc-clip/peers/peer-a")
	if err == nil {
		t.Fatal("expected remote health probe failure")
	}
	if !strings.Contains(err.Error(), "cc-clip-hook health probe failed") {
		t.Fatalf("err = %v, want full remote probe prefix", err)
	}
	if !strings.Contains(err.Error(), "http=000") {
		t.Fatalf("err = %v, want remote probe details", err)
	}
	if len(session.commands) != 1 {
		t.Fatalf("expected one remote command, got %v", session.commands)
	}
}

// TestRunRemoteNotificationHealthProbeRejectsEmptyStateDir pins the new
// contract that an empty stateDir is a programming error — the probe's
// purpose is to exercise the run's specific nonce via that peer-scoped
// directory. Falling back to a generic `~/.cache/cc-clip` silently targeted
// the wrong nonce file and produced a misleading "health probe failed".
func TestRunRemoteNotificationHealthProbeRejectsEmptyStateDir(t *testing.T) {
	session := &fixedRemoteExecutor{}
	err := runRemoteNotificationHealthProbe(session, 19001, "")
	if err == nil {
		t.Fatal("expected error for empty stateDir, got nil")
	}
	if len(session.commands) != 0 {
		t.Fatalf("expected no remote commands to be issued, got %v", session.commands)
	}
}

func TestRunShimUninstallWithHostSkipsLocalShim(t *testing.T) {
	localCalled := false
	remoteHost := ""

	err := runShimUninstall(shim.TargetXclip, "/tmp/local-bin", "dev-success-cc-clip-wavehi-local", uninstallOps{
		uninstallLocalShim: func(target shim.Target, installPath string) error {
			localCalled = true
			return nil
		},
		removeRemotePath: func(host string) error {
			remoteHost = host
			return nil
		},
	})
	if err != nil {
		t.Fatalf("runShimUninstall returned error: %v", err)
	}
	if localCalled {
		t.Fatal("expected local shim uninstall to be skipped when --host is set")
	}
	if remoteHost != "dev-success-cc-clip-wavehi-local" {
		t.Fatalf("expected remote cleanup host to be used, got %q", remoteHost)
	}
}

func TestRunShimUninstallWithoutHostRemovesLocalShim(t *testing.T) {
	localCalled := false
	remoteCalled := false

	err := runShimUninstall(shim.TargetWlPaste, "/tmp/local-bin", "", uninstallOps{
		uninstallLocalShim: func(target shim.Target, installPath string) error {
			localCalled = true
			if target != shim.TargetWlPaste {
				t.Fatalf("unexpected target: %v", target)
			}
			if installPath != "/tmp/local-bin" {
				t.Fatalf("unexpected install path: %q", installPath)
			}
			return nil
		},
		removeRemotePath: func(host string) error {
			remoteCalled = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("runShimUninstall returned error: %v", err)
	}
	if !localCalled {
		t.Fatal("expected local shim uninstall to run when --host is not set")
	}
	if remoteCalled {
		t.Fatal("expected remote PATH cleanup to be skipped without --host")
	}
}

func TestPostGenericNotificationDeliversExpectedPayload(t *testing.T) {
	tm := token.NewManager(time.Hour)
	_, err := tm.Generate()
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	nonce := "test-notify-nonce-0123456789abcdef0123456789abcdef0123456789abcdef"
	srv.RegisterNotificationNonce(nonce)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	port := extractPort(t, ts.URL)

	home := t.TempDir()
	cacheDir := filepath.Join(home, ".cache", "cc-clip")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "notify.nonce"), []byte(nonce+"\n"), 0600); err != nil {
		t.Fatalf("failed to write nonce file: %v", err)
	}

	oldHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("failed to set HOME: %v", err)
	}
	defer func() {
		_ = os.Setenv("HOME", oldHome)
	}()

	msg := daemon.GenericMessagePayload{
		Title:   "Build complete",
		Body:    "All tests passed",
		Urgency: 1,
	}
	if err := postGenericNotification(port, msg); err != nil {
		t.Fatalf("postGenericNotification failed: %v", err)
	}

	select {
	case env := <-srv.NotifyChannel():
		if env.GenericMessage == nil {
			t.Fatal("expected GenericMessage payload")
		}
		if env.GenericMessage.Title != msg.Title {
			t.Fatalf("expected title %q, got %q", msg.Title, env.GenericMessage.Title)
		}
		if env.GenericMessage.Body != msg.Body {
			t.Fatalf("expected body %q, got %q", msg.Body, env.GenericMessage.Body)
		}
		if env.GenericMessage.Urgency != msg.Urgency {
			t.Fatalf("expected urgency %d, got %d", msg.Urgency, env.GenericMessage.Urgency)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected notification to be enqueued")
	}
}

func TestClaudeHookConfigJSONIncludesNotificationAndStop(t *testing.T) {
	cfg := claudeHookConfigJSON()
	if !strings.Contains(cfg, `"Notification"`) {
		t.Fatalf("expected Notification hook in config, got %q", cfg)
	}
	if !strings.Contains(cfg, `"Stop"`) {
		t.Fatalf("expected Stop hook in config, got %q", cfg)
	}
	if strings.Count(cfg, `"command": "cc-clip-hook"`) != 2 {
		t.Fatalf("expected hook command to appear twice, got %q", cfg)
	}

	// Each event entry must use the current matcher-wrapped schema
	// ({"hooks":[{type:command,...}]}), not the legacy flat form
	// ({type:command,...}) that older Claude Code versions accepted.
	var parsed struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatalf("claudeHookConfigJSON not valid JSON: %v\n%s", err, cfg)
	}
	for _, event := range []string{"Stop", "Notification"} {
		entries, ok := parsed.Hooks[event]
		if !ok || len(entries) == 0 {
			t.Fatalf("%s missing from hooks: %q", event, cfg)
		}
		if len(entries[0].Hooks) == 0 || entries[0].Hooks[0].Command != "cc-clip-hook" {
			t.Fatalf("%s entry not matcher-wrapped with cc-clip-hook command: %q", event, cfg)
		}
	}
}

func TestResolveUninstallPeerTargetUsesExplicitPeerID(t *testing.T) {
	ident := peer.Identity{ID: "local-peer", Label: "macbook"}

	peerID, managedHost, err := resolveUninstallPeerTarget("myserver", "other-peer", ident)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if peerID != "other-peer" {
		t.Fatalf("expected explicit peer ID to be preserved, got %q", peerID)
	}
	if managedHost != "" {
		t.Fatalf("expected explicit peer ID cleanup to skip local SSH cleanup, got host=%q", managedHost)
	}
}

func TestResolveUninstallPeerTargetRequiresHost(t *testing.T) {
	ident := peer.Identity{ID: "local-peer", Label: "macbook"}

	_, _, err := resolveUninstallPeerTarget("", "local-peer", ident)
	if err == nil {
		t.Fatal("expected missing host to be rejected")
	}
	if !strings.Contains(err.Error(), "--host is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveUninstallPeerTargetUsesAliasForLocalPeerID(t *testing.T) {
	ident := peer.Identity{ID: "local-peer", Label: "macbook"}

	peerID, managedHost, err := resolveUninstallPeerTarget("myserver", "local-peer", ident)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if peerID != "local-peer" {
		t.Fatalf("expected local peer ID to be preserved, got %q", peerID)
	}
	if managedHost != "myserver" {
		t.Fatalf("unexpected managed host %q", managedHost)
	}
}

func TestParsePeerRegistrationEmptyOutput(t *testing.T) {
	reg, err := parsePeerRegistration("   \n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg != nil {
		t.Fatalf("expected nil registration for empty output, got %#v", reg)
	}
}

func TestParsePeerRegistrationJSON(t *testing.T) {
	reg, err := parsePeerRegistration(`{"peer_id":"peer-a","label":"macbook","reserved_port":18340,"state_dir":"~/.cache/cc-clip/peers/peer-a"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg == nil {
		t.Fatal("expected registration")
	}
	if reg.PeerID != "peer-a" || reg.ReservedPort != 18340 {
		t.Fatalf("unexpected registration: %#v", reg)
	}
}

func TestEnsureRemotePeerRegistrySupportAcceptsPeerAwareBinary(t *testing.T) {
	session := &recordingRemoteExecutor{
		responses: map[string]remoteExecResponse{
			"~/.local/bin/cc-clip peer 2>&1 || true": {
				out: "usage: cc-clip peer <reserve|release|show> [flags]\n",
			},
		},
	}

	if err := ensureRemotePeerRegistrySupport(session, "~/.local/bin/cc-clip"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureRemotePeerRegistrySupportRejectsLegacyBinary(t *testing.T) {
	session := &recordingRemoteExecutor{
		responses: map[string]remoteExecResponse{
			"~/.local/bin/cc-clip peer 2>&1 || true": {
				out: "unknown command: peer\n",
			},
		},
	}

	err := ensureRemotePeerRegistrySupport(session, "~/.local/bin/cc-clip")
	if err == nil {
		t.Fatal("expected legacy remote binary to be rejected")
	}
	if !strings.Contains(err.Error(), "re-run without --token-only") {
		t.Fatalf("expected actionable redeploy hint, got %v", err)
	}
}

func TestResolveTokenOnlyPeerReservationReturnsExistingReservation(t *testing.T) {
	reg, err := resolveTokenOnlyPeerReservation(&peer.Registration{
		PeerID:       "peer-a",
		ReservedPort: 19001,
		StateDir:     "~/.cache/cc-clip/peers/peer-a",
	}, nil)
	if err != nil {
		t.Fatalf("resolveTokenOnlyPeerReservation: %v", err)
	}
	if reg.ReservedPort != 19001 {
		t.Fatalf("ReservedPort = %d, want 19001", reg.ReservedPort)
	}
	if reg.PeerID != "peer-a" {
		t.Fatalf("PeerID = %q, want %q", reg.PeerID, "peer-a")
	}
	if reg.StateDir != "~/.cache/cc-clip/peers/peer-a" {
		t.Fatalf("StateDir = %q, want %q", reg.StateDir, "~/.cache/cc-clip/peers/peer-a")
	}
}

func TestResolveTokenOnlyPeerReservationFallsBackToLegacyStateDir(t *testing.T) {
	reg, err := resolveTokenOnlyPeerReservation(&peer.Registration{
		PeerID:       "peer-a",
		ReservedPort: 19001,
	}, nil)
	if err != nil {
		t.Fatalf("resolveTokenOnlyPeerReservation: %v", err)
	}
	if reg.StateDir != legacyPeerStateDir("peer-a") {
		t.Fatalf("StateDir = %q, want %q", reg.StateDir, legacyPeerStateDir("peer-a"))
	}
}

func TestResolveTokenOnlyPeerReservationRejectsMissingReservation(t *testing.T) {
	_, err := resolveTokenOnlyPeerReservation(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "re-run without --token-only") {
		t.Fatalf("err = %v, want actionable missing-reservation error", err)
	}
}

func TestResolveTokenOnlyPeerReservationRejectsLookupFailure(t *testing.T) {
	_, err := resolveTokenOnlyPeerReservation(nil, errors.New("remote lookup failed"))
	if err == nil || !strings.Contains(err.Error(), "look up existing peer reservation") {
		t.Fatalf("err = %v, want lookup failure", err)
	}
}

func TestSaveConnectTunnelStatePersistsHostPortsForEnabledTunnel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := saveConnectTunnelState("myserver", 18339, 19001, true); err != nil {
		t.Fatalf("saveConnectTunnelState: %v", err)
	}

	s, err := tunnel.LoadStateByHost(tunnel.DefaultStateDir(), "myserver")
	if err != nil {
		t.Fatalf("LoadStateByHost: %v", err)
	}
	if s == nil {
		t.Fatal("expected state, got nil")
	}
	if s.Config.LocalPort != 18339 || s.Config.RemotePort != 19001 {
		t.Fatalf("ports = %d/%d, want 18339/19001", s.Config.LocalPort, s.Config.RemotePort)
	}
	if !s.Config.Enabled {
		t.Fatal("Config.Enabled = false, want true")
	}
	if s.Config.SSHConfigResolved {
		t.Fatal("SSHConfigResolved = true, want false (let /tunnels/up re-resolve)")
	}
	if s.Status != tunnel.StatusConnecting {
		t.Fatalf("Status = %q, want connecting", s.Status)
	}
}

// TestSaveConnectTunnelStateIsIdempotentForReconnectOfConnectedTunnel pins
// the contract that re-running `cc-clip connect` on a host whose tunnel is
// already `connected` (same LocalPort + same RemotePort + Enabled=true)
// does NOT downgrade the persisted Status to `connecting`. The SwiftBar
// plugin and `cc-clip tunnel list` both render directly from the persisted
// Status, so a regression here would briefly flap a healthy tunnel back to
// "connecting" in the UI on every reconnect.
//
// The live-field-preservation branch in saveConnectTunnelState is gated
// on all three matching; the inverse cases (enabled=false, different
// RemotePort) are pinned by sibling tests below.
func TestSaveConnectTunnelStateIsIdempotentForReconnectOfConnectedTunnel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Seed an existing connected tunnel state for this host.
	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "myserver",
			LocalPort:  18339,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
		PID:    4242,
	}); err != nil {
		t.Fatalf("seed SaveState: %v", err)
	}

	// Simulate a re-run of `cc-clip connect myserver`.
	if err := saveConnectTunnelState("myserver", 18339, 19001, true); err != nil {
		t.Fatalf("saveConnectTunnelState: %v", err)
	}

	s, err := tunnel.LoadStateByHost(tunnel.DefaultStateDir(), "myserver")
	if err != nil {
		t.Fatalf("LoadStateByHost: %v", err)
	}
	if s == nil {
		t.Fatal("expected state to survive re-save, got nil")
	}
	if s.Status != tunnel.StatusConnected {
		t.Fatalf("Status = %q, want %q (re-running connect must not flap connected→connecting)",
			s.Status, tunnel.StatusConnected)
	}
}

func TestSaveConnectTunnelStatePersistsStoppedWhenNoTunnelRequested(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := saveConnectTunnelState("myserver", 18339, 19001, false); err != nil {
		t.Fatalf("saveConnectTunnelState: %v", err)
	}

	s, err := tunnel.LoadStateByHost(tunnel.DefaultStateDir(), "myserver")
	if err != nil {
		t.Fatalf("LoadStateByHost: %v", err)
	}
	if s.Config.Enabled {
		t.Fatal("Config.Enabled = true, want false for --no-tunnel")
	}
	if s.Status != tunnel.StatusStopped {
		t.Fatalf("Status = %q, want stopped", s.Status)
	}
}

func TestCleanupCreatedTokenOnlyFallbackRemovesTunnelStateAndReleasesPeer(t *testing.T) {
	calls := []string{}
	err := cleanupCreatedTokenOnlyFallback("myserver", 18444, tokenOnlyFallbackCleanupOps{
		removePersistentTunnel: func(host string, lp int) error {
			calls = append(calls, fmt.Sprintf("tunnel:%s:%d", host, lp))
			return nil
		},
		releasePeer: func() error {
			calls = append(calls, "release")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("cleanupCreatedTokenOnlyFallback: %v", err)
	}
	if got, want := strings.Join(calls, ","), "tunnel:myserver:18444,release"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestCleanupCreatedTokenOnlyFallbackTreatsMissingTunnelStateAsNoOp(t *testing.T) {
	calls := []string{}
	err := cleanupCreatedTokenOnlyFallback("myserver", 18444, tokenOnlyFallbackCleanupOps{
		removePersistentTunnel: func(string, int) error {
			calls = append(calls, "tunnel")
			return os.ErrNotExist
		},
		releasePeer: func() error {
			calls = append(calls, "release")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("cleanupCreatedTokenOnlyFallback: %v", err)
	}
	if got, want := strings.Join(calls, ","), "tunnel,release"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestCleanupCreatedTokenOnlyFallbackJoinsRollbackErrors(t *testing.T) {
	err := cleanupCreatedTokenOnlyFallback("myserver", 18444, tokenOnlyFallbackCleanupOps{
		removePersistentTunnel: func(string, int) error { return errors.New("tunnel cleanup failed") },
		releasePeer:            func() error { return errors.New("release failed") },
	})
	if err == nil {
		t.Fatal("expected rollback error")
	}
	if !strings.Contains(err.Error(), "tunnel cleanup failed") {
		t.Fatalf("err = %v, want tunnel cleanup failure", err)
	}
	if !strings.Contains(err.Error(), "release failed") {
		t.Fatalf("err = %v, want release failure", err)
	}
}

func TestConnectActivateTunnelStopsStartedTunnelWhenRemoteVerificationFails(t *testing.T) {
	var calls []string

	err := connectActivateTunnelWithOps(nil, connectOpts{}, "myserver", 18339, 19001, connectActivateTunnelOps{
		startPersistentTunnel: func(localPort int, host string, remotePort int) error {
			calls = append(calls, fmt.Sprintf("start:%s:%d:%d", host, localPort, remotePort))
			return nil
		},
		stopPersistentTunnel: func(localPort int, host string) error {
			calls = append(calls, fmt.Sprintf("stop:%s:%d", host, localPort))
			return nil
		},
		remoteStatus: func(*shim.SSHSession, string) (string, error) {
			return "status failed", errors.New("boom")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "remote binary verification failed: status failed") {
		t.Fatalf("err = %v, want remote verification failure", err)
	}
	if got, want := strings.Join(calls, ","), "start:myserver:18339:19001,stop:myserver:18339"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

// TestConnectActivateTunnelHappyPathLeavesTunnelRunning pins the positive
// branch: a successful start + successful remote status check must NOT
// invoke the stop callback. Without this test the rollback path
// (TestConnectActivateTunnelStopsStartedTunnelWhenRemoteVerificationFails)
// could regress to always stopping and only the failure case would catch it.
func TestConnectActivateTunnelHappyPathLeavesTunnelRunning(t *testing.T) {
	var calls []string

	err := connectActivateTunnelWithOps(nil, connectOpts{}, "myserver", 18339, 19001, connectActivateTunnelOps{
		startPersistentTunnel: func(localPort int, host string, remotePort int) error {
			calls = append(calls, fmt.Sprintf("start:%s:%d:%d", host, localPort, remotePort))
			return nil
		},
		stopPersistentTunnel: func(localPort int, host string) error {
			calls = append(calls, fmt.Sprintf("stop:%s:%d", host, localPort))
			return nil
		},
		remoteStatus: func(*shim.SSHSession, string) (string, error) {
			return "cc-clip ok", nil
		},
	})
	if err != nil {
		t.Fatalf("connectActivateTunnelWithOps: %v", err)
	}
	if got, want := strings.Join(calls, ","), "start:myserver:18339:19001"; got != want {
		t.Fatalf("calls = %q, want only the start call (no stop on happy path)", got)
	}
}

// TestConnectActivateTunnelNoTunnelSkipsStart pins --no-tunnel: when the
// operator opts out of daemon-managed start, neither startPersistentTunnel
// nor stopPersistentTunnel must be invoked, and remote verification must
// still run so a broken remote binary surfaces immediately.
func TestConnectActivateTunnelNoTunnelSkipsStart(t *testing.T) {
	var calls []string

	err := connectActivateTunnelWithOps(nil, connectOpts{noTunnel: true}, "myserver", 18339, 19001, connectActivateTunnelOps{
		startPersistentTunnel: func(int, string, int) error {
			calls = append(calls, "start")
			return nil
		},
		stopPersistentTunnel: func(int, string) error {
			calls = append(calls, "stop")
			return nil
		},
		remoteStatus: func(*shim.SSHSession, string) (string, error) {
			calls = append(calls, "status")
			return "cc-clip ok", nil
		},
	})
	if err != nil {
		t.Fatalf("connectActivateTunnelWithOps: %v", err)
	}
	if got, want := strings.Join(calls, ","), "status"; got != want {
		t.Fatalf("calls = %q, want only %q (start/stop must not run with --no-tunnel)", got, want)
	}
}

// TestConnectActivateTunnelStopsPendingTunnelWhenStartFails pins the new
// post-`postTunnelUp` failure contract: if the daemon accepted the request
// but the subsequent `/tunnels` poll timed out (startPersistentTunnel
// returns error), we MUST call stopPersistentTunnel so the daemon does not
// keep a phantom ssh subprocess alive. Without this, failAfterSave removes
// the local state file while the manager re-persists it on the next tick.
func TestConnectActivateTunnelStopsPendingTunnelWhenStartFails(t *testing.T) {
	var calls []string
	err := connectActivateTunnelWithOps(nil, connectOpts{}, "myserver", 18339, 19001, connectActivateTunnelOps{
		startPersistentTunnel: func(int, string, int) error {
			calls = append(calls, "start")
			return errors.New("poll timeout")
		},
		stopPersistentTunnel: func(int, string) error {
			calls = append(calls, "stop")
			return nil
		},
		remoteStatus: func(*shim.SSHSession, string) (string, error) {
			calls = append(calls, "status")
			return "", nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "failed to start persistent tunnel") {
		t.Fatalf("err = %v, want start-failure error", err)
	}
	if got, want := strings.Join(calls, ","), "start,stop"; got != want {
		t.Fatalf("calls = %q, want %q (stop must fire after start failure; status must NOT run)", got, want)
	}
}

// TestConnectActivateTunnelWrapsStopErrorAfterVerificationFailure covers
// the "tunnel started + remote verify failed + stop also failed" branch so
// a future refactor that accidentally swallows stopErr is caught.
func TestConnectActivateTunnelWrapsStopErrorAfterVerificationFailure(t *testing.T) {
	err := connectActivateTunnelWithOps(nil, connectOpts{}, "myserver", 18339, 19001, connectActivateTunnelOps{
		startPersistentTunnel: func(int, string, int) error { return nil },
		stopPersistentTunnel:  func(int, string) error { return errors.New("cannot reach daemon") },
		remoteStatus:          func(*shim.SSHSession, string) (string, error) { return "status boom", errors.New("exec err") },
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "remote binary verification failed") {
		t.Fatalf("err = %v, want verification failure wrapped", err)
	}
	if !strings.Contains(err.Error(), "additionally failed to stop started tunnel: cannot reach daemon") {
		t.Fatalf("err = %v, want stop error surfaced in wrap", err)
	}
}

// TestConnectStartPersistentTunnelWithReturnsOnFirstConnected pins the
// happy path of the poll loop: once /tunnels reports connected for the
// target (host, daemonPort), the call returns nil without further polling.
func TestConnectStartPersistentTunnelWithReturnsOnFirstConnected(t *testing.T) {
	// Shrink the poll interval so the test runs instantly even if the first
	// fetch returned `connecting`.
	oldInterval := connectTunnelUpPollInterval
	connectTunnelUpPollInterval = time.Millisecond
	t.Cleanup(func() { connectTunnelUpPollInterval = oldInterval })

	polls := 0
	fetch := func(int) ([]*tunnel.TunnelState, error) {
		polls++
		s := &tunnel.TunnelState{
			Config: tunnel.TunnelConfig{Host: "myserver", LocalPort: 18339, RemotePort: 19001},
			Status: tunnel.StatusConnecting,
		}
		if polls >= 2 {
			s.Status = tunnel.StatusConnected
		}
		return []*tunnel.TunnelState{s}, nil
	}
	if err := connectStartPersistentTunnelWith(18339, "myserver", 19001,
		func(int, string, int) error { return nil }, fetch); err != nil {
		t.Fatalf("connectStartPersistentTunnelWith: %v", err)
	}
	if polls < 2 {
		t.Fatalf("polls = %d, want >= 2 (should have seen connecting then connected)", polls)
	}
}

// TestConnectStartPersistentTunnelWithTimesOutAndReportsStatus pins the
// timeout path: when /tunnels never shows connected, the returned error
// must include the last observed status so operators can debug.
func TestConnectStartPersistentTunnelWithTimesOutAndReportsStatus(t *testing.T) {
	oldTimeout := connectTunnelUpTimeout
	oldInterval := connectTunnelUpPollInterval
	connectTunnelUpTimeout = 30 * time.Millisecond
	connectTunnelUpPollInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		connectTunnelUpTimeout = oldTimeout
		connectTunnelUpPollInterval = oldInterval
	})

	fetch := func(int) ([]*tunnel.TunnelState, error) {
		return []*tunnel.TunnelState{{
			Config:    tunnel.TunnelConfig{Host: "myserver", LocalPort: 18339, RemotePort: 19001},
			Status:    tunnel.StatusConnecting,
			LastError: "permission denied",
		}}, nil
	}
	err := connectStartPersistentTunnelWith(18339, "myserver", 19001,
		func(int, string, int) error { return nil }, fetch)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "did not reach connected state") {
		t.Fatalf("err = %v, want timeout message", err)
	}
	if !strings.Contains(err.Error(), "status=connecting") {
		t.Fatalf("err = %v, want last status in error", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want last daemon error surfaced", err)
	}
}

// TestConnectStartPersistentTunnelWithFastFailsOnOtherDaemonPort covers the
// "daemon owns this host but on a different local port" branch: the poll
// must NOT burn the full 15s; it must surface a diagnostic suffix telling
// the operator where the host actually lives.
func TestConnectStartPersistentTunnelWithFastFailsOnOtherDaemonPort(t *testing.T) {
	oldTimeout := connectTunnelUpTimeout
	oldInterval := connectTunnelUpPollInterval
	connectTunnelUpTimeout = time.Second
	connectTunnelUpPollInterval = time.Millisecond
	t.Cleanup(func() {
		connectTunnelUpTimeout = oldTimeout
		connectTunnelUpPollInterval = oldInterval
	})

	fetch := func(int) ([]*tunnel.TunnelState, error) {
		return []*tunnel.TunnelState{{
			// Same host, different LocalPort.
			Config: tunnel.TunnelConfig{Host: "myserver", LocalPort: 18444, RemotePort: 19001},
			Status: tunnel.StatusConnected,
		}}, nil
	}
	start := time.Now()
	err := connectStartPersistentTunnelWith(18339, "myserver", 19001,
		func(int, string, int) error { return nil }, fetch)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected mismatch error")
	}
	if elapsed >= connectTunnelUpTimeout {
		t.Fatalf("poll did not fast-fail: took %s >= timeout %s", elapsed, connectTunnelUpTimeout)
	}
	if !strings.Contains(err.Error(), "no tunnel owned by daemon on port 18339") {
		t.Fatalf("err = %v, want diagnostic suffix mentioning wrong daemon port", err)
	}
	if !strings.Contains(err.Error(), "myserver@18444->19001=connected") {
		t.Fatalf("err = %v, want summary of actually-owning daemon", err)
	}
}

// TestSummarizeTunnelStatesForHostRendersEveryMatch pins the compact
// summary format used in connect error messages. A refactor that flips
// field order or drops status would silently degrade diagnostics.
func TestSummarizeTunnelStatesForHostRendersEveryMatch(t *testing.T) {
	states := []*tunnel.TunnelState{
		nil,
		{Config: tunnel.TunnelConfig{Host: "other", LocalPort: 18339, RemotePort: 19001}, Status: tunnel.StatusConnected},
		{Config: tunnel.TunnelConfig{Host: "myserver", LocalPort: 18339, RemotePort: 19001}, Status: tunnel.StatusConnected},
		{Config: tunnel.TunnelConfig{Host: "MyServer", LocalPort: 18444, RemotePort: 19002}, Status: tunnel.StatusConnecting},
	}
	got := summarizeTunnelStatesForHost(states, "myserver")
	want := "myserver@18339->19001=connected, MyServer@18444->19002=connecting"
	if got != want {
		t.Fatalf("summarize = %q, want %q", got, want)
	}
}

func TestSummarizeTunnelStatesForHostReturnsEmptyWhenNoMatch(t *testing.T) {
	got := summarizeTunnelStatesForHost([]*tunnel.TunnelState{
		{Config: tunnel.TunnelConfig{Host: "other", LocalPort: 18339, RemotePort: 19001}, Status: tunnel.StatusConnected},
	}, "myserver")
	if got != "" {
		t.Fatalf("summarize = %q, want empty string so caller can omit suffix", got)
	}
}

// TestSaveConnectTunnelStateEnabledFalseDoesNotPreserveConnectedStatus
// pins the invariant that `cc-clip connect --no-tunnel` (enabled=false)
// must not inherit Status=connected from a previously-connected run.
// Otherwise LoadAndStartAll skips Enabled=false entries while `tunnel
// list` still shows the host as connected — internally inconsistent.
func TestSaveConnectTunnelStateEnabledFalseDoesNotPreserveConnectedStatus(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{Host: "myserver", LocalPort: 18339, RemotePort: 19001, Enabled: true},
		Status: tunnel.StatusConnected,
		PID:    4242,
	}); err != nil {
		t.Fatalf("seed SaveState: %v", err)
	}
	if err := saveConnectTunnelState("myserver", 18339, 19001, false); err != nil {
		t.Fatalf("saveConnectTunnelState: %v", err)
	}
	s, err := tunnel.LoadStateByHost(tunnel.DefaultStateDir(), "myserver")
	if err != nil {
		t.Fatalf("LoadStateByHost: %v", err)
	}
	if s == nil {
		t.Fatal("expected state to survive, got nil")
	}
	if s.Config.Enabled {
		t.Fatal("Config.Enabled = true, want false for --no-tunnel")
	}
	if s.Status != tunnel.StatusStopped {
		t.Fatalf("Status = %q, want stopped (must not preserve connected when enabled=false)", s.Status)
	}
	if s.PID != 0 {
		t.Fatalf("PID = %d, want 0 (must not preserve stale PID when enabled=false)", s.PID)
	}
}

// TestSaveConnectTunnelStateRemotePortChangeResetsRuntimeFields pins the
// invariant that re-running connect with a NEW RemotePort must reset
// Status→connecting and clear PID/StartedAt — otherwise a stale PID
// pointing at the old forward survives.
func TestSaveConnectTunnelStateRemotePortChangeResetsRuntimeFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{Host: "myserver", LocalPort: 18339, RemotePort: 19001, Enabled: true},
		Status: tunnel.StatusConnected,
		PID:    4242,
	}); err != nil {
		t.Fatalf("seed SaveState: %v", err)
	}
	if err := saveConnectTunnelState("myserver", 18339, 29999, true); err != nil {
		t.Fatalf("saveConnectTunnelState: %v", err)
	}
	s, err := tunnel.LoadStateByHost(tunnel.DefaultStateDir(), "myserver")
	if err != nil {
		t.Fatalf("LoadStateByHost: %v", err)
	}
	if s.Config.RemotePort != 29999 {
		t.Fatalf("RemotePort = %d, want 29999", s.Config.RemotePort)
	}
	if s.Status != tunnel.StatusConnecting {
		t.Fatalf("Status = %q, want connecting (remote port changed)", s.Status)
	}
	if s.PID != 0 {
		t.Fatalf("PID = %d, want 0 (remote port changed, stale PID must clear)", s.PID)
	}
}

func TestParseRemoteCodexStateDirsDeduplicatesAndSkipsBlankLines(t *testing.T) {
	got := parseRemoteCodexStateDirs(strings.Join([]string{
		"",
		"~/.cache/cc-clip/peers/peer-a/codex",
		"~/.cache/cc-clip/peers/peer-b/codex",
		"~/.cache/cc-clip/peers/peer-a/codex",
		"",
	}, "\n"))
	if len(got) != 2 {
		t.Fatalf("expected 2 unique state dirs, got %v", got)
	}
	if got[0] != "~/.cache/cc-clip/peers/peer-a/codex" {
		t.Fatalf("unexpected first state dir %q", got[0])
	}
	if got[1] != "~/.cache/cc-clip/peers/peer-b/codex" {
		t.Fatalf("unexpected second state dir %q", got[1])
	}
}

func TestRemoteCodexStateDirsIncludesAllPeerStateDirectories(t *testing.T) {
	session := &recordingRemoteExecutor{
		responses: map[string]remoteExecResponse{
			`find "$HOME/.cache/cc-clip/peers" -mindepth 2 -maxdepth 2 -type d -name codex -print 2>/dev/null`: {
				out: strings.Join([]string{
					"~/.cache/cc-clip/peers/peer-a/codex",
					"~/.cache/cc-clip/peers/peer-b/codex",
					"",
				}, "\n"),
			},
		},
	}

	got := remoteCodexStateDirs(session)
	want := []string{
		legacyCodexStateDir,
		"~/.cache/cc-clip/peers/peer-a/codex",
		"~/.cache/cc-clip/peers/peer-b/codex",
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected state dirs %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("state dirs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTargetRemoteCodexStateDirUsesPeerStateWhenPresent(t *testing.T) {
	reg := &peer.Registration{StateDir: "~/.cache/cc-clip/peers/peer-a"}

	if got := targetRemoteCodexStateDir(reg); got != "~/.cache/cc-clip/peers/peer-a/codex" {
		t.Fatalf("unexpected peer codex state dir %q", got)
	}
}

func TestTargetRemoteCodexStateDirFallsBackToLegacy(t *testing.T) {
	if got := targetRemoteCodexStateDir(nil); got != legacyCodexStateDir {
		t.Fatalf("expected legacy codex state dir %q, got %q", legacyCodexStateDir, got)
	}
}

func TestCodexCleanupStateDirsIncludesLegacyAndPeerState(t *testing.T) {
	reg := &peer.Registration{StateDir: "~/.cache/cc-clip/peers/peer-a"}

	got := codexCleanupStateDirs(reg)
	want := []string{
		legacyCodexStateDir,
		"~/.cache/cc-clip/peers/peer-a/codex",
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected cleanup state dirs %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cleanup state dirs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCodexCleanupStateDirsDeduplicatesLegacyFallback(t *testing.T) {
	got := codexCleanupStateDirs(nil)
	if len(got) != 1 || got[0] != legacyCodexStateDir {
		t.Fatalf("expected only legacy codex state dir, got %v", got)
	}
}

func TestCompatStateDirsIncludesPeerAndLegacyState(t *testing.T) {
	got := compatStateDirs("~/.cache/cc-clip/peers/peer-a")
	want := []string{"~/.cache/cc-clip/peers/peer-a", legacyStateDir}
	if len(got) != len(want) {
		t.Fatalf("unexpected compat state dirs %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("compat state dirs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCompatStateDirsDeduplicatesLegacyState(t *testing.T) {
	got := compatStateDirs(legacyStateDir)
	if len(got) != 1 || got[0] != legacyStateDir {
		t.Fatalf("expected only legacy state dir, got %v", got)
	}
}

func TestShimShellQuoteExpandsHomeRelativePaths(t *testing.T) {
	got := shimShellQuote("~/.cache/cc-clip/codex")
	if got != `"$HOME/.cache/cc-clip/codex"` {
		t.Fatalf("unexpected quoted path %q", got)
	}
}

func TestCleanupPeerRemoteStateStopsCodexProcessesBeforeRemovingStateDir(t *testing.T) {
	session := &recordingRemoteExecutor{
		responses: map[string]remoteExecResponse{
			`cat "$HOME/.cache/cc-clip/peers/peer-a/codex/xvfb.pid" 2>/dev/null`: {
				out: "123\n",
			},
			"ps -p 123 -o comm= 2>/dev/null": {
				out: "Xvfb\n",
			},
		},
	}

	if err := cleanupPeerRemoteState(session, "~/.cache/cc-clip/peers/peer-a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bridgeStop := session.indexOfCommandContaining("bridge.pid")
	xvfbRead := session.indexOfCommandContaining("codex/xvfb.pid")
	stateRemove := session.indexOfCommandContaining(`rm -rf "$HOME/.cache/cc-clip/peers/peer-a"`)

	if bridgeStop < 0 {
		t.Fatalf("expected bridge stop command, got %v", session.commands)
	}
	if xvfbRead < 0 {
		t.Fatalf("expected Xvfb stop command, got %v", session.commands)
	}
	if stateRemove < 0 {
		t.Fatalf("expected peer state removal command, got %v", session.commands)
	}
	if stateRemove < bridgeStop || stateRemove < xvfbRead {
		t.Fatalf("expected state removal after stopping Codex runtime, got %v", session.commands)
	}
}

func TestBridgeConfiguredForPort(t *testing.T) {
	session := &recordingRemoteExecutor{
		responses: map[string]remoteExecResponse{
			`cat "$HOME/.cache/cc-clip/peers/peer-a/codex/bridge.port" 2>/dev/null`: {
				out: "18340\n",
			},
		},
	}

	if !bridgeConfiguredForPort(session, "~/.cache/cc-clip/peers/peer-a/codex", 18340) {
		t.Fatal("expected bridge port match")
	}
	if bridgeConfiguredForPort(session, "~/.cache/cc-clip/peers/peer-a/codex", 18341) {
		t.Fatal("expected bridge port mismatch")
	}
}

func TestStartBridgeRemoteWritesBridgePortFile(t *testing.T) {
	session := &recordingRemoteExecutor{}

	if err := startBridgeRemote(session, "42", 18340, "~/.cache/cc-clip/peers/peer-a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(session.commands) != 1 {
		t.Fatalf("expected one remote command, got %v", session.commands)
	}
	if !strings.Contains(session.commands[0], `printf '18340\n' > "$HOME/.cache/cc-clip/peers/peer-a/codex/bridge.port"`) {
		t.Fatalf("expected bridge start command to persist configured port, got %q", session.commands[0])
	}
}

func TestStartBridgeRemoteQuotesStateDirPaths(t *testing.T) {
	session := &recordingRemoteExecutor{}
	stateDir := "/tmp/cc clip/peer-a"

	if err := startBridgeRemote(session, "42", 18340, stateDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(session.commands) != 1 {
		t.Fatalf("expected one remote command, got %v", session.commands)
	}
	for _, needle := range []string{
		"CC_CLIP_STATE_DIR='/tmp/cc clip/peer-a'",
		"> '/tmp/cc clip/peer-a/codex/bridge.log' 2>&1",
		"echo $! > '/tmp/cc clip/peer-a/codex/bridge.pid'",
		"printf '18340\\n' > '/tmp/cc clip/peer-a/codex/bridge.port'",
		"cat '/tmp/cc clip/peer-a/codex/bridge.pid' 2>/dev/null",
	} {
		if !strings.Contains(session.commands[0], needle) {
			t.Fatalf("expected bridge start command to contain %q, got %q", needle, session.commands[0])
		}
	}
}

func TestConnectNotifyDisableRemovesManagedAssets(t *testing.T) {
	session := &recordingRemoteExecutor{
		responses: map[string]remoteExecResponse{
			"head -5 ~/.local/bin/cc-clip-hook 2>/dev/null || true": {
				out: "#!/usr/bin/env bash\n# cc-clip-hook — Claude Code hook bridge\n",
			},
			"head -5 ~/.local/bin/clipcc 2>/dev/null || true": {
				out: "#!/usr/bin/env bash\n# cc-clip clipcc wrapper — auto-inject notification hooks\n",
			},
		},
	}

	if err := connectNotifyDisable(session, "~/.cache/cc-clip/peers/peer-a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, needle := range []string{
		`rm -f "$HOME/.cache/cc-clip/peers/peer-a/notify.nonce" "$HOME/.cache/cc-clip/peers/peer-a/notify-health.log"`,
		`rm -f "$HOME/.cache/cc-clip/notify.nonce" "$HOME/.cache/cc-clip/notify-health.log"`,
		`find "$HOME/.cache/cc-clip/peers" -mindepth 2 -maxdepth 2 \( -name 'notify.nonce' -o -name 'notify-health.log' \) -delete 2>/dev/null || true`,
		`rm -f "$HOME/.local/bin/cc-clip-hook"`,
		`rm -f "$HOME/.local/bin/clipcc"`,
		`sed -i.cc-clip-bak '/# >>> cc-clip notify \(do not edit\) >>>/,/# <<< cc-clip notify \(do not edit\) <<</d' ~/.codex/config.toml 2>/dev/null || true; rm -f ~/.codex/config.toml.cc-clip-bak`,
	} {
		if session.indexOfCommandContaining(needle) < 0 {
			t.Fatalf("expected command containing %q, got %v", needle, session.commands)
		}
	}
}

func TestConnectNotifyDisableLeavesUserClipCCWrapperUntouched(t *testing.T) {
	session := &recordingRemoteExecutor{
		responses: map[string]remoteExecResponse{
			"head -5 ~/.local/bin/cc-clip-hook 2>/dev/null || true": {
				out: "#!/usr/bin/env bash\n# cc-clip-hook — Claude Code hook bridge\n",
			},
			"head -5 ~/.local/bin/clipcc 2>/dev/null || true": {
				out: "#!/usr/bin/env bash\n# user-managed wrapper\n",
			},
		},
	}

	if err := connectNotifyDisable(session, "~/.cache/cc-clip/peers/peer-a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session.indexOfCommandContaining(`rm -f "$HOME/.local/bin/clipcc"`) >= 0 {
		t.Fatalf("expected user-managed clipcc wrapper to be left alone, got %v", session.commands)
	}
}

func TestConnectNotifyDisableDoesNotTouchClaudeBinary(t *testing.T) {
	session := &recordingRemoteExecutor{
		responses: map[string]remoteExecResponse{
			"head -5 ~/.local/bin/cc-clip-hook 2>/dev/null || true": {
				out: "#!/usr/bin/env bash\n# cc-clip-hook — Claude Code hook bridge\n",
			},
		},
	}

	if err := connectNotifyDisable(session, "~/.cache/cc-clip/peers/peer-a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, cmd := range session.commands {
		if strings.Contains(cmd, "~/.local/bin/claude") {
			t.Fatalf("expected teardown to leave claude untouched, got %v", session.commands)
		}
	}
}

func TestUninstallPeerRemoteAndConfigSkipsTunnelCleanupWhenManagedHostEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	tunnelCalls := 0
	err := uninstallPeerRemoteAndConfigWithOps("", func() (*peer.Registration, error) {
		return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
	}, uninstallPeerCleanupOps{
		removePersistentTunnel: func(string, int) error {
			tunnelCalls++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tunnelCalls != 0 {
		t.Fatalf("removePersistentTunnel called %d times, want 0 when managedHost is empty", tunnelCalls)
	}
}

func TestUninstallPeerRemoteAndConfigRemovesPersistentTunnelState(t *testing.T) {
	calls := []string{}

	err := uninstallPeerRemoteAndConfigWithOps("myserver", func() (*peer.Registration, error) {
		return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
	}, uninstallPeerCleanupOps{
		removePersistentTunnel: func(host string, localPort int) error {
			calls = append(calls, fmt.Sprintf("tunnel:%s:%d", host, localPort))
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := strings.Join(calls, ","), "tunnel:myserver:0"; got != want {
		t.Fatalf("cleanup order = %q, want %q", got, want)
	}
}

func TestUninstallPeerRemoteAndConfigSurfacesTunnelCleanupErrors(t *testing.T) {
	err := uninstallPeerRemoteAndConfigWithOps("myserver", func() (*peer.Registration, error) {
		return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
	}, uninstallPeerCleanupOps{
		removePersistentTunnel: func(string, int) error {
			return errors.New("tunnel cleanup failed")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "persistent tunnel") {
		t.Fatalf("err = %v, want tunnel cleanup failure", err)
	}
}

func TestRemovePersistentTunnelWithStopsOfflineAndRemovesStateWhenDaemonUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	postCalls := 0
	persistCalls := 0
	removeCalls := 0

	err := removePersistentTunnelWith("myserver", 18339,
		func(_ int, host string) error {
			postCalls++
			if host != "myserver" {
				t.Fatalf("host = %q, want myserver", host)
			}
			return fmt.Errorf("%w (dial tcp 127.0.0.1:18339: connect: connection refused)", errDaemonUnreachable)
		},
		func(host string, localPort int) error {
			persistCalls++
			if host != "myserver" {
				t.Fatalf("host = %q, want myserver", host)
			}
			if localPort != 18339 {
				t.Fatalf("localPort = %d, want 18339", localPort)
			}
			return nil
		},
		func(stateDir, host string, localPort int) error {
			removeCalls++
			if host != "myserver" {
				t.Fatalf("host = %q, want myserver", host)
			}
			if stateDir != tunnel.DefaultStateDir() {
				t.Fatalf("stateDir = %q, want %q", stateDir, tunnel.DefaultStateDir())
			}
			if localPort != 18339 {
				t.Fatalf("localPort = %d, want 18339", localPort)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if postCalls != 1 {
		t.Fatalf("postCalls = %d, want 1", postCalls)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
	if removeCalls != 1 {
		t.Fatalf("removeCalls = %d, want 1", removeCalls)
	}
}

func TestRemovePersistentTunnelWithKeepsStateWhenOfflineStopFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	persistCalls := 0
	removeCalls := 0

	err := removePersistentTunnelWith("myserver", 18339,
		func(_ int, _ string) error {
			return fmt.Errorf("%w (dial tcp 127.0.0.1:18339: connect: connection refused)", errDaemonUnreachable)
		},
		func(host string, localPort int) error {
			persistCalls++
			if host != "myserver" {
				t.Fatalf("host = %q, want myserver", host)
			}
			if localPort != 18339 {
				t.Fatalf("localPort = %d, want 18339", localPort)
			}
			return errors.New("still running")
		},
		func(string, string, int) error {
			removeCalls++
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "stop persistent tunnel offline") {
		t.Fatalf("err = %v, want offline stop error", err)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
	if removeCalls != 0 {
		t.Fatalf("removeCalls = %d, want 0", removeCalls)
	}
}

func TestRemovePersistentTunnelWithFallsBackOfflineWhenTunnelControlRouteIsUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	postCalls := 0
	persistCalls := 0
	removeCalls := 0

	err := removePersistentTunnelWith("myserver", 18339,
		func(_ int, host string) error {
			postCalls++
			if host != "myserver" {
				t.Fatalf("host = %q, want myserver", host)
			}
			return fmt.Errorf("%w: daemon returned 404: Not Found", errDaemonTunnelControlUnavailable)
		},
		func(host string, localPort int) error {
			persistCalls++
			if host != "myserver" {
				t.Fatalf("host = %q, want myserver", host)
			}
			if localPort != 18339 {
				t.Fatalf("localPort = %d, want 18339", localPort)
			}
			return nil
		},
		func(string, string, int) error {
			removeCalls++
			return nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if postCalls != 1 {
		t.Fatalf("postCalls = %d, want 1", postCalls)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
	if removeCalls != 1 {
		t.Fatalf("removeCalls = %d, want 1", removeCalls)
	}
}

func TestRemovePersistentTunnelWithReturnsAuthFailureWithoutOfflineFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	persistCalls := 0
	removeCalls := 0

	err := removePersistentTunnelWith("myserver", 18339,
		func(port int, _ string) error {
			if port != 18339 {
				t.Fatalf("port = %d, want 18339", port)
			}
			return fmt.Errorf("%w: daemon returned 401: missing authorization", errDaemonAuth)
		},
		func(string, int) error {
			persistCalls++
			return nil
		},
		func(_ string, _ string, _ int) error {
			removeCalls++
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "stop persistent tunnel") {
		t.Fatalf("err = %v, want daemon auth failure", err)
	}
	if persistCalls != 0 {
		t.Fatalf("persistCalls = %d, want 0", persistCalls)
	}
	if removeCalls != 0 {
		t.Fatalf("removeCalls = %d, want 0", removeCalls)
	}
}

func TestRemovePersistentTunnelWithIgnoresMissingTunnelInDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	removeCalls := 0

	err := removePersistentTunnelWith("myserver", 18339,
		func(_ int, _ string) error {
			return fmt.Errorf("%w: tunnel myserver not found", tunnel.ErrTunnelNotFound)
		},
		func(string, int) error {
			t.Fatal("persistFn should not be called when daemon reports missing tunnel")
			return nil
		},
		func(string, string, int) error {
			removeCalls++
			return nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if removeCalls != 1 {
		t.Fatalf("removeCalls = %d, want 1", removeCalls)
	}
}

func TestRemovePersistentTunnelWithDoesNotRetryDifferentSavedPort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupLocalOnlyTokenDir(t)

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "myserver",
			LocalPort:  18444,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var ports []int
	persistCalls := 0
	removeCalls := 0

	err := removePersistentTunnelWith("myserver", 18339,
		func(port int, _ string) error {
			ports = append(ports, port)
			return fmt.Errorf("%w (dial tcp 127.0.0.1:%d: connect: connection refused)", errDaemonUnreachable, port)
		},
		func(host string, localPort int) error {
			persistCalls++
			if host != "myserver" || localPort != 18339 {
				t.Fatalf("offline stop = (%q, %d), want (%q, %d)", host, localPort, "myserver", 18339)
			}
			return os.ErrNotExist
		},
		func(_ string, _ string, localPort int) error {
			removeCalls++
			if localPort != 18339 {
				t.Fatalf("localPort = %d, want 18339", localPort)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got, want := fmt.Sprint(ports), "[18339]"; got != want {
		t.Fatalf("ports = %s, want %s", got, want)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
	if removeCalls != 1 {
		t.Fatalf("removeCalls = %d, want 1", removeCalls)
	}
}

func TestRemovePersistentTunnelRemovesOnlyRequestedPortForHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupLocalOnlyTokenDir(t)

	ports := []int{18444, 18555}
	for _, port := range ports {
		if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
			Config: tunnel.TunnelConfig{
				Host:       "myserver",
				LocalPort:  port,
				RemotePort: 19001,
				Enabled:    true,
			},
			Status: tunnel.StatusConnected,
		}); err != nil {
			t.Fatalf("SaveState(%d): %v", port, err)
		}
	}

	if err := removePersistentTunnel("myserver", ports[0]); err != nil {
		t.Fatalf("removePersistentTunnel: %v", err)
	}

	if _, err := tunnel.LoadState(tunnel.DefaultStateDir(), "myserver", ports[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadState(%d) err = %v, want os.ErrNotExist", ports[0], err)
	}
	if _, err := tunnel.LoadState(tunnel.DefaultStateDir(), "myserver", ports[1]); err != nil {
		t.Fatalf("LoadState(%d): %v", ports[1], err)
	}
}

func TestRemovePersistentTunnelRemovesAllSavedPortsForHostWhenPortUnknown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupLocalOnlyTokenDir(t)

	ports := []int{18444, 18555}
	for _, port := range ports {
		if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
			Config: tunnel.TunnelConfig{
				Host:       "myserver",
				LocalPort:  port,
				RemotePort: 19001 + (port - ports[0]),
				Enabled:    true,
			},
			Status: tunnel.StatusConnected,
		}); err != nil {
			t.Fatalf("SaveState(%d): %v", port, err)
		}
	}

	if err := removePersistentTunnel("myserver", 0); err != nil {
		t.Fatalf("removePersistentTunnel: %v", err)
	}

	for _, port := range ports {
		if _, err := tunnel.LoadState(tunnel.DefaultStateDir(), "myserver", port); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("LoadState(%d) err = %v, want os.ErrNotExist", port, err)
		}
	}
}

type recordingRemoteExecutor struct {
	commands  []string
	responses map[string]remoteExecResponse
}

// fixedRemoteExecutor returns the SAME (out, err) response for EVERY Exec
// call — intended for one-shot tests (e.g. a single health probe). Use
// recordingRemoteExecutor if you need per-command responses.
type fixedRemoteExecutor struct {
	commands []string
	out      string
	err      error
}

type remoteExecResponse struct {
	out string
	err error
}

func (r *recordingRemoteExecutor) Exec(cmd string) (string, error) {
	r.commands = append(r.commands, cmd)
	if resp, ok := r.responses[cmd]; ok {
		return resp.out, resp.err
	}
	return "", nil
}

func (r *fixedRemoteExecutor) Exec(cmd string) (string, error) {
	r.commands = append(r.commands, cmd)
	return r.out, r.err
}

func (r *recordingRemoteExecutor) indexOfCommandContaining(substr string) int {
	for i, cmd := range r.commands {
		if strings.Contains(cmd, substr) {
			return i
		}
	}
	return -1
}

// testClipboard is a minimal mock for daemon.ClipboardReader.
type testClipboard struct{}

func (c *testClipboard) Type() (daemon.ClipboardInfo, error) {
	return daemon.ClipboardInfo{Type: daemon.ClipboardEmpty}, nil
}

func (c *testClipboard) ImageBytes() ([]byte, error) {
	return nil, nil
}

// extractPort extracts the port number from an httptest server URL.
func extractPort(t *testing.T, url string) int {
	t.Helper()
	// URL format: http://127.0.0.1:PORT
	parts := strings.Split(url, ":")
	if len(parts) < 3 {
		t.Fatalf("unexpected URL format: %s", url)
	}
	port, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatalf("failed to parse port from URL %s: %v", url, err)
	}
	return port
}

func TestListenerPortUsesBoundEphemeralPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	want := ln.Addr().(*net.TCPAddr).Port
	got := listenerPort(ln, 0)
	if got != want {
		t.Fatalf("listenerPort = %d, want %d", got, want)
	}
}

func TestIsNumeric(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"0", true},
		{"123", true},
		{"", false},
		{"abc", false},
		{"12a", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isNumeric(tt.input)
			if got != tt.want {
				t.Errorf("isNumeric(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestConfiguredPortFlagWinsOverEnv pins the precedence contract: an explicit
// --port argument must beat CC_CLIP_PORT. Otherwise scripts that pass --port
// target the wrong daemon whenever an unrelated CC_CLIP_PORT is exported.
func TestConfiguredPortFlagWinsOverEnv(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		env      string
		envSet   bool
		wantPort int
		wantExpl bool
	}{
		{name: "flag only", args: []string{"cc-clip", "--port", "18500"}, wantPort: 18500, wantExpl: true},
		{name: "env only", args: []string{"cc-clip"}, env: "18400", envSet: true, wantPort: 18400, wantExpl: true},
		{name: "flag beats env", args: []string{"cc-clip", "--port", "18500"}, env: "18400", envSet: true, wantPort: 18500, wantExpl: true},
		{name: "neither", args: []string{"cc-clip"}, wantPort: 18339, wantExpl: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			savedArgs := os.Args
			os.Args = tt.args
			t.Cleanup(func() { os.Args = savedArgs })
			if tt.envSet {
				t.Setenv("CC_CLIP_PORT", tt.env)
			} else {
				os.Unsetenv("CC_CLIP_PORT")
			}
			got, explicit, err := configuredPort()
			if err != nil {
				t.Fatalf("configuredPort err = %v", err)
			}
			if got != tt.wantPort {
				t.Errorf("port = %d, want %d", got, tt.wantPort)
			}
			if explicit != tt.wantExpl {
				t.Errorf("explicit = %v, want %v", explicit, tt.wantExpl)
			}
		})
	}
}
