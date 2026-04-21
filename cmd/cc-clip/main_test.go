package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
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
	"github.com/shunmei/cc-clip/internal/sshconfig"
	"github.com/shunmei/cc-clip/internal/token"
	"github.com/shunmei/cc-clip/internal/tunnel"
	"github.com/shunmei/cc-clip/internal/userhome"
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

type fakeUserhomeResolver struct {
	lookup func(string) (*user.User, error)
	home   func() (string, error)
	sudo   func() bool
}

func (f fakeUserhomeResolver) LookupUser(name string) (*user.User, error) {
	if f.lookup == nil {
		return nil, fmt.Errorf("unexpected LookupUser(%q)", name)
	}
	return f.lookup(name)
}

func (f fakeUserhomeResolver) UserHomeDir() (string, error) {
	if f.home == nil {
		return "", errors.New("unexpected UserHomeDir call")
	}
	return f.home()
}

func (f fakeUserhomeResolver) IsSudoRoot() bool {
	if f.sudo == nil {
		return false
	}
	return f.sudo()
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

func TestEnsureSetupLocalDaemonInstallsServiceWhenDaemonAlreadyRunningWithoutAutostart(t *testing.T) {
	t.Setenv("CC_CLIP_PROBE_TIMEOUT_MS", "1")

	installCalled := 0
	sleepCalled := 0
	lines, err := ensureSetupLocalDaemon(18339, setupLocalDaemonOps{
		goos:  "darwin",
		probe: func(addr string, timeout time.Duration) error { return nil },
		serviceStatus: func() (bool, error) {
			return false, nil
		},
		executable: func() (string, error) {
			return "/tmp/cc-clip", nil
		},
		evalSymlinks: func(path string) (string, error) {
			if path != "/tmp/cc-clip" {
				t.Fatalf("evalSymlinks path = %q, want /tmp/cc-clip", path)
			}
			return "/opt/homebrew/bin/cc-clip", nil
		},
		install: func(path string, port int) error {
			installCalled++
			if path != "/opt/homebrew/bin/cc-clip" {
				t.Fatalf("install path = %q, want /opt/homebrew/bin/cc-clip", path)
			}
			if port != 18339 {
				t.Fatalf("install port = %d, want 18339", port)
			}
			return nil
		},
		sleep: func(d time.Duration) {
			sleepCalled++
			if d != 500*time.Millisecond {
				t.Fatalf("sleep duration = %v, want 500ms", d)
			}
		},
	})
	if err != nil {
		t.Fatalf("ensureSetupLocalDaemon() error = %v", err)
	}
	if installCalled != 1 {
		t.Fatalf("install called %d times, want 1", installCalled)
	}
	if sleepCalled != 1 {
		t.Fatalf("sleep called %d times, want 1", sleepCalled)
	}
	wantLines := []string{
		"      daemon already running on :18339",
		"      auto-start not configured; installing service",
		"      launchd service installed and started",
	}
	if !reflect.DeepEqual(lines, wantLines) {
		t.Fatalf("lines = %#v, want %#v", lines, wantLines)
	}
}

func TestEnsureSetupLocalDaemonSkipsInstallWhenDaemonAlreadyRunningAndAutostartConfigured(t *testing.T) {
	t.Setenv("CC_CLIP_PROBE_TIMEOUT_MS", "1")

	lines, err := ensureSetupLocalDaemon(18339, setupLocalDaemonOps{
		goos:  "darwin",
		probe: func(addr string, timeout time.Duration) error { return nil },
		serviceStatus: func() (bool, error) {
			return true, nil
		},
		executable: func() (string, error) {
			t.Fatal("executable should not be called")
			return "", nil
		},
		evalSymlinks: func(path string) (string, error) {
			t.Fatal("evalSymlinks should not be called")
			return "", nil
		},
		install: func(path string, port int) error {
			t.Fatal("install should not be called")
			return nil
		},
		sleep: func(d time.Duration) {
			t.Fatal("sleep should not be called")
		},
	})
	if err != nil {
		t.Fatalf("ensureSetupLocalDaemon() error = %v", err)
	}
	wantLines := []string{"      daemon already running on :18339"}
	if !reflect.DeepEqual(lines, wantLines) {
		t.Fatalf("lines = %#v, want %#v", lines, wantLines)
	}
}

func TestEnsureSetupLocalDaemonInstallsServiceWhenDaemonNotRunningOnDarwin(t *testing.T) {
	t.Setenv("CC_CLIP_PROBE_TIMEOUT_MS", "1")

	installCalled := 0
	lines, err := ensureSetupLocalDaemon(18339, setupLocalDaemonOps{
		goos: "darwin",
		probe: func(addr string, timeout time.Duration) error {
			return errors.New("connection refused")
		},
		serviceStatus: func() (bool, error) {
			t.Fatal("serviceStatus should not be called")
			return false, nil
		},
		executable: func() (string, error) {
			return "/tmp/cc-clip", nil
		},
		evalSymlinks: func(path string) (string, error) {
			return path, nil
		},
		install: func(path string, port int) error {
			installCalled++
			return nil
		},
		sleep: func(d time.Duration) {},
	})
	if err != nil {
		t.Fatalf("ensureSetupLocalDaemon() error = %v", err)
	}
	if installCalled != 1 {
		t.Fatalf("install called %d times, want 1", installCalled)
	}
	wantLines := []string{"      launchd service installed and started"}
	if !reflect.DeepEqual(lines, wantLines) {
		t.Fatalf("lines = %#v, want %#v", lines, wantLines)
	}
}

func TestEnsureSetupLocalDaemonInstallsServiceWhenDaemonAlreadyRunningAndStatusCheckErrors(t *testing.T) {
	t.Setenv("CC_CLIP_PROBE_TIMEOUT_MS", "1")

	installCalled := 0
	lines, err := ensureSetupLocalDaemon(18339, setupLocalDaemonOps{
		goos:  "windows",
		probe: func(addr string, timeout time.Duration) error { return nil },
		serviceStatus: func() (bool, error) {
			return false, errors.New("not installed")
		},
		executable: func() (string, error) {
			return `C:\cc-clip.exe`, nil
		},
		evalSymlinks: func(path string) (string, error) {
			return path, nil
		},
		install: func(path string, port int) error {
			installCalled++
			if path != `C:\cc-clip.exe` {
				t.Fatalf("install path = %q, want %q", path, `C:\cc-clip.exe`)
			}
			if port != 18339 {
				t.Fatalf("install port = %d, want 18339", port)
			}
			return nil
		},
		sleep: func(d time.Duration) {},
	})
	if err != nil {
		t.Fatalf("ensureSetupLocalDaemon() error = %v", err)
	}
	if installCalled != 1 {
		t.Fatalf("install called %d times, want 1", installCalled)
	}
	wantLines := []string{
		"      daemon already running on :18339",
		"      auto-start not configured; installing service",
		"      scheduled task installed and started",
	}
	if !reflect.DeepEqual(lines, wantLines) {
		t.Fatalf("lines = %#v, want %#v", lines, wantLines)
	}
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

// TestRunRemoteNotificationHealthProbeSurfacesStderrWhenStdoutEmpty pins the
// fallback path for script-level failures that produce no stdout (missing
// hook binary, permission denied, ssh transport error). Without the fallback
// the user just sees the ssh err string with no hint at the real cause.
func TestRunRemoteNotificationHealthProbeSurfacesStderrWhenStdoutEmpty(t *testing.T) {
	// Build a real *exec.ExitError so we can attach a Stderr payload — the
	// struct's ProcessState field has unexported state that only a real run
	// can populate.
	realFail := exec.Command("sh", "-c", "exit 1")
	runErr := realFail.Run()
	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T", runErr)
	}
	exitErr.Stderr = []byte("bash: cc-clip-hook: No such file or directory\n")

	session := &fixedRemoteExecutor{err: exitErr} // empty stdout

	err := runRemoteNotificationHealthProbe(session, 19001, "~/.cache/cc-clip/peers/peer-a")
	if err == nil {
		t.Fatal("expected remote health probe failure")
	}
	if !strings.Contains(err.Error(), "No such file or directory") {
		t.Fatalf("err = %v; want stderr payload surfaced", err)
	}
}

// TestRunRemoteNotificationHealthProbeSurfacesExportedHookPrefix
// cross-pins the hook-template prefix constant with the probe's
// error-wrapping path. The hook script's strict-mode echo writes a string
// starting with shim.HookHealthFailurePrefix; this test feeds a synthetic
// remote stdout containing that exact prefix and asserts the probe
// surfaces it verbatim. Together with TestHookScriptStrictModePrefixIsExportedConstant
// in internal/shim, a future template refactor that diverges from the
// constant will fail BOTH sides simultaneously instead of letting one
// half drift.
func TestRunRemoteNotificationHealthProbeSurfacesExportedHookPrefix(t *testing.T) {
	remoteStdout := shim.HookHealthFailurePrefix + "418"
	session := &fixedRemoteExecutor{
		out: remoteStdout,
		err: errors.New("exit status 1"),
	}

	err := runRemoteNotificationHealthProbe(session, 19001, "~/.cache/cc-clip/peers/peer-a")
	if err == nil {
		t.Fatal("expected probe failure")
	}
	if !strings.Contains(err.Error(), shim.HookHealthFailurePrefix) {
		t.Fatalf("err = %v; want surfaced %q from exported hook constant", err, shim.HookHealthFailurePrefix)
	}
	if !strings.Contains(err.Error(), "418") {
		t.Fatalf("err = %v; want HTTP code suffix preserved", err)
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

// TestRunShimUninstallDoesNotTouchSSHConfig pins the invariant that
// `runShimUninstall` (the host-scoped uninstall path, NOT --peer) MUST NOT
// modify ~/.ssh/config under ANY combination of remote cleanup success or
// failure. Per AGENTS.md the only paths permitted to remove the
// `# >>> cc-clip SetEnv … >>>` marker block are --peer self-release
// (`cmdUninstallPeer`) and the equivalent `--host H --peer` flow. A future
// refactor that added `removeLaptopSSHConfigSetEnv(host)` to runShimUninstall
// would break the multi-laptop contract: it would delete one laptop's per-peer
// SSH routing whenever the user ran a host-scoped uninstall on a different
// laptop.
//
// Both the success and failure remote-cleanup branches are exercised here —
// the previous test only ran the failure branch, which left half the
// invariant unpinned (a regression that touched ssh_config only on the
// success path would have slipped through).
func TestRunShimUninstallDoesNotTouchSSHConfig(t *testing.T) {
	original := `Host example
  HostName example.com
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18340 CC_CLIP_STATE_DIR=/home/me/.cache/cc-clip/peers/peer-a
  # <<< cc-clip SetEnv (do not edit) <<<
`
	cases := []struct {
		name        string
		remoteError error
	}{
		{"remote-cleanup-succeeds", nil},
		{"remote-cleanup-fails", errors.New("ssh down")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)

			prevAdvisor := legacyManagedBlockAdvisor
			legacyManagedBlockAdvisor = func(string) string { return "" }
			t.Cleanup(func() { legacyManagedBlockAdvisor = prevAdvisor })

			sshDir := filepath.Join(home, ".ssh")
			if err := os.MkdirAll(sshDir, 0o700); err != nil {
				t.Fatalf("mkdir .ssh: %v", err)
			}
			configPath := filepath.Join(sshDir, "config")
			if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
				t.Fatalf("write ssh config: %v", err)
			}
			beforeStat, err := os.Stat(configPath)
			if err != nil {
				t.Fatalf("stat ssh config: %v", err)
			}

			runErr := runShimUninstall(shim.TargetXclip, "/tmp/local-bin", "example", uninstallOps{
				uninstallLocalShim: func(shim.Target, string) error { return nil },
				removeRemotePath: func(string) error {
					return tc.remoteError
				},
			})
			if runErr != nil {
				t.Fatalf("runShimUninstall returned error: %v", runErr)
			}

			got, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("read ssh config: %v", err)
			}
			if string(got) != original {
				t.Fatalf("ssh config must be byte-identical:\n--- got\n%s\n--- want\n%s", got, original)
			}
			afterStat, err := os.Stat(configPath)
			if err != nil {
				t.Fatalf("stat ssh config: %v", err)
			}
			// Both ModTime *and* size must match — runShimUninstall must
			// not have opened the file for write at all.
			if !beforeStat.ModTime().Equal(afterStat.ModTime()) {
				t.Fatalf("ssh config mtime must not change: before=%v after=%v", beforeStat.ModTime(), afterStat.ModTime())
			}
			if beforeStat.Size() != afterStat.Size() {
				t.Fatalf("ssh config size must not change: before=%d after=%d", beforeStat.Size(), afterStat.Size())
			}
		})
	}
}

