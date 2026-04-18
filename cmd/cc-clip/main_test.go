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
	"github.com/shunmei/cc-clip/internal/setup"
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

func TestEnsureManagedHostConfigForReservationUsesExistingReservation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	configPath := filepath.Join(sshDir, "config")
	initial := strings.Join([]string{
		"Host myserver",
		"    HostName 10.0.0.1",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(initial), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	changes, err := ensureManagedHostConfigForReservation("myserver", 18444, &peer.Registration{
		PeerID:       "peer-a",
		ReservedPort: 19001,
		StateDir:     "~/.cache/cc-clip/peers/peer-a",
	})
	if err != nil {
		t.Fatalf("ensureManagedHostConfigForReservation: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != "created" {
		t.Fatalf("changes = %+v, want one created change", changes)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(content)
	if strings.Count(got, "Host myserver") != 1 {
		t.Fatalf("expected config to reuse one Host myserver stanza, got:\n%s", got)
	}
	if strings.Count(got, "# >>> cc-clip managed host: myserver >>>") != 1 || strings.Count(got, "# <<< cc-clip managed host: myserver <<<") != 1 {
		t.Fatalf("expected one managed host fragment, got:\n%s", got)
	}
	if !strings.Contains(got, "RemoteForward 19001 127.0.0.1:18444") {
		t.Fatalf("expected managed RemoteForward to be updated, got:\n%s", got)
	}
}

func TestCleanupCreatedTokenOnlyFallbackRemovesManagedConfigAndReleasesPeer(t *testing.T) {
	calls := []string{}
	err := cleanupCreatedTokenOnlyFallback("myserver", tokenOnlyFallbackCleanupOps{
		removeManagedHostConfig: func(host string) error {
			calls = append(calls, "config:"+host)
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
	if got, want := strings.Join(calls, ","), "config:myserver,release"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestCleanupCreatedTokenOnlyFallbackJoinsRollbackErrors(t *testing.T) {
	err := cleanupCreatedTokenOnlyFallback("myserver", tokenOnlyFallbackCleanupOps{
		removeManagedHostConfig: func(string) error { return errors.New("config cleanup failed") },
		releasePeer:             func() error { return errors.New("release failed") },
	})
	if err == nil {
		t.Fatal("expected rollback error")
	}
	if !strings.Contains(err.Error(), "config cleanup failed") {
		t.Fatalf("err = %v, want config cleanup failure", err)
	}
	if !strings.Contains(err.Error(), "release failed") {
		t.Fatalf("err = %v, want release failure", err)
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

func TestUninstallPeerRemoteAndConfigRemovesHostFragmentWhenRemoteReleaseFails(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", home)
	defer func() { _ = os.Setenv("HOME", oldHome) }()

	configPath := filepath.Join(home, ".ssh", "config")
	config := strings.Join([]string{
		"Host myserver",
		"    HostName myserver",
		"    # >>> cc-clip managed host: myserver >>>",
		"    RemoteForward 18340 127.0.0.1:18339",
		"    # <<< cc-clip managed host: myserver <<<",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		t.Fatal(err)
	}

	err := uninstallPeerRemoteAndConfig("myserver", func() (*peer.Registration, error) {
		return nil, fmt.Errorf("peer peer-a not found")
	})
	if err == nil {
		t.Fatal("expected remote cleanup failure")
	}

	content, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(content), "cc-clip managed host: myserver") {
		t.Fatalf("expected managed host fragment to be removed, got:\n%s", string(content))
	}
	if !strings.Contains(string(content), "Host myserver") {
		t.Fatalf("expected Host block to remain, got:\n%s", string(content))
	}
}

func TestUninstallPeerRemoteAndConfigSkipsLocalCleanupWhenTargetsEmpty(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", home)
	defer func() { _ = os.Setenv("HOME", oldHome) }()

	configPath := filepath.Join(home, ".ssh", "config")
	config := strings.Join([]string{
		"Host myserver",
		"    HostName myserver",
		"",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		t.Fatal(err)
	}

	if err := uninstallPeerRemoteAndConfig("", func() (*peer.Registration, error) {
		return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(content), "Host myserver") {
		t.Fatalf("expected Host block to remain untouched, got:\n%s", string(content))
	}
}

func TestUninstallPeerRemoteAndConfigRemovesPersistentTunnelState(t *testing.T) {
	calls := []string{}

	err := uninstallPeerRemoteAndConfigWithOps("myserver", func() (*peer.Registration, error) {
		return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
	}, uninstallPeerCleanupOps{
		readManagedTunnelPorts: func(host string) (setup.ManagedTunnelPorts, error) {
			calls = append(calls, "ports:"+host)
			return setup.ManagedTunnelPorts{RemotePort: 18340, LocalPort: 18339}, nil
		},
		removeManagedHostConfig: func(host string) error {
			calls = append(calls, "config:"+host)
			return nil
		},
		removePersistentTunnel: func(host string, localPort int) error {
			calls = append(calls, fmt.Sprintf("tunnel:%s:%d", host, localPort))
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := strings.Join(calls, ","), "ports:myserver,tunnel:myserver:18339,config:myserver"; got != want {
		t.Fatalf("cleanup order = %q, want %q", got, want)
	}
}

func TestUninstallPeerRemoteAndConfigFallsBackToHostWideTunnelCleanupWhenManagedPortsInvalid(t *testing.T) {
	calls := []string{}

	err := uninstallPeerRemoteAndConfigWithOps("myserver", func() (*peer.Registration, error) {
		return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
	}, uninstallPeerCleanupOps{
		readManagedTunnelPorts: func(host string) (setup.ManagedTunnelPorts, error) {
			calls = append(calls, "ports:"+host)
			return setup.ManagedTunnelPorts{}, fmt.Errorf("%w for %s: shared Host stanzas are not supported", setup.ErrManagedRemotePortInvalid, host)
		},
		removeManagedHostConfig: func(host string) error {
			calls = append(calls, "config:"+host)
			return nil
		},
		removePersistentTunnel: func(host string, localPort int) error {
			calls = append(calls, fmt.Sprintf("tunnel:%s:%d", host, localPort))
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := strings.Join(calls, ","), "ports:myserver,tunnel:myserver:0,config:myserver"; got != want {
		t.Fatalf("cleanup order = %q, want %q", got, want)
	}
}

func TestUninstallPeerRemoteAndConfigFallsBackToHostWideTunnelCleanupWhenManagedPortsMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "config file missing", err: os.ErrNotExist},
		{name: "host block missing", err: setup.ErrSSHHostBlockNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := []string{}

			err := uninstallPeerRemoteAndConfigWithOps("myserver", func() (*peer.Registration, error) {
				return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
			}, uninstallPeerCleanupOps{
				readManagedTunnelPorts: func(host string) (setup.ManagedTunnelPorts, error) {
					calls = append(calls, "ports:"+host)
					return setup.ManagedTunnelPorts{}, tc.err
				},
				removeManagedHostConfig: func(host string) error {
					calls = append(calls, "config:"+host)
					return nil
				},
				removePersistentTunnel: func(host string, localPort int) error {
					calls = append(calls, fmt.Sprintf("tunnel:%s:%d", host, localPort))
					return nil
				},
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got, want := strings.Join(calls, ","), "ports:myserver,tunnel:myserver:0,config:myserver"; got != want {
				t.Fatalf("cleanup order = %q, want %q", got, want)
			}
		})
	}
}

func TestUninstallPeerRemoteAndConfigDoesNotPrintConfigRemovalWhenTunnelCleanupFails(t *testing.T) {
	var err error
	out := captureStdout(t, func() {
		err = uninstallPeerRemoteAndConfigWithOps("myserver", func() (*peer.Registration, error) {
			return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
		}, uninstallPeerCleanupOps{
			readManagedTunnelPorts: func(host string) (setup.ManagedTunnelPorts, error) {
				return setup.ManagedTunnelPorts{RemotePort: 18340, LocalPort: 18339}, nil
			},
			removePersistentTunnel: func(string, int) error {
				return errors.New("tunnel cleanup failed")
			},
			removeManagedHostConfig: func(string) error {
				t.Fatal("removeManagedHostConfig should not be called when tunnel cleanup fails")
				return nil
			},
		})
	})
	if err == nil || !strings.Contains(err.Error(), "persistent tunnel") {
		t.Fatalf("err = %v, want tunnel cleanup failure", err)
	}
	if strings.Contains(out, "Removed cc-clip SSH config") {
		t.Fatalf("unexpected config removal message in output:\n%s", out)
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