func TestRunShimUninstallInvokesLegacyBlockAdvisor(t *testing.T) {
	// The uninstall flow must call the legacy-block advisor so users upgrading
	// from a pre-daemon-tunnel release see the "delete the managed block"
	// guidance without having to additionally run `cc-clip doctor`.
	prev := legacyManagedBlockAdvisor
	t.Cleanup(func() { legacyManagedBlockAdvisor = prev })

	var gotHosts []string
	legacyManagedBlockAdvisor = func(host string) string {
		gotHosts = append(gotHosts, host)
		return ""
	}

	// Remote branch — advisor must run with the caller's host alias.
	if err := runShimUninstall(shim.TargetXclip, "/tmp/local-bin", "example", uninstallOps{
		uninstallLocalShim: func(shim.Target, string) error { return nil },
		removeRemotePath:   func(string) error { return nil },
	}); err != nil {
		t.Fatalf("runShimUninstall(remote) returned error: %v", err)
	}

	// Local branch — advisor must still run with empty host.
	if err := runShimUninstall(shim.TargetWlPaste, "/tmp/local-bin", "", uninstallOps{
		uninstallLocalShim: func(shim.Target, string) error { return nil },
		removeRemotePath:   func(string) error { return nil },
	}); err != nil {
		t.Fatalf("runShimUninstall(local) returned error: %v", err)
	}

	if got, want := gotHosts, []string{"example", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("advisor host args: got %q, want %q", got, want)
	}
}

// TestRunShimUninstallWithHostPreservesPATHWhenOtherPeersRemain is the
// multi-peer safety pin for the plain `cc-clip uninstall --host H` path
// (the one that does NOT release the peer). When another laptop still
// holds a reservation in the remote registry, the PATH marker is shared
// infrastructure — deleting it would unhook `~/.local/bin/clipcc` from
// the other laptop's Claude Code sessions. Preserve it.
func TestRunShimUninstallWithHostPreservesPATHWhenOtherPeersRemain(t *testing.T) {
	pathCalls := 0
	err := runShimUninstall(shim.TargetXclip, "/tmp/local-bin", "example", uninstallOps{
		uninstallLocalShim: func(shim.Target, string) error { return nil },
		removeRemotePath: func(string) error {
			pathCalls++
			return nil
		},
		countRemoteOtherPeers: func(host string) (int, bool, error) {
			if host != "example" {
				t.Fatalf("peer-count query host = %q, want %q", host, "example")
			}
			return 2, true, nil
		},
	})
	if err != nil {
		t.Fatalf("runShimUninstall returned error: %v", err)
	}
	if pathCalls != 0 {
		t.Fatalf("removeRemotePath called %d times, want 0 while other peers remain", pathCalls)
	}
}

// TestRunShimUninstallWithHostRemovesPATHWhenLastPeer pins the
// complementary case: when the registry has zero peers, the shared PATH
// marker is safe to delete.
func TestRunShimUninstallWithHostRemovesPATHWhenLastPeer(t *testing.T) {
	pathCalls := 0
	err := runShimUninstall(shim.TargetXclip, "/tmp/local-bin", "example", uninstallOps{
		uninstallLocalShim: func(shim.Target, string) error { return nil },
		removeRemotePath: func(string) error {
			pathCalls++
			return nil
		},
		countRemoteOtherPeers: func(host string) (int, bool, error) { return 0, true, nil },
	})
	if err != nil {
		t.Fatalf("runShimUninstall returned error: %v", err)
	}
	if pathCalls != 1 {
		t.Fatalf("removeRemotePath called %d times, want 1 when no peers remain", pathCalls)
	}
}

// TestRunShimUninstallWithHostPreservesPATHWhenSelfUnresolved pins the
// "ambiguous preserve" branch: the registry reports peers but we can't
// identify which one (if any) is this workstation. A confident
// "N other peer(s)" message would misattribute the preservation to
// nonexistent peers and mislead the operator. Preserve with a distinct
// warning instead.
func TestRunShimUninstallWithHostPreservesPATHWhenSelfUnresolved(t *testing.T) {
	pathCalls := 0
	err := runShimUninstall(shim.TargetXclip, "/tmp/local-bin", "example", uninstallOps{
		uninstallLocalShim: func(shim.Target, string) error { return nil },
		removeRemotePath: func(string) error {
			pathCalls++
			return nil
		},
		countRemoteOtherPeers: func(host string) (int, bool, error) {
			// Registry has 1 peer but self couldn't be resolved. That 1
			// peer might be us — we can't tell — so preserve.
			return 1, false, nil
		},
	})
	if err != nil {
		t.Fatalf("runShimUninstall returned error: %v", err)
	}
	if pathCalls != 0 {
		t.Fatalf("removeRemotePath called %d times, want 0 when self peer id could not be resolved", pathCalls)
	}
}

// TestRunShimUninstallWithHostRemovesPATHWhenSelfUnresolvedAndEmpty pins
// the complementary case: registry is empty AND self could not be resolved.
// There is no risk of breaking another peer (there are none), so the marker
// is safe to remove. The self-resolution signal only gates the preserve
// branch; it does not itself block cleanup.
func TestRunShimUninstallWithHostRemovesPATHWhenSelfUnresolvedAndEmpty(t *testing.T) {
	pathCalls := 0
	err := runShimUninstall(shim.TargetXclip, "/tmp/local-bin", "example", uninstallOps{
		uninstallLocalShim: func(shim.Target, string) error { return nil },
		removeRemotePath: func(string) error {
			pathCalls++
			return nil
		},
		countRemoteOtherPeers: func(host string) (int, bool, error) { return 0, false, nil },
	})
	if err != nil {
		t.Fatalf("runShimUninstall returned error: %v", err)
	}
	if pathCalls != 1 {
		t.Fatalf("removeRemotePath called %d times, want 1 when registry is empty", pathCalls)
	}
}

// TestRunShimUninstallWithHostFailsSafeOnCountQueryError: when the remote
// registry can't be reached, preserve the PATH marker rather than risk
// breaking a concurrent peer.
func TestRunShimUninstallWithHostFailsSafeOnCountQueryError(t *testing.T) {
	pathCalls := 0
	err := runShimUninstall(shim.TargetXclip, "/tmp/local-bin", "example", uninstallOps{
		uninstallLocalShim: func(shim.Target, string) error { return nil },
		removeRemotePath: func(string) error {
			pathCalls++
			return nil
		},
		countRemoteOtherPeers: func(host string) (int, bool, error) {
			return 0, false, errors.New("ssh handshake failed")
		},
	})
	if err != nil {
		t.Fatalf("runShimUninstall returned error: %v", err)
	}
	if pathCalls != 0 {
		t.Fatalf("removeRemotePath called %d times, want 0 on count query failure (fail safe)", pathCalls)
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

func TestPostGenericNotificationBootstrapsMissingNonceFile(t *testing.T) {
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

	tokenDir := t.TempDir()
	token.TokenDirOverride = tokenDir
	defer func() { token.TokenDirOverride = "" }()

	if _, err := token.WriteTokenFile(sess.Token, sess.ExpiresAt); err != nil {
		t.Fatalf("failed to write session token: %v", err)
	}

	msg := daemon.GenericMessagePayload{
		Title:   "Bootstrapped",
		Body:    "Nonce file was created on demand",
		Urgency: 1,
	}
	if err := postGenericNotification(port, msg); err != nil {
		t.Fatalf("postGenericNotification failed: %v", err)
	}

	noncePath := filepath.Join(tokenDir, "notify.nonce")
	nonceBytes, err := os.ReadFile(noncePath)
	if err != nil {
		t.Fatalf("failed to read bootstrapped nonce file: %v", err)
	}
	nonce := strings.TrimSpace(string(nonceBytes))
	if len(nonce) != 64 {
		t.Fatalf("bootstrapped nonce length = %d, want 64", len(nonce))
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
	case <-time.After(2 * time.Second):
		t.Fatal("expected notification to be enqueued")
	}
}

func TestPostGenericNotificationReRegistersPersistedNonceAfterDaemonRestart(t *testing.T) {
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

	tokenDir := t.TempDir()
	token.TokenDirOverride = tokenDir
	defer func() { token.TokenDirOverride = "" }()

	if _, err := token.WriteTokenFile(sess.Token, sess.ExpiresAt); err != nil {
		t.Fatalf("failed to write session token: %v", err)
	}

	nonce := "test-notify-nonce-reregister-0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(tokenDir, "notify.nonce"), []byte(nonce+"\n"), 0600); err != nil {
		t.Fatalf("failed to write nonce file: %v", err)
	}

	msg := daemon.GenericMessagePayload{
		Title:   "Recovered",
		Body:    "Nonce was re-registered after daemon restart",
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
	case <-time.After(2 * time.Second):
		t.Fatal("expected notification to be enqueued after nonce re-registration")
	}
}

func TestRunCmdNotifySoundRequiresTerminalNotifier(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("notify-sound is only supported on macOS")
	}
	t.Setenv("HOME", t.TempDir())

	err := runCmdNotifySound("Glass", cmdNotifySoundDeps{
		lookPath: func(string) (string, error) {
			return "", errors.New("not found")
		},
		writeSound: func(string) error {
			t.Fatal("writeSound should not be called when terminal-notifier is missing")
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected missing terminal-notifier error")
	}
	if !strings.Contains(err.Error(), "brew install terminal-notifier") {
		t.Fatalf("expected install hint, got %v", err)
	}
}

func TestRunCmdNotifySoundPersistsSound(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("notify-sound is only supported on macOS")
	}
	t.Setenv("HOME", t.TempDir())

	stdout := captureStdout(t, func() {
		if err := runCmdNotifySound("Glass", cmdNotifySoundDeps{
			lookPath: func(string) (string, error) {
				return "/opt/homebrew/bin/terminal-notifier", nil
			},
			writeSound: daemon.WriteNotificationSound,
		}); err != nil {
			t.Fatalf("runCmdNotifySound: %v", err)
		}
	})

	got, err := daemon.ReadNotificationSound()
	if err != nil {
		t.Fatalf("ReadNotificationSound: %v", err)
	}
	if got != "Glass" {
		t.Fatalf("sound = %q, want %q", got, "Glass")
	}
	if !strings.Contains(stdout, `Set cc-clip notification sound to "Glass"`) {
		t.Fatalf("stdout = %q, want success message", stdout)
	}
}

func TestRunCmdNotifySoundClearsSound(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("notify-sound is only supported on macOS")
	}
	t.Setenv("HOME", t.TempDir())
	if err := daemon.WriteNotificationSound("Ping"); err != nil {
		t.Fatalf("seed WriteNotificationSound: %v", err)
	}

	stdout := captureStdout(t, func() {
		if err := runCmdNotifySound("off", cmdNotifySoundDeps{
			lookPath: func(string) (string, error) {
				return "/opt/homebrew/bin/terminal-notifier", nil
			},
			writeSound: daemon.WriteNotificationSound,
		}); err != nil {
			t.Fatalf("runCmdNotifySound: %v", err)
		}
	})

	got, err := daemon.ReadNotificationSound()
	if err != nil {
		t.Fatalf("ReadNotificationSound: %v", err)
	}
	if got != "" {
		t.Fatalf("sound = %q, want empty after off", got)
	}
	if !strings.Contains(stdout, "Disabled cc-clip notification sound") {
		t.Fatalf("stdout = %q, want disabled message", stdout)
	}
}

func TestRunCmdNotifySoundClearsSoundWithoutTerminalNotifier(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("notify-sound is only supported on macOS")
	}
	t.Setenv("HOME", t.TempDir())
	if err := daemon.WriteNotificationSound("Ping"); err != nil {
		t.Fatalf("seed WriteNotificationSound: %v", err)
	}

	stdout := captureStdout(t, func() {
		if err := runCmdNotifySound("off", cmdNotifySoundDeps{
			lookPath: func(string) (string, error) {
				return "", errors.New("not found")
			},
			writeSound: daemon.WriteNotificationSound,
		}); err != nil {
			t.Fatalf("runCmdNotifySound: %v", err)
		}
	})

	got, err := daemon.ReadNotificationSound()
	if err != nil {
		t.Fatalf("ReadNotificationSound: %v", err)
	}
	if got != "" {
		t.Fatalf("sound = %q, want empty after off", got)
	}
	if !strings.Contains(stdout, "Disabled cc-clip notification sound") {
		t.Fatalf("stdout = %q, want disabled message", stdout)
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

// TestResolveUninstallPeerTargetBareFlagUsesLocalIdentity pins the bare
// `uninstall --peer --host H` code path that cmdUninstallPeer takes after
// auto-filling peerArg from ident.ID. A regression where peerArg=="" is
// treated as "foreign" would silently skip local tunnel cleanup.
func TestResolveUninstallPeerTargetBareFlagUsesLocalIdentity(t *testing.T) {
	ident := peer.Identity{ID: "local-peer", Label: "macbook"}

	peerID, managedHost, err := resolveUninstallPeerTarget("myserver", "", ident)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if peerID != "local-peer" {
		t.Fatalf("bare --peer must resolve to ident.ID, got %q", peerID)
	}
	if managedHost != "myserver" {
		t.Fatalf("bare --peer must tear down local tunnel state, got host=%q", managedHost)
	}
}

func TestResolveUninstallPeerTargetBareIgnoresManagedSetEnvWithoutLocalIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	cfg := `Host myserver
  HostName example.com
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18339 CC_CLIP_STATE_DIR=/home/shared/.cache/cc-clip/peers/stale-peer
  # <<< cc-clip SetEnv (do not edit) <<<
`
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(cfg), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := resolveUninstallPeerTarget("myserver", "", peer.Identity{})
	if err == nil || !strings.Contains(err.Error(), "pass --peer-id <id> explicitly") {
		t.Fatalf("err = %v, want fail-closed missing-identity error", err)
	}
}

func TestResolveSelfUninstallPeerTargetRequiresLocalIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	cfg := `Host myserver
  HostName example.com
  # >>> cc-clip SetEnv (do not edit) >>>
  SetEnv CC_CLIP_PORT=18339 CC_CLIP_STATE_DIR=/home/shared/.cache/cc-clip/peers/stale-peer
  # <<< cc-clip SetEnv (do not edit) <<<
`
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(cfg), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := resolveSelfUninstallPeerTarget("myserver", peer.Identity{})
	if err == nil || !strings.Contains(err.Error(), "restore the local peer identity first") {
		t.Fatalf("err = %v, want fail-closed self-resolution error", err)
	}
}

func TestLoadIdentityForUninstallPeerBareRequiresExistingIdentity(t *testing.T) {
	prev := loadLocalPeerIdentity
	t.Cleanup(func() { loadLocalPeerIdentity = prev })
	loadLocalPeerIdentity = func() (peer.Identity, error) {
		return peer.Identity{}, peer.ErrLocalIdentityNotFound
	}

	_, err := loadIdentityForUninstallPeer("")
	if err == nil || !strings.Contains(err.Error(), "pass --peer-id <id> explicitly") {
		t.Fatalf("err = %v, want actionable missing-identity error", err)
	}
}

func TestLoadIdentityForUninstallPeerExplicitPeerToleratesMissingIdentity(t *testing.T) {
	prev := loadLocalPeerIdentity
	t.Cleanup(func() { loadLocalPeerIdentity = prev })
	loadLocalPeerIdentity = func() (peer.Identity, error) {
		return peer.Identity{}, peer.ErrLocalIdentityNotFound
	}

	ident, err := loadIdentityForUninstallPeer("foreign-peer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ident != (peer.Identity{}) {
		t.Fatalf("ident = %#v, want zero identity when local identity is absent", ident)
	}
}

func TestLocalPeerRegistrationFailsClosedWithoutIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	reg, err := localPeerRegistration(nil, "~/.local/bin/cc-clip")
	if !errors.Is(err, peer.ErrLocalIdentityNotFound) {
		t.Fatalf("err = %v, want ErrLocalIdentityNotFound", err)
	}
	if reg != nil {
		t.Fatalf("reg = %#v, want nil when local identity is missing", reg)
	}

	baseDir, baseErr := peer.BaseDir()
	if baseErr != nil {
		t.Fatalf("BaseDir: %v", baseErr)
	}
	if _, statErr := os.Stat(filepath.Join(baseDir, "local-peer-id")); !os.IsNotExist(statErr) {
		t.Fatalf("localPeerRegistration should not create local identity, stat err = %v", statErr)
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

// TestRollbackConnectReservationRunsBothEffectsInOrder pins the contract
// that the `failAfterSave` rollback path in runConnect always removes the
// local tunnel-state file AND releases the remote peer, in that order, even
// if the state removal fails. A partial refactor that forgot to wire one
// side would silently leak either a remote port reservation or a stale
// local state file that only `cc-clip tunnel remove` would clean.
func TestRollbackConnectReservationRunsBothEffectsInOrder(t *testing.T) {
	cases := []struct {
		name       string
		removeErr  error
		wantOrder  []string
		wantLogSub string
	}{
		{
			name:      "happy_path",
			removeErr: nil,
			wantOrder: []string{"removeState", "releasePeer"},
		},
		{
			name:       "state_remove_failure_still_releases_peer",
			removeErr:  errors.New("disk gone"),
			wantOrder:  []string{"removeState", "releasePeer"},
			wantLogSub: "disk gone",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			var logBuf bytes.Buffer
			oldOut := log.Writer()
			oldFlags := log.Flags()
			log.SetOutput(&logBuf)
			log.SetFlags(0)
			t.Cleanup(func() {
				log.SetOutput(oldOut)
				log.SetFlags(oldFlags)
			})

			rollbackConnectReservation(
				func() error {
					calls = append(calls, "removeState")
					return tc.removeErr
				},
				func() {
					calls = append(calls, "releasePeer")
				},
			)

			if !reflect.DeepEqual(calls, tc.wantOrder) {
				t.Fatalf("call order = %v, want %v", calls, tc.wantOrder)
			}
			if tc.wantLogSub != "" && !strings.Contains(logBuf.String(), tc.wantLogSub) {
				t.Fatalf("log output = %q, want substring %q", logBuf.String(), tc.wantLogSub)
			}
		})
	}
}

// TestRollbackConnectReservationHandlesNilEffects pins the defensive nil
// guards: a caller that omits either side (e.g. tests) must not panic.
func TestRollbackConnectReservationHandlesNilEffects(t *testing.T) {
	// Should not panic.
	rollbackConnectReservation(nil, nil)
}

// TestConnectStartPersistentTunnelWithReportsUnknownWhenHostMissing pins the
// "daemon reachable, but our host never appears in /tunnels" branch: the
// poll must still return a timeout-style error with status=unknown and no
// diagnostic suffix (there is nothing else to report). Without this test a
// future refactor could silently swallow the branch or misreport a stale
// lastStatus from a prior iteration.
func TestConnectStartPersistentTunnelWithReportsUnknownWhenHostMissing(t *testing.T) {
	oldTimeout := connectTunnelUpTimeout
	oldInterval := connectTunnelUpPollInterval
	connectTunnelUpTimeout = 30 * time.Millisecond
	connectTunnelUpPollInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		connectTunnelUpTimeout = oldTimeout
		connectTunnelUpPollInterval = oldInterval
	})

	// /tunnels returns an empty list — daemon is up, but our host is nowhere.
	fetch := func(int) ([]*tunnel.TunnelState, error) {
		return nil, nil
	}
	err := connectStartPersistentTunnelWith(18339, "myserver", 19001,
		func(int, string, int) error { return nil }, fetch)
	if err == nil {
		t.Fatal("expected timeout error when host never appears")
	}
	if !strings.Contains(err.Error(), "did not reach connected state") {
		t.Fatalf("err = %v, want timeout message", err)
	}
	if !strings.Contains(err.Error(), "status=unknown") {
		t.Fatalf("err = %v, want status=unknown for never-seen host", err)
	}
	// No diagnostic suffix is appropriate here: there is no other state to
	// report. A regression that fabricates one would surface as extra text.
	if strings.Contains(err.Error(), "daemon reports:") {
		t.Fatalf("err = %v, expected no 'daemon reports:' suffix when host never appeared", err)
	}
	if strings.Contains(err.Error(), "no tunnel owned by daemon on port") {
		t.Fatalf("err = %v, expected no wrong-port suffix when host never appeared", err)
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

func TestCodexCleanupStateDirsWithLocalIdentityFallbackAddsPeerCodexDir(t *testing.T) {
	prev := loadLocalPeerIdentity
	t.Cleanup(func() { loadLocalPeerIdentity = prev })
	loadLocalPeerIdentity = func() (peer.Identity, error) {
		return peer.Identity{ID: "peer-a"}, nil
	}

	got := codexCleanupStateDirsWithLocalIdentityFallback(nil, errors.New("remote lookup failed"))
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

func TestCodexCleanupStateDirsWithLocalIdentityFallbackPreservesExplicitPeerState(t *testing.T) {
	prev := loadLocalPeerIdentity
	t.Cleanup(func() { loadLocalPeerIdentity = prev })
	loadLocalPeerIdentity = func() (peer.Identity, error) {
		return peer.Identity{ID: "peer-b"}, nil
	}

	reg := &peer.Registration{PeerID: "peer-a", StateDir: "~/.cache/cc-clip/peers/peer-a"}
	got := codexCleanupStateDirsWithLocalIdentityFallback(reg, nil)
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

func TestCmdUninstallCodexLocalSkipsRelativeCleanupWhenHomeLookupFails(t *testing.T) {
	prevDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	cwd := t.TempDir()
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("Chdir temp dir: %v", err)
	}
	t.Cleanup(func() {
		if chdirErr := os.Chdir(prevDir); chdirErr != nil {
			t.Fatalf("restore cwd: %v", chdirErr)
		}
	})

	userhome.SetResolverForTest(t, fakeUserhomeResolver{
		home: func() (string, error) {
			return "", errors.New("boom")
		},
	})

	relativeStateDir := filepath.Join(cwd, ".cache", "cc-clip", "codex")
	if err := os.MkdirAll(relativeStateDir, 0700); err != nil {
		t.Fatalf("MkdirAll relative state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(relativeStateDir, "bridge.pid"), []byte("123\n"), 0600); err != nil {
		t.Fatalf("WriteFile bridge pid: %v", err)
	}

	output := captureStdout(t, cmdUninstallCodexLocal)
	if !strings.Contains(output, "cannot determine local home directory") {
		t.Fatalf("expected home-directory warning, got:\n%s", output)
	}
	if _, err := os.Stat(relativeStateDir); err != nil {
		t.Fatalf("relative state dir should be preserved, stat err=%v", err)
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

func TestCleanupAndReleasePeerWithCleansLegacyStateWhenLookupMissing(t *testing.T) {
	var cleaned []string
	releaseCalls := 0

	reg, err := cleanupAndReleasePeerWith(
		"peer-a",
		func() (peer.Registration, error) {
			return peer.Registration{}, fmt.Errorf("lookup failed: %w", peer.ErrPeerNotFound)
		},
		func() (peer.Registration, error) {
			releaseCalls++
			return peer.Registration{}, nil
		},
		func(stateDir string) error {
			cleaned = append(cleaned, stateDir)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg != nil {
		t.Fatalf("expected nil registration on idempotent lookup miss, got %#v", reg)
	}
	if releaseCalls != 0 {
		t.Fatalf("release called %d times, want 0 after lookup miss", releaseCalls)
	}
	if got, want := cleaned, []string{legacyPeerStateDir("peer-a")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup calls = %v, want %v", got, want)
	}
}

func TestCleanupAndReleasePeerWithRejectsUnsafePeerIDOnLookupMiss(t *testing.T) {
	cleanupCalls := 0
	reg, err := cleanupAndReleasePeerWith(
		"../../.ssh",
		func() (peer.Registration, error) {
			return peer.Registration{}, fmt.Errorf("lookup failed: %w", peer.ErrPeerNotFound)
		},
		func() (peer.Registration, error) {
			return peer.Registration{}, nil
		},
		func(string) error {
			cleanupCalls++
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "invalid peer id") {
		t.Fatalf("err = %v, want invalid peer id error", err)
	}
	if reg != nil {
		t.Fatalf("expected nil registration, got %#v", reg)
	}
	if cleanupCalls != 0 {
		t.Fatalf("cleanup called %d times, want 0", cleanupCalls)
	}
}

// TestCleanupAndReleasePeerWithSurfacesLookupRegOnReleaseRace pins that
// when lookup returns a real reservation but release races to
// ErrPeerNotFound (concurrent uninstall or stale-after-lookup), the caller
// still gets the registration snapshot from lookup. Without this, the
// user-facing message would degrade to "Peer already released on remote
// (idempotent)" even though we know exactly which port/label was reserved,
// which is confusing during multi-host cleanup.
func TestCleanupAndReleasePeerWithSurfacesLookupRegOnReleaseRace(t *testing.T) {
	var cleaned []string
	releaseCalls := 0

	reg, err := cleanupAndReleasePeerWith(
		"peer-a",
		func() (peer.Registration, error) {
			return peer.Registration{
				PeerID:       "peer-a",
				Label:        "laptop-a",
				ReservedPort: 18339,
				StateDir:     "~/.cache/cc-clip/peers/peer-a-custom",
			}, nil
		},
		func() (peer.Registration, error) {
			releaseCalls++
			return peer.Registration{}, fmt.Errorf("release failed: %w", peer.ErrPeerNotFound)
		},
		func(stateDir string) error {
			cleaned = append(cleaned, stateDir)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg == nil {
		t.Fatalf("expected lookup reg snapshot on release race, got nil")
	}
	if reg.ReservedPort != 18339 || reg.Label != "laptop-a" {
		t.Fatalf("reg = %#v, want port=18339 label=laptop-a", reg)
	}
	if releaseCalls != 1 {
		t.Fatalf("release called %d times, want 1", releaseCalls)
	}
	if got, want := cleaned, []string{"~/.cache/cc-clip/peers/peer-a-custom"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup calls = %v, want %v", got, want)
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
		`rm -f "$HOME/.local/bin/cc-clip-hook"`,
		`rm -f "$HOME/.local/bin/clipcc"`,
		`sed -i.cc-clip-bak '/# >>> cc-clip notify \(do not edit\) >>>/,/# <<< cc-clip notify \(do not edit\) <<</d' ~/.codex/config.toml 2>/dev/null || true; rm -f ~/.codex/config.toml.cc-clip-bak`,
	} {
		if session.indexOfCommandContaining(needle) < 0 {
			t.Fatalf("expected command containing %q, got %v", needle, session.commands)
		}
	}

	// Multi-peer safety pin #1: removeRemoteNotifyState no longer issues the
	// global `find ~/.cache/cc-clip/peers … -delete` sweep that earlier
	// versions ran. If this re-appears, it will silently wipe other
	// laptops' notify nonces on shared-account servers. See
	// cmd/cc-clip/main.go:removeRemoteNotifyState for the rationale.
	if session.indexOfCommandContaining(`find "$HOME/.cache/cc-clip/peers"`) >= 0 {
		t.Fatalf("removeRemoteNotifyState must not sweep other peers' notify state, got %v", session.commands)
	}

	// Multi-peer safety pin #2: removeRemoteNotifyState no longer touches
	// the legacy ~/.cache/cc-clip/notify.nonce that the hook script falls
	// back to when a caller's SSH session lacks CC_CLIP_STATE_DIR. Sweeping
	// it from one peer's teardown would break every other peer that relies
	// on that fallback. The contract in AGENTS.md is explicit: "touches only
	// the caller's own stateDir".
	if session.indexOfCommandContaining(`rm -f "$HOME/.cache/cc-clip/notify.nonce"`) >= 0 {
		t.Fatalf("removeRemoteNotifyState must not delete the legacy shared notify.nonce, got %v", session.commands)
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

// TestUninstallPeerRemoteAndConfigInvokesPATHCleanup pins the contract that
// `cc-clip uninstall --host H --peer` also strips the remote PATH marker, so
// the documented "remote shim + peer + local tunnel state" sequence does not
// leave behind an orphan ~/.local/bin/cc-clip shim pointing at a
// now-unreachable daemon.
func TestUninstallPeerRemoteAndConfigInvokesPATHCleanup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	pathCalls := 0
	err := uninstallPeerRemoteAndConfigWithOps("myserver", func() (*peer.Registration, error) {
		return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
	}, uninstallPeerCleanupOps{
		removeRemotePath: func() error {
			pathCalls++
			return nil
		},
		// countRemainingPeers=0 is required for shared-asset cleanup to
		// fire: a nil counter is now treated as "unknown → preserve"
		// (fail-safe invariant pinned in AGENTS.md).
		countRemainingPeers:    func() (int, error) { return 0, nil },
		removePersistentTunnel: func(string, int) error { return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pathCalls != 1 {
		t.Fatalf("removeRemotePath called %d times, want 1", pathCalls)
	}
}

// TestUninstallPeerRemoteAndConfigSurfacesPATHCleanupErrors pins the
// SetEnv-preservation invariant: a PATH-cleanup failure must surface as a
// non-nil error so cmdUninstallPeer's log.Fatalf preserves the local
// ~/.ssh/config SetEnv block for a safe retry.
func TestUninstallPeerRemoteAndConfigSurfacesPATHCleanupErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	err := uninstallPeerRemoteAndConfigWithOps("myserver", func() (*peer.Registration, error) {
		return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
	}, uninstallPeerCleanupOps{
		removeRemotePath: func() error {
			return errors.New("rc file missing")
		},
		// countRemainingPeers=0 is required for the shared-asset cleanup
		// path to execute — otherwise removeRemotePath is skipped under
		// the fail-safe default and no error would be surfaced.
		countRemainingPeers:    func() (int, error) { return 0, nil },
		removePersistentTunnel: func(string, int) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "PATH marker") {
		t.Fatalf("err = %v, want PATH marker failure", err)
	}
}

func TestUninstallPeerRemoteAndConfigSkipsFollowOnCleanupWhenRemoteCleanupFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	pathCalls := 0
	tunnelCalls := 0
	err := uninstallPeerRemoteAndConfigWithOps("myserver", func() (*peer.Registration, error) {
		return nil, errors.New("remote cleanup failed")
	}, uninstallPeerCleanupOps{
		removeRemotePath: func() error {
			pathCalls++
			return nil
		},
		removePersistentTunnel: func(string, int) error {
			tunnelCalls++
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "remote cleanup failed") {
		t.Fatalf("err = %v, want remote cleanup failure", err)
	}
	if pathCalls != 0 {
		t.Fatalf("removeRemotePath called %d times, want 0", pathCalls)
	}
	if tunnelCalls != 0 {
		t.Fatalf("removePersistentTunnel called %d times, want 0", tunnelCalls)
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

// TestRunCmdUninstallPeerPreservesSetEnvWhenSessionFails pins the
// cmdUninstallPeer ordering invariant directly: any failure on the path
// to uninstallPeerRemoteAndConfig — including a failed SSH session
// open — must NOT call deps.removeSetEnv. The local SetEnv block in
// ~/.ssh/config has to survive so the operator's retry can reuse the
// already-allocated remote port; rewriting it on a failed cleanup would
// either force-allocate a new remote port on retry or, worse, drop the
// per-peer state-dir hint and let the next session's hook script
// authenticate against the wrong nonce.
func TestRunCmdUninstallPeerPreservesSetEnvWhenSessionFails(t *testing.T) {
	prev := loadLocalPeerIdentity
	t.Cleanup(func() { loadLocalPeerIdentity = prev })
	loadLocalPeerIdentity = func() (peer.Identity, error) {
		return peer.Identity{ID: "abcdef0123456789", Label: "imac"}, nil
	}

	setEnvCalls := []string{}
	advisCalls := []string{}
	err := runCmdUninstallPeer("myserver", "", cmdUninstallPeerDeps{
		newSession: func(host string) (*shim.SSHSession, error) {
			return nil, errors.New("ssh transport down")
		},
		removeSetEnv:     func(host string) { setEnvCalls = append(setEnvCalls, host) },
		printLegacyAdvis: func(host string) { advisCalls = append(advisCalls, host) },
	})
	if err == nil || !strings.Contains(err.Error(), "ssh transport down") {
		t.Fatalf("expected ssh transport error, got %v", err)
	}
	if len(setEnvCalls) != 0 {
		t.Fatalf("removeSetEnv was called %v despite session failure; SetEnv block must survive for retry", setEnvCalls)
	}
	if len(advisCalls) != 0 {
		t.Fatalf("printLegacyAdvis was called %v despite session failure; must abort early", advisCalls)
	}
}

// TestRunCmdUninstallPeerSkipsSetEnvForNonSelfPeer pins the second half
// of the ordering contract on the SUCCESS path: removeSetEnv must NOT fire
// when the operator is releasing somebody else's peer (managedHost == "").
// Cleaning a foreign peer's lease does not grant permission to edit this
// laptop's ssh config, which is what would happen if a future refactor
// unconditionally called removeSetEnv after a successful cleanup.
func TestRunCmdUninstallPeerSkipsSetEnvForNonSelfPeer(t *testing.T) {
	prev := loadLocalPeerIdentity
	t.Cleanup(func() { loadLocalPeerIdentity = prev })
	// Local identity has its own ID; we pass a DIFFERENT --peer-id, so
	// resolveUninstallPeerTarget returns managedHost="" (foreign peer).
	loadLocalPeerIdentity = func() (peer.Identity, error) {
		return peer.Identity{ID: "this-laptop-id", Label: "imac"}, nil
	}

	setEnvCalls := []string{}
	legacyAdvisCalls := []string{}
	err := runCmdUninstallPeer("myserver", "some-other-peer-id", cmdUninstallPeerDeps{
		remoteCleanup: func(host, managedHost, peerID string) error {
			if host != "myserver" {
				t.Fatalf("host = %q, want myserver", host)
			}
			if managedHost != "" {
				t.Fatalf("managedHost = %q, want empty for foreign peer", managedHost)
			}
			if peerID != "some-other-peer-id" {
				t.Fatalf("peerID = %q, want some-other-peer-id", peerID)
			}
			return nil
		},
		removeSetEnv:     func(host string) { setEnvCalls = append(setEnvCalls, host) },
		printLegacyAdvis: func(host string) { legacyAdvisCalls = append(legacyAdvisCalls, host) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(setEnvCalls) != 0 {
		t.Fatalf("removeSetEnv was called %v for a foreign --peer-id; only self-release may touch ssh_config", setEnvCalls)
	}
	if got, want := legacyAdvisCalls, []string{"myserver"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy advisory calls = %#v, want %#v", got, want)
	}
}

// TestUninstallPeerRemoteAndConfigPreservesSharedAssetsWhenOtherPeersRemain
// is the multi-peer safety pin: when the remote peer registry still lists
// another laptop's reservation after our release, cc-clip must NOT delete
// the shared `~/.local/bin/clipcc`, `cc-clip-hook`, Codex notify config,
// or PATH marker — doing so would silently break the other laptop's
// clipboard / notification path until its next `cc-clip connect`.
func TestUninstallPeerRemoteAndConfigPreservesSharedAssetsWhenOtherPeersRemain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	notifyCalls := 0
	pathCalls := 0
	err := uninstallPeerRemoteAndConfigWithOps("myserver", func() (*peer.Registration, error) {
		return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
	}, uninstallPeerCleanupOps{
		removeNotify: func(*peer.Registration) error {
			notifyCalls++
			return nil
		},
		removeRemotePath: func() error {
			pathCalls++
			return nil
		},
		countRemainingPeers: func() (int, error) {
			return 1, nil
		},
		removePersistentTunnel: func(string, int) error { return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if notifyCalls != 0 {
		t.Fatalf("removeNotify called %d times, want 0 while another peer remains", notifyCalls)
	}
	if pathCalls != 0 {
		t.Fatalf("removeRemotePath called %d times, want 0 while another peer remains", pathCalls)
	}
}

// TestUninstallPeerRemoteAndConfigRemovesSharedAssetsWhenLastPeer pins the
// complementary case: once our release leaves zero peers, the shared
// assets are safe to delete and the cleanup should fire.
func TestUninstallPeerRemoteAndConfigRemovesSharedAssetsWhenLastPeer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	notifyCalls := 0
	pathCalls := 0
	err := uninstallPeerRemoteAndConfigWithOps("myserver", func() (*peer.Registration, error) {
		return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
	}, uninstallPeerCleanupOps{
		removeNotify: func(*peer.Registration) error {
			notifyCalls++
			return nil
		},
		removeRemotePath: func() error {
			pathCalls++
			return nil
		},
		countRemainingPeers: func() (int, error) {
			return 0, nil
		},
		removePersistentTunnel: func(string, int) error { return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if notifyCalls != 1 {
		t.Fatalf("removeNotify called %d times, want 1", notifyCalls)
	}
	if pathCalls != 1 {
		t.Fatalf("removeRemotePath called %d times, want 1", pathCalls)
	}
}

// TestUninstallPeerRemoteAndConfigFailsSafeOnCountQueryError pins the
// "preserve on doubt" rule: if we can't confirm this is the last peer
// (registry unreadable, ssh flake, …), the shared assets stay put. We'd
// rather leak a hook script on the remote than wipe one another laptop
// is actively using.
func TestUninstallPeerRemoteAndConfigFailsSafeOnCountQueryError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	notifyCalls := 0
	pathCalls := 0
	err := uninstallPeerRemoteAndConfigWithOps("myserver", func() (*peer.Registration, error) {
		return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
	}, uninstallPeerCleanupOps{
		removeNotify: func(*peer.Registration) error {
			notifyCalls++
			return nil
		},
		removeRemotePath: func() error {
			pathCalls++
			return nil
		},
		countRemainingPeers: func() (int, error) {
			return 0, errors.New("registry unreadable")
		},
		removePersistentTunnel: func(string, int) error { return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if notifyCalls != 0 {
		t.Fatalf("removeNotify called %d times, want 0 on count failure (fail safe)", notifyCalls)
	}
	if pathCalls != 0 {
		t.Fatalf("removeRemotePath called %d times, want 0 on count failure (fail safe)", pathCalls)
	}
}

// TestUninstallPeerRemoteAndConfigFailsSafeWhenCountOpNotWired pins the
// stricter half of the fail-safe invariant: a caller that does NOT wire
// countRemainingPeers is treated as "unknown peer count → preserve",
// not "assume solo ownership → delete". Without this test a future
// refactor could quietly re-enable the old nil=allow default and a
// caller that forgot to wire the op would silently wipe shared assets
// on a multi-laptop account. AGENTS.md pins this fail-closed behavior.
func TestUninstallPeerRemoteAndConfigFailsSafeWhenCountOpNotWired(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	notifyCalls := 0
	pathCalls := 0
	err := uninstallPeerRemoteAndConfigWithOps("myserver", func() (*peer.Registration, error) {
		return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
	}, uninstallPeerCleanupOps{
		removeNotify: func(*peer.Registration) error {
			notifyCalls++
			return nil
		},
		removeRemotePath: func() error {
			pathCalls++
			return nil
		},
		// countRemainingPeers intentionally unwired — the contract says
		// this must PRESERVE shared assets, not delete them.
		removePersistentTunnel: func(string, int) error { return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if notifyCalls != 0 {
		t.Fatalf("removeNotify called %d times, want 0 when count op is not wired (fail safe)", notifyCalls)
	}
	if pathCalls != 0 {
		t.Fatalf("removeRemotePath called %d times, want 0 when count op is not wired (fail safe)", pathCalls)
	}
}

// TestUninstallPeerPreservesLegacySSHConfigBlock pins the "no auto-migration"
// contract documented in CLAUDE.md: cc-clip must never silently delete a
// user's leftover `# >>> cc-clip managed host: ... >>>` block from
// ~/.ssh/config. If a future contributor adds a cleanup shim, this test will
// fail because the fake HOME's ssh/config is byte-compared before/after.
func TestUninstallPeerPreservesLegacySSHConfigBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Defensive: stub the legacy-block advisor so a future refactor that
	// routes this test through printLegacyManagedBlockAdvisoryIfAny cannot
	// reach the real user's ~/.ssh/config even if HOME override is leaky.
	prevAdvisor := legacyManagedBlockAdvisor
	legacyManagedBlockAdvisor = func(string) string { return "" }
	t.Cleanup(func() { legacyManagedBlockAdvisor = prevAdvisor })

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	legacy := `Host example.com
  HostName example.com
  User me

# >>> cc-clip managed host: example.com >>>
Host example.com
  RemoteForward 18339 127.0.0.1:18339
  ControlMaster no
  ControlPath none
# <<< cc-clip managed host: example.com <<<
`
	configPath := filepath.Join(sshDir, "config")
	if err := os.WriteFile(configPath, []byte(legacy), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := uninstallPeerRemoteAndConfigWithOps("example.com", func() (*peer.Registration, error) {
		return &peer.Registration{PeerID: "peer-a", Label: "imac", ReservedPort: 18340}, nil
	}, uninstallPeerCleanupOps{
		removePersistentTunnel: func(string, int) error { return nil },
	})
	if err != nil {
		t.Fatalf("uninstall returned error: %v", err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after: %v", err)
	}
	if string(after) != legacy {
		t.Fatalf("cc-clip uninstall must not modify ~/.ssh/config.\nbefore:\n%s\nafter:\n%s", legacy, after)
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

func TestConfiguredDoctorPortsUsesRemoteSelectorOnlyWhenExplicit(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		env            string
		envSet         bool
		wantLocalPort  int
		wantRemotePort int
	}{
		{
			name:           "implicit default leaves remote selector unset",
			args:           []string{"cc-clip", "doctor", "--host", "myserver"},
			wantLocalPort:  18339,
			wantRemotePort: 0,
		},
		{
			name:           "explicit flag selects same remote port",
			args:           []string{"cc-clip", "doctor", "--host", "myserver", "--port", "18444"},
			wantLocalPort:  18444,
			wantRemotePort: 18444,
		},
		{
			name:           "env override is explicit for remote selector too",
			args:           []string{"cc-clip", "doctor", "--host", "myserver"},
			env:            "18444",
			envSet:         true,
			wantLocalPort:  18444,
			wantRemotePort: 18444,
		},
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

			localPort, remotePort, err := configuredDoctorPorts()
			if err != nil {
				t.Fatalf("configuredDoctorPorts err = %v", err)
			}
			if localPort != tt.wantLocalPort {
				t.Fatalf("localPort = %d, want %d", localPort, tt.wantLocalPort)
			}
			if remotePort != tt.wantRemotePort {
				t.Fatalf("remotePort = %d, want %d", remotePort, tt.wantRemotePort)
			}
		})
	}
}

// captureStdioMu serialises access to captureStdio. The helper swaps
// os.Stdout and os.Stderr process-wide, so any t.Parallel() introduced
// in this package would race on those globals without this guard. The
// mutex is cheap: the helper is used only in a handful of setup tests.
var captureStdioMu sync.Mutex

// captureStdio captures both stdout and stderr produced by fn. Used by
// the applyLaptopSSHConfigSetEnv / removeLaptopSSHConfigSetEnv tests to
// pin the user-facing success and warning lines separately.
func captureStdio(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()

	captureStdioMu.Lock()
	defer captureStdioMu.Unlock()

	oldStdout, oldStderr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	defer func() {
		os.Stdout, os.Stderr = oldStdout, oldStderr
	}()

	fn()

	_ = wOut.Close()
	_ = wErr.Close()
	outBytes, _ := io.ReadAll(rOut)
	errBytes, _ := io.ReadAll(rErr)
	_ = rOut.Close()
	_ = rErr.Close()
	return string(outBytes), string(errBytes)
}

// TestApplyLaptopSSHConfigSetEnvHappyPath pins that a literal Host block
// receives the SetEnv marker pair and the success line is printed to
// stdout (not stderr — operators grep stderr for warnings).
func TestApplyLaptopSSHConfigSetEnvHappyPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	cfgPath := filepath.Join(sshDir, "config")
	if err := os.WriteFile(cfgPath, []byte("Host myalias\n  HostName srv.example.com\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	stdout, stderr := captureStdio(t, func() {
		applyLaptopSSHConfigSetEnv("myalias", 18340, "/home/shared/.cache/cc-clip/peers/peer-a")
	})

	if stderr != "" {
		t.Errorf("stderr should be empty on happy path, got %q", stderr)
	}
	if !strings.Contains(stdout, "applied cc-clip SetEnv block") || !strings.Contains(stdout, "Host myalias") {
		t.Errorf("stdout missing success line, got %q", stdout)
	}

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, want := range []string{
		sshconfig.MarkerBegin,
		"SetEnv CC_CLIP_PORT=18340 CC_CLIP_STATE_DIR=/home/shared/.cache/cc-clip/peers/peer-a",
		sshconfig.MarkerEnd,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("config missing %q; got:\n%s", want, got)
		}
	}
}

// TestApplyLaptopSSHConfigSetEnvQuotesStateDirWithSpaces pins the
// end-to-end behavior when the remote peer registry returns a state dir
// containing whitespace. `applyLaptopSSHConfigSetEnv` delegates quoting
// to `sshconfig.Apply`, which must emit the value inside a quoted
// SetEnv token so `ssh -G` round-trips it verbatim. Without this test
// a future refactor that splits the SetEnv into two directives (one
// per var) or bypasses sshconfig's quoting would silently break the
// remote `clipcc` resolver.
func TestApplyLaptopSSHConfigSetEnvQuotesStateDirWithSpaces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	cfgPath := filepath.Join(sshDir, "config")
	if err := os.WriteFile(cfgPath, []byte("Host myalias\n  HostName srv.example.com\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	spacey := `/home/shared user/.cache/cc-clip/peers/peer a`
	stdout, stderr := captureStdio(t, func() {
		applyLaptopSSHConfigSetEnv("myalias", 18340, spacey)
	})
	if stderr != "" {
		t.Errorf("stderr should be empty on happy path with spaced state dir, got %q", stderr)
	}
	if !strings.Contains(stdout, "applied cc-clip SetEnv block") {
		t.Errorf("stdout missing success line, got %q", stdout)
	}
	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	// sshconfig.Apply emits the value inside quotes when it needs
	// quoting; the unquoted raw path would round-trip as three tokens
	// through ssh -G. Assert both halves end up on a single SetEnv line.
	wantLine := `SetEnv CC_CLIP_PORT=18340 CC_CLIP_STATE_DIR="/home/shared user/.cache/cc-clip/peers/peer a"`
	if !strings.Contains(string(got), wantLine) {
		t.Fatalf("ssh_config missing expected quoted SetEnv line %q; got:\n%s", wantLine, got)
	}

	// ssh -G proves the round-trip: if quoting is wrong, the state dir
	// arrives as truncated or as multiple setenv entries. Skip on hosts
	// without ssh (CI containers without the client package).
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skipf("ssh not available: %v", err)
	}
	out, err := exec.Command("ssh", "-G", "-F", cfgPath, "myalias").CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G failed: %v\noutput: %s", err, out)
	}
	// Historic ssh -G lower-cases the keyword and prints `setenv KEY=VALUE`
	// per pair; the quoted value is emitted without surrounding quotes.
	want := "setenv CC_CLIP_STATE_DIR=" + spacey
	if !strings.Contains(string(out), want) {
		t.Fatalf("ssh -G did not round-trip spaced state dir; want %q in:\n%s", want, out)
	}
}

// TestApplyLaptopSSHConfigSetEnvWarnings exercises every user-facing
// error branch in applyLaptopSSHConfigSetEnv. Each case pins:
//   - the warning goes to stderr (matching runShimUninstall and the UNIX
//     convention that step confirmations land on stdout and anything the
//     user must notice-or-else lands on stderr; scripted operators grep
//     stderr for warnings and would miss them on stdout)
//   - stdout does NOT carry the warning
//   - the host name appears in the warning (operators need to know which
//     alias failed)
//   - a distinctive phrase identifies which branch fired (so a refactor
//     that collapses two branches into one generic message is caught)
//   - the success line is NOT emitted on stdout (helper returned from
//     the warn branch)
func TestApplyLaptopSSHConfigSetEnvWarnings(t *testing.T) {
	cases := []struct {
		name        string
		setupConfig func(t *testing.T, cfgPath string) // nil = no config file
		host        string
		wantPhrase  string // substring that must appear in stdout
	}{
		{
			name: "host_block_missing",
			setupConfig: func(t *testing.T, p string) {
				if err := os.WriteFile(p, []byte("Host other\n  HostName other.example\n"), 0600); err != nil {
					t.Fatalf("write config: %v", err)
				}
			},
			host:       "myalias",
			wantPhrase: "no `Host myalias` block found",
		},
		{
			name: "only_glob_match",
			setupConfig: func(t *testing.T, p string) {
				if err := os.WriteFile(p, []byte("Host *.example.com\n  User shared\n"), 0600); err != nil {
					t.Fatalf("write config: %v", err)
				}
			},
			host:       "box.example.com",
			wantPhrase: "matched only by a wildcard or negation pattern",
		},
		{
			name: "host_block_in_include",
			setupConfig: func(t *testing.T, p string) {
				if err := os.WriteFile(p, []byte("Include ~/.ssh/conf.d/*.conf\nHost other\n  HostName other.example\n"), 0600); err != nil {
					t.Fatalf("write config: %v", err)
				}
			},
			host:       "myalias",
			wantPhrase: "does not walk Include directives",
		},
		{
			name: "symlinked_config",
			setupConfig: func(t *testing.T, p string) {
				real := p + ".real"
				if err := os.WriteFile(real, []byte("Host myalias\n  HostName srv\n"), 0600); err != nil {
					t.Fatalf("write real: %v", err)
				}
				if err := os.Symlink(real, p); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			host:       "myalias",
			wantPhrase: "~/.ssh/config is a symlink",
		},
		{
			name:        "missing_config_file",
			setupConfig: nil, // leave ~/.ssh/config absent
			host:        "myalias",
			wantPhrase:  "~/.ssh/config not found",
		},
		{
			name: "existing_setenv_conflict",
			setupConfig: func(t *testing.T, p string) {
				if err := os.WriteFile(p, []byte("Host myalias\n  HostName srv\n  SetEnv FOO=bar\n"), 0600); err != nil {
					t.Fatalf("write config: %v", err)
				}
			},
			host:       "myalias",
			wantPhrase: "already has a user-authored SetEnv directive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			sshDir := filepath.Join(home, ".ssh")
			if err := os.MkdirAll(sshDir, 0700); err != nil {
				t.Fatalf("mkdir .ssh: %v", err)
			}
			cfgPath := filepath.Join(sshDir, "config")
			if tc.setupConfig != nil {
				tc.setupConfig(t, cfgPath)
			}

			stdout, stderr := captureStdio(t, func() {
				applyLaptopSSHConfigSetEnv(tc.host, 18340, "/tmp/state")
			})

			if !strings.Contains(stderr, tc.wantPhrase) {
				t.Errorf("stderr missing %q; got:\n%s", tc.wantPhrase, stderr)
			}
			if !strings.Contains(stderr, tc.host) {
				t.Errorf("stderr missing host %q; got:\n%s", tc.host, stderr)
			}
			if strings.Contains(stdout, tc.wantPhrase) {
				t.Errorf("stdout should not carry warning; got:\n%s", stdout)
			}
			if strings.Contains(stdout, "applied cc-clip SetEnv block") {
				t.Errorf("unexpected success line on stdout: %q", stdout)
			}
		})
	}
}

// TestApplyLaptopSSHConfigSetEnvIsNotInRollback pins the intentional
// late-failure contract: once runConnect has called
// applyLaptopSSHConfigSetEnv, a subsequent Codex/notification-bridge
// failure does NOT revert the ssh_config SetEnv block. The tunnel is
// already live and the block accurately reflects remote state;
// reverting would strip working config on a transient late-stage glitch.
//
// A regression would be a refactor that moves applyLaptopSSHConfigSetEnv
// into a `rollback` / `failAfterSave` closure. We detect that source-level
// via the Go AST: every CallExpr of applyLaptopSSHConfigSetEnv must have
// the enclosing FuncDecl `runConnect` as its nearest function ancestor,
// NOT any intervening FuncLit (closure) — because a FuncLit inside
// runConnect is exactly the shape of a rollback callback.
//
// Using go/parser + ast.Inspect rather than a hand-rolled brace counter
// avoids false positives from raw string literals, rune literals that
// happen to contain `{` or `}`, or reformatted source.
func TestApplyLaptopSSHConfigSetEnvIsNotInRollback(t *testing.T) {
	fset := gotoken.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var runConnectDecl *ast.FuncDecl
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Recv == nil && fd.Name != nil && fd.Name.Name == "runConnect" {
			runConnectDecl = fd
			break
		}
	}
	if runConnectDecl == nil {
		t.Fatal("func runConnect(...) not found; rename or move broke the test")
	}

	foundInRunConnect := 0
	var stack []ast.Node
	ast.Inspect(runConnectDecl, func(n ast.Node) bool {
		if n == nil {
			// Pop on ascent.
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			return false
		}
		// Check BEFORE we push: a CallExpr at this point has `stack` equal to
		// its ancestors within runConnectDecl.
		if call, ok := n.(*ast.CallExpr); ok {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "applyLaptopSSHConfigSetEnv" {
				pos := fset.Position(call.Pos())
				if insideFuncLit(stack) {
					t.Errorf("applyLaptopSSHConfigSetEnv call at %s is inside a nested FuncLit (rollback/closure) — it must live directly in runConnect so late failures don't revert the ssh_config SetEnv block", pos)
				}
				foundInRunConnect++
			}
		}
		stack = append(stack, n)
		return true
	})

	if foundInRunConnect == 0 {
		t.Fatal("no applyLaptopSSHConfigSetEnv calls found in runConnect; the test is stale or the wiring regressed")
	}

	// Second invariant: the ONLY calls to applyLaptopSSHConfigSetEnv in
	// this package must live inside runConnect. A hidden rollback closure
	// defined in a sibling function would otherwise slip past the first
	// assertion. We do a full-file walk and count occurrences across all
	// function bodies; the count must equal what we counted inside
	// runConnect.
	totalCalls := 0
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "applyLaptopSSHConfigSetEnv" {
				totalCalls++
			}
		}
		return true
	})
	if totalCalls != foundInRunConnect {
		t.Errorf("found %d total applyLaptopSSHConfigSetEnv calls in main.go but only %d inside runConnect; the extra call(s) likely live in a rollback/closure elsewhere", totalCalls, foundInRunConnect)
	}
}

// insideFuncLit reports whether any ancestor in `stack` is a *ast.FuncLit
// — i.e. a function literal (closure) created inside the top-level
// FuncDecl being inspected. Used by TestApplyLaptopSSHConfigSetEnvIsNotInRollback.
func insideFuncLit(stack []ast.Node) bool {
	for _, n := range stack {
		if _, ok := n.(*ast.FuncLit); ok {
			return true
		}
	}
	return false
}

// TestRunConnectCallsApplyLaptopSSHConfigSetEnvOnBothPaths is an
// anti-regression guard for the P2 finding that the wiring between
// runConnect / --token-only and applyLaptopSSHConfigSetEnv was unpinned.
// Without this, a refactor that deletes either call site compiles and
// passes every unit test, even though interactive ssh sessions would
// then silently stop pushing CC_CLIP_PORT / CC_CLIP_STATE_DIR — breaking
// the multi-laptop shared-account feature.
//
// The guard is source-level (same shape as internal/setup's anti-feature
// test): locate runConnect's body and verify it contains at least two
// call sites, one inside a `return`-terminated branch (the token-only
// fast path) and one outside (the full-deploy path). A single call is
// insufficient — either branch missing the call would silently break
// that path, and both matter.
func TestRunConnectCallsApplyLaptopSSHConfigSetEnvOnBothPaths(t *testing.T) {
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	lines := strings.Split(string(data), "\n")

	startIdx := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "func runConnect(") {
			startIdx = i
			break
		}
	}
	if startIdx < 0 {
		t.Fatal("func runConnect(...) not found in main.go; test is stale if runConnect was renamed")
	}
	endIdx := -1
	for i := startIdx + 1; i < len(lines); i++ {
		if lines[i] == "}" {
			endIdx = i
			break
		}
	}
	if endIdx < 0 {
		t.Fatal("closing brace for runConnect not found; runConnect must end with a `}` at column 0")
	}

	body := lines[startIdx : endIdx+1]
	var callLines []int
	for i, line := range body {
		if strings.Contains(line, "applyLaptopSSHConfigSetEnv(") {
			callLines = append(callLines, i)
		}
	}

	if len(callLines) < 2 {
		t.Fatalf("runConnect must contain at least two calls to applyLaptopSSHConfigSetEnv (one inside `if tokenOnly` branch, one in the full-deploy path); got %d at line offsets %v", len(callLines), callLines)
	}

	// At least one call must be followed by a `return` within a few lines —
	// that is the token-only fast-path signature. Without it the test would
	// pass trivially on a refactor that kept only the full-path call site.
	// Accept both bare `return` (void era) and `return nil` (error-returning
	// era) since runConnect's signature changed when we routed the Codex
	// failure through an error return instead of os.Exit(1).
	foundReturnAfter := false
	for _, ci := range callLines {
		for j := ci + 1; j < len(body) && j <= ci+10; j++ {
			trimmed := strings.TrimSpace(body[j])
			if trimmed == "return" || trimmed == "return nil" {
				foundReturnAfter = true
				break
			}
			// Stop scanning if we hit another top-level directive.
			if strings.HasPrefix(trimmed, "fmt.Println(") || strings.HasPrefix(trimmed, "if ") {
				break
			}
		}
		if foundReturnAfter {
			break
		}
	}
	if !foundReturnAfter {
		t.Errorf("expected at least one applyLaptopSSHConfigSetEnv call followed closely by `return` (the token-only fast path); call offsets = %v", callLines)
	}
}

// TestRemoveLaptopSSHConfigSetEnvSymlinkRefused pins that Remove also
// surfaces the symlink warning (not just Apply) and leaves the symlink
// intact — replacing it with a regular file would detach a user's
// dotfiles silently.
func TestRemoveLaptopSSHConfigSetEnvSymlinkRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	real := filepath.Join(sshDir, "config.real")
	if err := os.WriteFile(real, []byte("Host myalias\n  HostName srv\n"), 0600); err != nil {
		t.Fatalf("write real: %v", err)
	}
	link := filepath.Join(sshDir, "config")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	stdout, stderr := captureStdio(t, func() {
		removeLaptopSSHConfigSetEnv("myalias")
	})

	if !strings.Contains(stderr, "~/.ssh/config is a symlink") {
		t.Errorf("expected symlink warning on stderr; got %q", stderr)
	}
	if strings.Contains(stdout, "~/.ssh/config is a symlink") {
		t.Errorf("stdout should not carry warning; got %q", stdout)
	}
	// Symlink must survive unchanged.
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat after remove: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was replaced with a regular file")
	}
}

// TestRemoveLaptopSSHConfigSetEnvMissingConfigIsSilent pins that
// removing when ~/.ssh/config is absent prints the success line (idempotent
// cleanup) with no warning on stderr — uninstall must not scare users who
// never had a config file.
func TestRemoveLaptopSSHConfigSetEnvMissingConfigIsSilent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// No ~/.ssh dir at all.

	stdout, stderr := captureStdio(t, func() {
		removeLaptopSSHConfigSetEnv("myalias")
	})

	if stderr != "" {
		t.Errorf("stderr should be empty for missing config, got %q", stderr)
	}
	if !strings.Contains(stdout, "Removed cc-clip SetEnv block") {
		t.Errorf("stdout should print success line, got %q", stdout)
	}
}

// TestLocalSSHConfigDisplayPathFallsBackWhenHomeUnresolvable pins the
// defensive "~/.ssh/config" fallback so success messages never print an
// empty path if $HOME is somehow unset.
func TestLocalSSHConfigDisplayPathFallsBackWhenHomeUnresolvable(t *testing.T) {
	// os.UserHomeDir returns an error when HOME is empty on Unix.
	if runtime.GOOS == "windows" {
		t.Skip("HOME semantics differ on Windows")
	}
	t.Setenv("HOME", "")
	got := localSSHConfigDisplayPath()
	if got == "" {
		t.Fatal("localSSHConfigDisplayPath returned empty string; should fall back to ~/.ssh/config")
	}
}

// TestClassifyErrorPreservesExitCodeSegments pins the P1-2 review fix: the
// error classifier must surface distinct exit codes for business-level
// failures (TokenInvalid=12, NoImage=10), tunnel/transport issues
// (TunnelUnreachable=11), and everything else (DownloadFailed=13). Before
// this fix, every non-business error collapsed to DownloadFailed, hiding
// tunnel flap signals from operators and tooling.
func TestClassifyErrorPreservesExitCodeSegments(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"token-invalid-wrapped", fmt.Errorf("wrap: %w", tunnel.ErrTokenInvalid), 12},
		{"no-image-wrapped", fmt.Errorf("wrap: %w", tunnel.ErrNoImage), 10},
		{"tunnel-not-found", fmt.Errorf("wrap: %w", tunnel.ErrTunnelNotFound), 11},
		{"context-deadline", fmt.Errorf("wrap: %w", context.DeadlineExceeded), 11},
		{"net-op-error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, 11},
		{"io-unexpected-eof", fmt.Errorf("wrap: %w", io.ErrUnexpectedEOF), 11},
		{"plain-string", errors.New("some daemon response garbage"), 13},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyError(tc.err)
			if got != tc.want {
				t.Fatalf("classifyError(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestValidateOrchestrationFlagsRejectsBadCombinations pins the rule that
// --token-only is mutually exclusive with --force, --codex, and --no-notify.
// The token-only branch in runConnect returns before Codex / notification
// deploy steps, so silent acceptance of the combinations would let
// `cc-clip connect host --token-only --codex` print "Setup complete" while
// never deploying Codex support.
func TestValidateOrchestrationFlagsRejectsBadCombinations(t *testing.T) {
	cases := []struct {
		name      string
		cmd       string
		force     bool
		tokenOnly bool
		codex     bool
		noNotify  bool
		wantSub   string
	}{
		{"connect-force-and-token-only", "connect", true, true, false, false, "--force and --token-only are mutually exclusive"},
		{"connect-token-only-and-codex", "connect", false, true, true, false, "--token-only cannot be combined with --codex"},
		{"connect-token-only-and-no-notify", "connect", false, true, false, true, "--token-only cannot be combined with --no-notify"},
		{"setup-force-and-token-only", "setup", true, true, false, false, "--force and --token-only are mutually exclusive"},
		{"setup-token-only-and-codex", "setup", false, true, true, false, "--token-only cannot be combined with --codex"},
		{"setup-token-only-and-no-notify", "setup", false, true, false, true, "--token-only cannot be combined with --no-notify"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateOrchestrationFlags(tc.cmd, tc.force, tc.tokenOnly, tc.codex, tc.noNotify)
			if got == "" {
				t.Fatalf("expected validation error, got empty string for %+v", tc)
			}
			if !strings.HasPrefix(got, tc.cmd+":") {
				t.Fatalf("error should be prefixed with %q, got %q", tc.cmd+":", got)
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("error %q does not contain %q", got, tc.wantSub)
			}
		})
	}
}

func TestValidateOrchestrationFlagsAcceptsValidCombinations(t *testing.T) {
	cases := []struct {
		name      string
		force     bool
		tokenOnly bool
		codex     bool
		noNotify  bool
	}{
		{"all-false", false, false, false, false},
		{"force-only", true, false, false, false},
		{"token-only", false, true, false, false},
		{"codex-only", false, false, true, false},
		{"no-notify-only", false, false, false, true},
		{"force-and-codex", true, false, true, false},
		{"force-and-no-notify", true, false, false, true},
		{"force-codex-and-no-notify", true, false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateOrchestrationFlags("connect", tc.force, tc.tokenOnly, tc.codex, tc.noNotify); got != "" {
				t.Fatalf("expected combination to validate, got error: %q", got)
			}
		})
	}
}
