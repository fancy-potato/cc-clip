package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shunmei/cc-clip/internal/daemon"
	"github.com/shunmei/cc-clip/internal/exitcode"
	"github.com/shunmei/cc-clip/internal/session"
	"github.com/shunmei/cc-clip/internal/token"
	"github.com/shunmei/cc-clip/internal/tunnel"
)

// setupLocalOnlyTokenDir routes the local-only tunnel-control token at a
// t.TempDir() copy of $HOME/.cache/cc-clip. localOnlyTokenDir refuses paths
// outside that prefix, so tests cannot set TokenDirOverride directly.
func setupLocalOnlyTokenDir(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cache := filepath.Join(home, ".cache", "cc-clip")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	prev := token.TokenDirOverride
	token.TokenDirOverride = cache
	t.Cleanup(func() { token.TokenDirOverride = prev })
}

func TestNewDaemonTunnelJSONRequestAddsAuthHeaders(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	controlToken, _, err := token.LoadOrGenerateTunnelControlToken()
	if err != nil {
		t.Fatalf("LoadOrGenerateTunnelControlToken: %v", err)
	}

	req, authed, err := newDaemonTunnelJSONRequest(http.MethodPost, "http://127.0.0.1:18339/tunnels/up", bytes.NewReader([]byte(`{}`)), true)
	if err != nil {
		t.Fatalf("newDaemonTunnelJSONRequest: %v", err)
	}
	if !authed {
		t.Fatal("expected request to carry tunnel control auth")
	}

	if got := req.Header.Get(tunnelControlAuthHeader); got != controlToken {
		t.Fatalf("%s = %q, want %q", tunnelControlAuthHeader, got, controlToken)
	}
	if got := req.Header.Get("User-Agent"); got != "cc-clip/tunnel" {
		t.Fatalf("User-Agent = %q, want %q", got, "cc-clip/tunnel")
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want %q", got, "application/json")
	}
}

func TestNewDaemonTunnelJSONRequestAllowsOfflineFallbackWhenTokenMissing(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	req, authed, err := newDaemonTunnelJSONRequest(http.MethodPost, "http://127.0.0.1:18339/tunnels/down", bytes.NewReader([]byte(`{}`)), false)
	if err != nil {
		t.Fatalf("newDaemonTunnelJSONRequest: %v", err)
	}
	if authed {
		t.Fatal("expected missing local tunnel token to skip auth header")
	}
	if got := req.Header.Get(tunnelControlAuthHeader); got != "" {
		t.Fatalf("%s = %q, want empty", tunnelControlAuthHeader, got)
	}
}

func TestTunnelControlHTTPClientDoesNotFollowRedirects(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	req, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := newTunnelControlHTTPClient(time.Second).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusTemporaryRedirect)
	}
	if redirected {
		t.Fatal("expected redirect target not to be contacted")
	}
}

func mustRegisterTunnelRoutes(t *testing.T, mux *http.ServeMux, mgr *tunnel.Manager, daemonPort int) string {
	t.Helper()
	if err := registerTunnelRoutes(mux, mgr, daemonPort); err != nil {
		t.Fatalf("registerTunnelRoutes: %v", err)
	}
	controlToken, err := token.ReadTunnelControlToken()
	if err != nil {
		t.Fatalf("ReadTunnelControlToken: %v", err)
	}
	return controlToken
}

func authorizeTunnelRouteRequest(req *http.Request, controlToken string) {
	req.Header.Set(tunnelControlAuthHeader, controlToken)
	req.Header.Set("User-Agent", "cc-clip/test")
}

func TestTunnelRoutesRequireAuth(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	stateDir := t.TempDir()
	mgr := tunnel.NewManager(stateDir)
	defer mgr.Shutdown()
	controlToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	unauthList, err := http.NewRequest(http.MethodGet, ts.URL+"/tunnels", nil)
	if err != nil {
		t.Fatalf("NewRequest unauth list: %v", err)
	}
	unauthList.Header.Set("User-Agent", "cc-clip/test")

	resp, err := http.DefaultClient.Do(unauthList)
	if err != nil {
		t.Fatalf("Do unauth list: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth list status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	authOnlyList, err := http.NewRequest(http.MethodGet, ts.URL+"/tunnels", nil)
	if err != nil {
		t.Fatalf("NewRequest auth-only list: %v", err)
	}
	authOnlyList.Header.Set("Authorization", "Bearer clipboard-token")
	authOnlyList.Header.Set("User-Agent", "cc-clip/test")

	resp, err = http.DefaultClient.Do(authOnlyList)
	if err != nil {
		t.Fatalf("Do auth-only list: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("auth-only list status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	authList, err := http.NewRequest(http.MethodGet, ts.URL+"/tunnels", nil)
	if err != nil {
		t.Fatalf("NewRequest auth list: %v", err)
	}
	authorizeTunnelRouteRequest(authList, controlToken)

	resp, err = http.DefaultClient.Do(authList)
	if err != nil {
		t.Fatalf("Do auth list: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth list status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	unauthUp, err := http.NewRequest(http.MethodPost, ts.URL+"/tunnels/up", bytes.NewReader([]byte(`{"host":"example","remote_port":18340,"local_port":18339}`)))
	if err != nil {
		t.Fatalf("NewRequest unauth up: %v", err)
	}
	unauthUp.Header.Set("User-Agent", "cc-clip/test")
	unauthUp.Header.Set("Content-Type", "application/json")

	resp, err = http.DefaultClient.Do(unauthUp)
	if err != nil {
		t.Fatalf("Do unauth up: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth up status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	authUp, err := http.NewRequest(http.MethodPost, ts.URL+"/tunnels/up", bytes.NewReader([]byte(`{"host":"example","remote_port":18340,"local_port":18339}`)))
	if err != nil {
		t.Fatalf("NewRequest auth up: %v", err)
	}
	authorizeTunnelRouteRequest(authUp, controlToken)
	authUp.Header.Set("Content-Type", "application/json")

	resp, err = http.DefaultClient.Do(authUp)
	if err != nil {
		t.Fatalf("Do auth up: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth up status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	unauthDown, err := http.NewRequest(http.MethodPost, ts.URL+"/tunnels/down", bytes.NewReader([]byte(`{"host":"example"}`)))
	if err != nil {
		t.Fatalf("NewRequest unauth down: %v", err)
	}
	unauthDown.Header.Set("User-Agent", "cc-clip/test")
	unauthDown.Header.Set("Content-Type", "application/json")

	resp, err = http.DefaultClient.Do(unauthDown)
	if err != nil {
		t.Fatalf("Do unauth down: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth down status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestTunnelListRouteReturnsStateLoadError(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	stateDir := filepath.Join(t.TempDir(), "states.json")
	if err := os.WriteFile(stateDir, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mgr := tunnel.NewManager(stateDir)
	defer mgr.Shutdown()
	controlToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/tunnels", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	authorizeTunnelRouteRequest(req, controlToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
}

func TestRegisterTunnelRoutesRequiresMux(t *testing.T) {
	if err := registerTunnelRoutes(nil, tunnel.NewManager(t.TempDir()), 18339); err == nil {
		t.Fatal("expected error")
	}
}

// TestUnknownTunnelsPathReturns401ForAuthenticatedLoopbackProbes pins two
// behaviors of the /tunnels/ catch-all:
//  1. A non-cc-clip User-Agent gets 403 (not 401), so the catch-all is not
//     a cheap "does this daemon exist" oracle for browsers that stumbled on
//     the loopback port.
//  2. A properly-shaped cc-clip request without the control token gets 401,
//     so CLI / SwiftBar clients still get a consistent authorization
//     challenge for any path under /tunnels/.
func TestUnknownTunnelsPathReturns401ForAuthenticatedLoopbackProbes(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	stateDir := t.TempDir()
	mgr := tunnel.NewManager(stateDir)
	defer mgr.Shutdown()
	if _, err := registerTunnelRoutesForTest(srv.Mux(), mgr, 18339); err != nil {
		t.Fatalf("registerTunnelRoutes: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Browser-like probe with no cc-clip User-Agent: must be 403, not 401.
	browserProbe, err := http.NewRequest(http.MethodGet, ts.URL+"/tunnels/bogus", nil)
	if err != nil {
		t.Fatalf("NewRequest browser probe: %v", err)
	}
	browserProbe.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(browserProbe)
	if err != nil {
		t.Fatalf("Do browser probe: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("browser catch-all status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	// Legitimate cc-clip-looking request without a token: 401.
	cliProbe, err := http.NewRequest(http.MethodGet, ts.URL+"/tunnels/bogus", nil)
	if err != nil {
		t.Fatalf("NewRequest cli probe: %v", err)
	}
	cliProbe.Header.Set("User-Agent", "cc-clip/test")
	resp, err = http.DefaultClient.Do(cliProbe)
	if err != nil {
		t.Fatalf("Do cli probe: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cli catch-all status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// registerTunnelRoutesForTest is a tiny adapter that returns the control
// token alongside the registration error so tests can avoid the separate
// `mustRegisterTunnelRoutes` + `ReadTunnelControlToken` dance when they
// only need one of the two. Keeps the real production API surface small.
func registerTunnelRoutesForTest(mux *http.ServeMux, mgr *tunnel.Manager, daemonPort int) (string, error) {
	if err := registerTunnelRoutes(mux, mgr, daemonPort); err != nil {
		return "", err
	}
	return token.ReadTunnelControlToken()
}

func TestTunnelUpDefaultsLocalPortToDaemonPort(t *testing.T) {
	requireSSHBinary(t)
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	stateDir := t.TempDir()
	mgr := tunnel.NewManager(stateDir)
	defer mgr.Shutdown()
	controlToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/tunnels/up", bytes.NewReader([]byte(`{"host":"example","remote_port":19001}`)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	authorizeTunnelRouteRequest(req, controlToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Poll for the state to reach an active status. Fail explicitly on
	// deadline so a slow runner produces a clear timeout message instead
	// of a confusing "status = Disconnected" failure further down.
	var state *tunnel.TunnelState
	reached := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, err = tunnel.LoadState(stateDir, "example", 18339)
		if err == nil && state != nil &&
			(state.Status == tunnel.StatusConnecting || state.Status == tunnel.StatusConnected) {
			reached = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state == nil {
		t.Fatalf("state never loaded from disk")
	}
	if !reached {
		t.Fatalf("status never reached Connecting/Connected within 2s; last observed: %q (LastError=%q)", state.Status, state.LastError)
	}
	if state.Config.LocalPort != 18339 {
		t.Fatalf("LocalPort = %d, want %d", state.Config.LocalPort, 18339)
	}
	if !state.Config.Enabled {
		t.Fatal("expected enabled=true")
	}
}

// requireSSHBinary skips the test when no `ssh` binary is available on
// PATH. Tests that drive tunnel.Manager.Up end up shelling out to ssh; a
// host without it would fail Start() and turn the active-state assertion
// into a flake. Mirrors the pattern in newIPv4TestServer.
func requireSSHBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skipf("ssh binary not available: %v", err)
	}
}

func TestTunnelUpRouteRejectsForeignLocalPort(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	stateDir := t.TempDir()
	mgr := tunnel.NewManager(stateDir)
	defer mgr.Shutdown()
	controlToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/tunnels/up", bytes.NewReader([]byte(`{"host":"example","remote_port":19001,"local_port":18444}`)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	authorizeTunnelRouteRequest(req, controlToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestTunnelDownRouteRejectsForeignLocalPort(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	stateDir := t.TempDir()
	mgr := tunnel.NewManager(stateDir)
	defer mgr.Shutdown()
	controlToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/tunnels/down", bytes.NewReader([]byte(`{"host":"example","local_port":18444}`)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	authorizeTunnelRouteRequest(req, controlToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestDaemonPortConfiguredExplicitlyIgnoresInvalidValues(t *testing.T) {
	oldArgs := os.Args
	os.Args = []string{"cc-clip", "tunnel", "up", "example", "--port", "bogus"}
	defer func() { os.Args = oldArgs }()

	t.Setenv("CC_CLIP_PORT", "not-a-port")
	if daemonPortConfiguredExplicitly() {
		t.Fatal("expected invalid flag/env values to be ignored")
	}
}

func TestResolveTunnelHostArgRejectsFlagToken(t *testing.T) {
	_, err := resolveTunnelHostArg([]string{"cc-clip", "tunnel", "up", "--remote-port"}, 3, "cc-clip tunnel up <host>", "--remote-port")
	if err == nil || !strings.Contains(err.Error(), "usage: cc-clip tunnel up <host>") {
		t.Fatalf("err = %v, want usage error", err)
	}
}

func TestResolveTunnelHostArgReturnsHost(t *testing.T) {
	host, err := resolveTunnelHostArg([]string{"cc-clip", "tunnel", "up", "example"}, 3, "cc-clip tunnel up <host>")
	if err != nil {
		t.Fatalf("resolveTunnelHostArg: %v", err)
	}
	if host != "example" {
		t.Fatalf("host = %q, want %q", host, "example")
	}
}

func TestResolveTunnelHostArgRejectsExtraPositionalArgument(t *testing.T) {
	_, err := resolveTunnelHostArg(
		[]string{"cc-clip", "tunnel", "up", "example", "extra", "--port", "18339"},
		3,
		"cc-clip tunnel up <host>",
		"--port",
	)
	if err == nil || !strings.Contains(err.Error(), `unexpected extra argument "extra"`) {
		t.Fatalf("err = %v, want extra positional error", err)
	}
}

func TestResolveTunnelUpPortsAdoptsSavedLocalPortWhenDaemonNotExplicit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18444,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	remotePort, daemonPort, err := resolveTunnelUpPorts("example", 0, 18339, false)
	if err != nil {
		t.Fatalf("resolveTunnelUpPorts: %v", err)
	}
	if remotePort != 19001 {
		t.Fatalf("remotePort = %d, want 19001", remotePort)
	}
	if daemonPort != 18444 {
		t.Fatalf("daemonPort = %d, want 18444", daemonPort)
	}
}

func TestResolveTunnelUpPortsRejectsHostWithMultipleSavedTunnels(t *testing.T) {
	// A host with multiple saved tunnels cannot be disambiguated by the
	// resolver itself. LoadStateByHost returns ErrAmbiguousTunnelState;
	// resolveTunnelUpPorts surfaces that error so the user picks a
	// specific daemon via --port / CC_CLIP_PORT.
	t.Setenv("HOME", t.TempDir())

	for _, tc := range []struct {
		localPort  int
		remotePort int
	}{
		{localPort: 18444, remotePort: 19001},
		{localPort: 18555, remotePort: 19002},
	} {
		if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
			Config: tunnel.TunnelConfig{
				Host:       "example",
				LocalPort:  tc.localPort,
				RemotePort: tc.remotePort,
				Enabled:    true,
			},
			Status: tunnel.StatusConnected,
		}); err != nil {
			t.Fatalf("SaveState(%d): %v", tc.localPort, err)
		}
	}

	_, _, err := resolveTunnelUpPorts("example", 0, 18339, false)
	if !errors.Is(err, tunnel.ErrAmbiguousTunnelState) {
		t.Fatalf("err = %v, want ErrAmbiguousTunnelState", err)
	}
}

// TestResolveTunnelUpPortsAmbiguousErrorIncludesHintSuffix pins the hint
// appended to the ambiguous-state error: operators hitting this branch need
// to be told how to proceed (pass --port or an explicit --remote-port) rather
// than just see the bare error. The suffix lives on the CLI side, not in the
// tunnel package, so a refactor that rewraps err could silently drop it.
func TestResolveTunnelUpPortsAmbiguousErrorIncludesHintSuffix(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, local := range []int{18444, 18555} {
		if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
			Config: tunnel.TunnelConfig{
				Host:       "example",
				LocalPort:  local,
				RemotePort: 19000 + local,
				Enabled:    true,
			},
			Status: tunnel.StatusConnected,
		}); err != nil {
			t.Fatalf("SaveState(%d): %v", local, err)
		}
	}

	_, _, err := resolveTunnelUpPorts("example", 0, 18339, false)
	if !errors.Is(err, tunnel.ErrAmbiguousTunnelState) {
		t.Fatalf("err = %v, want ErrAmbiguousTunnelState", err)
	}
	msg := err.Error()
	for _, want := range []string{"pass --port <local-port>", "select the owning daemon"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing hint %q", msg, want)
		}
	}
}

// TestResolveTunnelUpPortsRejectsCorruptStateWithZeroLocalPort pins the
// defensive branch that guards against a hand-edited or legacy state file
// whose LocalPort is 0. The resolver must not silently adopt whatever
// --port the user passed unless the operator explicitly bypasses saved-state
// lookup with --remote-port.
func TestResolveTunnelUpPortsRejectsCorruptStateWithZeroLocalPort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// SaveState rejects LocalPort=0, so fabricate the file on disk directly.
	// StateFilePath encodes the zero local port in both the filename and the
	// filename-vs-contents hash, so LoadAllStates will accept this entry.
	dir := tunnel.DefaultStateDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := tunnel.StateFilePath(dir, "example", 0)
	payload := []byte(`{"config":{"host":"example","local_port":0,"remote_port":19001,"enabled":true}}`)
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, _, err := resolveTunnelUpPorts("example", 0, 18339, false)
	if err == nil {
		t.Fatal("expected error for state with zero local_port")
	}
	msg := err.Error()
	for _, want := range []string{
		"no local_port",
		"cc-clip connect example",
		"--remote-port",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
	if !errors.Is(err, errTunnelUpPortResolutionUsage) {
		t.Fatalf("err = %v, want errTunnelUpPortResolutionUsage", err)
	}
}

// TestCannotDetermineRemotePortMessageWording pins the actionable wording
// emitted by cmdTunnelUp when the host has no saved tunnel state. Install
// scripts, docs, and operator muscle memory rely on the message naming
// `cc-clip connect <host>` as the fix and `--remote-port` as the manual
// override. A future refactor that drops either reference would silently
// downgrade the operator UX from "here's exactly what to type" to a
// generic "tunnel up failed".
func TestCannotDetermineRemotePortMessageWording(t *testing.T) {
	got := cannotDetermineRemotePortMessage("myserver")
	for _, want := range []string{
		"cannot determine remote port for myserver",
		"cc-clip connect myserver",
		"--remote-port",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("message %q missing %q", got, want)
		}
	}
}

// TestResolveTunnelUpPortsReturnsZeroRemoteWhenNoStateExists pins the
// precondition for the cmdTunnelUp "cannot determine remote port" error.
// When the operator runs `cc-clip tunnel up <host>` for a host that was
// never `cc-clip connect`-ed, resolveTunnelUpPorts must return remotePort=0
// and no error so cmdTunnelUp can surface the actionable message that names
// `cc-clip connect <host>` as the fix. A future refactor that returns a
// generic error here would hide the actionable wording behind a
// "tunnel up failed:" stderr that doesn't tell the operator what to do.
func TestResolveTunnelUpPortsReturnsZeroRemoteWhenNoStateExists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remote, daemon, err := resolveTunnelUpPorts("never-connected-host", 0, 18339, false)
	if err != nil {
		t.Fatalf("expected nil error so cmdTunnelUp emits its connect-first message; got %v", err)
	}
	if remote != 0 {
		t.Fatalf("remote = %d, want 0 (so cmdTunnelUp triggers the cannot-determine-remote-port branch)", remote)
	}
	if daemon != 18339 {
		t.Fatalf("daemon = %d, want 18339 (caller-supplied default preserved)", daemon)
	}
}

func TestResolveTunnelUpPortsRequiresExplicitDaemonWhenRemotePortExplicitAndOwnerAmbiguous(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, local := range []int{18444, 18555} {
		if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
			Config: tunnel.TunnelConfig{
				Host:       "example",
				LocalPort:  local,
				RemotePort: 19000 + local,
				Enabled:    true,
			},
			Status: tunnel.StatusConnected,
		}); err != nil {
			t.Fatalf("SaveState(%d): %v", local, err)
		}
	}

	_, _, err := resolveTunnelUpPorts("example", 29999, 18339, false)
	if err == nil {
		t.Fatal("expected ambiguity error when explicit --remote-port cannot infer daemon owner")
	}
	if !errors.Is(err, tunnel.ErrAmbiguousTunnelState) {
		t.Fatalf("err = %v, want ErrAmbiguousTunnelState", err)
	}
	for _, want := range []string{"--port <local-port>", "multiple saved daemon owners exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestResolveTunnelUpPortsRejectsImplicitDaemonWhenZeroLocalPortStateAndRemotePortExplicit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := tunnel.DefaultStateDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := tunnel.StateFilePath(dir, "example", 0)
	payload := []byte(`{"config":{"host":"example","local_port":0,"remote_port":19001,"enabled":true}}`)
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, _, err := resolveTunnelUpPorts("example", 29999, 18339, false)
	if err == nil || !strings.Contains(err.Error(), "--port <local-port> explicitly") {
		t.Fatalf("err = %v, want explicit daemon guidance", err)
	}
	if !errors.Is(err, errTunnelUpPortResolutionUsage) {
		t.Fatalf("err = %v, want errTunnelUpPortResolutionUsage", err)
	}
}

func TestResolveTunnelUpPortsAllowsExplicitDaemonWhenZeroLocalPortStateAndRemotePortExplicit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := tunnel.DefaultStateDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := tunnel.StateFilePath(dir, "example", 0)
	payload := []byte(`{"config":{"host":"example","local_port":0,"remote_port":19001,"enabled":true}}`)
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	remotePort, daemonPort, err := resolveTunnelUpPorts("example", 29999, 18444, true)
	if err != nil {
		t.Fatalf("resolveTunnelUpPorts: %v", err)
	}
	if remotePort != 29999 || daemonPort != 18444 {
		t.Fatalf("got remote=%d daemon=%d, want remote=29999 daemon=18444", remotePort, daemonPort)
	}
}

func TestResolveTunnelUpPortsAdoptsSavedDaemonWhenRemotePortExplicit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18444,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	remotePort, daemonPort, err := resolveTunnelUpPorts("example", 29999, 18339, false)
	if err != nil {
		t.Fatalf("resolveTunnelUpPorts: %v", err)
	}
	if remotePort != 29999 || daemonPort != 18444 {
		t.Fatalf("got remote=%d daemon=%d, want remote=29999 daemon=18444", remotePort, daemonPort)
	}
}

func TestResolveTunnelUpPortsIgnoresZeroLocalPortStateWhenRealOwnerExists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18444,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	dir := tunnel.DefaultStateDir()
	path := tunnel.StateFilePath(dir, "example", 0)
	payload := []byte(`{"config":{"host":"example","local_port":0,"remote_port":29998,"enabled":true}}`)
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	remotePort, daemonPort, err := resolveTunnelUpPorts("example", 0, 18339, false)
	if err != nil {
		t.Fatalf("resolveTunnelUpPorts: %v", err)
	}
	if remotePort != 19001 || daemonPort != 18444 {
		t.Fatalf("got remote=%d daemon=%d, want remote=19001 daemon=18444", remotePort, daemonPort)
	}
}

func TestResolveTunnelUpPortsRejectsExplicitDaemonPortThatDiffersFromSavedLocalPort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18444,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	_, _, err := resolveTunnelUpPorts("example", 0, 19999, true)
	if err == nil || !strings.Contains(err.Error(), "uses local port 18444") {
		t.Fatalf("err = %v, want saved-local-port mismatch error", err)
	}
	if !errors.Is(err, errTunnelUpPortResolutionUsage) {
		t.Fatalf("err = %v, want errTunnelUpPortResolutionUsage", err)
	}
}

func TestResolveTunnelUpPortsRejectsEnvConfiguredDaemonPortThatDiffersFromSavedLocalPort(t *testing.T) {
	// CC_CLIP_PORT is the only non-flag path that flips
	// daemonPortConfiguredExplicitly() to true. Cover the env→explicit
	// mapping end-to-end so a regression that drops the env branch in
	// configuredPort() does not silently cross daemons during `tunnel up`.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CC_CLIP_PORT", "19999")

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18444,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	daemonPort := getPort()
	if daemonPort != 19999 {
		t.Fatalf("getPort() = %d, want 19999", daemonPort)
	}
	if !daemonPortConfiguredExplicitly() {
		t.Fatal("daemonPortConfiguredExplicitly() = false, want true when CC_CLIP_PORT is set")
	}

	_, _, err := resolveTunnelUpPorts("example", 0, daemonPort, daemonPortConfiguredExplicitly())
	if err == nil || !strings.Contains(err.Error(), "uses local port 18444") {
		t.Fatalf("err = %v, want saved-local-port mismatch error", err)
	}
	if !errors.Is(err, errTunnelUpPortResolutionUsage) {
		t.Fatalf("err = %v, want errTunnelUpPortResolutionUsage", err)
	}
}

func TestResolveTunnelUpPortsRejectsSavedTunnelOnDifferentExplicitDaemonWhenRemotePortExplicit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18444,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	_, _, err := resolveTunnelUpPorts("example", 19001, 18339, true)
	if err == nil || !strings.Contains(err.Error(), "uses local port 18444") {
		t.Fatalf("err = %v, want saved-local-port mismatch error", err)
	}
	if strings.Contains(err.Error(), "--remote-port") {
		t.Fatalf("error should not suggest --remote-port here anymore: %v", err)
	}
	if !strings.Contains(err.Error(), "--port 18444") {
		t.Fatalf("error should point at the owning daemon port, got: %v", err)
	}
	if !errors.Is(err, errTunnelUpPortResolutionUsage) {
		t.Fatalf("err = %v, want errTunnelUpPortResolutionUsage", err)
	}
}

// TestResolveTunnelUpPortsRequiresExplicitPortWhenAmbiguousIncludesDefaultDaemon
// covers: two saved tunnel states for the same host (daemon ports 18339
// and 18444), and the operator invokes `cc-clip tunnel up <host>` with no
// --port flag — so `daemonPortExplicit=false`. Resolution must NOT silently
// pick 18339 (the builtin default) just because it happens to match one of
// the saved states; it must error with "pass --port <local-port> explicitly"
// so the operator picks one deliberately.
func TestResolveTunnelUpPortsRequiresExplicitPortWhenAmbiguousIncludesDefaultDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, local := range []int{18339, 18444} {
		if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
			Config: tunnel.TunnelConfig{
				Host:       "example",
				LocalPort:  local,
				RemotePort: 19000 + local,
				Enabled:    true,
			},
			Status: tunnel.StatusConnected,
		}); err != nil {
			t.Fatalf("SaveState(%d): %v", local, err)
		}
	}

	_, _, err := resolveTunnelUpPorts("example", 29999, 18339, false)
	if err == nil || !strings.Contains(err.Error(), "pass --port <local-port> explicitly") {
		t.Fatalf("err = %v, want explicit --port guidance", err)
	}
	if !errors.Is(err, tunnel.ErrAmbiguousTunnelState) {
		t.Fatalf("err = %v, want ErrAmbiguousTunnelState", err)
	}
}

func TestResolveTunnelUpPortsAllowsExplicitDaemonWhenAmbiguousAndSelected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, tc := range []struct {
		localPort  int
		remotePort int
	}{
		{18339, 19001},
		{18444, 19002},
	} {
		if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
			Config: tunnel.TunnelConfig{
				Host:       "example",
				LocalPort:  tc.localPort,
				RemotePort: tc.remotePort,
				Enabled:    true,
			},
			Status: tunnel.StatusConnected,
		}); err != nil {
			t.Fatalf("SaveState(%d): %v", tc.localPort, err)
		}
	}

	remotePort, daemonPort, err := resolveTunnelUpPorts("example", 29999, 18444, true)
	if err != nil {
		t.Fatalf("resolveTunnelUpPorts: %v", err)
	}
	if remotePort != 29999 || daemonPort != 18444 {
		t.Fatalf("got remote=%d daemon=%d, want remote=29999 daemon=18444", remotePort, daemonPort)
	}
}

func TestTunnelDownRouteReturnsNotFoundForMissingTunnel(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	stateDir := t.TempDir()
	mgr := tunnel.NewManager(stateDir)
	defer mgr.Shutdown()
	controlToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/tunnels/down", bytes.NewReader([]byte(`{"host":"missing","local_port":18339}`)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	authorizeTunnelRouteRequest(req, controlToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestTunnelDownRouteRejectsMissingLocalPort(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	mgr := tunnel.NewManager(t.TempDir())
	defer mgr.Shutdown()
	controlToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, route := range []string{"/tunnels/down", "/tunnels/remove"} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+route, bytes.NewReader([]byte(`{"host":"missing"}`)))
		if err != nil {
			t.Fatalf("NewRequest %s: %v", route, err)
		}
		authorizeTunnelRouteRequest(req, controlToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do %s: %v", route, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (missing local_port)", route, resp.StatusCode)
		}
	}
}

func TestTunnelListRouteIncludesAllSavedTunnels(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	stateDir := t.TempDir()
	mgr := tunnel.NewManager(stateDir)
	defer mgr.Shutdown()
	controlToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	for _, state := range []*tunnel.TunnelState{
		{
			Config: tunnel.TunnelConfig{Host: "owned", LocalPort: 18339, RemotePort: 19001, Enabled: true},
			Status: tunnel.StatusConnected,
		},
		{
			Config: tunnel.TunnelConfig{Host: "foreign", LocalPort: 18444, RemotePort: 19002, Enabled: true},
			Status: tunnel.StatusDisconnected,
		},
	} {
		if err := tunnel.SaveState(stateDir, state); err != nil {
			t.Fatalf("SaveState(%s): %v", state.Config.Host, err)
		}
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/tunnels", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	authorizeTunnelRouteRequest(req, controlToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var states []*tunnel.TunnelState
	if err := json.NewDecoder(resp.Body).Decode(&states); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("len(states) = %d, want 2", len(states))
	}
	if states[0].Config.Host != "foreign" || states[1].Config.Host != "owned" {
		t.Fatalf("hosts = [%q %q], want [foreign owned]", states[0].Config.Host, states[1].Config.Host)
	}
}

func TestTunnelRoutesRejectOutOfRangePorts(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	mgr := tunnel.NewManager(t.TempDir())
	defer mgr.Shutdown()
	controlToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{name: "negative remote", path: "/tunnels/up", body: `{"host":"example","remote_port":-1}`},
		{name: "too large remote", path: "/tunnels/up", body: `{"host":"example","remote_port":65536}`},
		{name: "negative local on up", path: "/tunnels/up", body: `{"host":"example","remote_port":19001,"local_port":-1}`},
		{name: "too large local on up", path: "/tunnels/up", body: `{"host":"example","remote_port":19001,"local_port":65536}`},
		{name: "negative local on down", path: "/tunnels/down", body: `{"host":"example","local_port":-1}`},
		{name: "too large local on down", path: "/tunnels/down", body: `{"host":"example","local_port":65536}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL+tc.path, bytes.NewReader([]byte(tc.body)))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			authorizeTunnelRouteRequest(req, controlToken)
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
			}
		})
	}
}

func TestTunnelRoutesRejectTrailingJSONData(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	mgr := tunnel.NewManager(t.TempDir())
	defer mgr.Shutdown()
	controlToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, body := range []string{
		`{"host":"example","remote_port":19001}garbage`,
		`{"host":"example","remote_port":19001}{"host":"other","remote_port":19002}`,
	} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/tunnels/up", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		authorizeTunnelRouteRequest(req, controlToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d for body %q", resp.StatusCode, http.StatusBadRequest, body)
		}
	}
}

func TestPostTunnelDownReturnsErrTunnelNotFoundOn404(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	if _, _, err := token.LoadOrGenerateTunnelControlToken(); err != nil {
		t.Fatalf("LoadOrGenerateTunnelControlToken: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The CLI performs a preflight GET /tunnels without auth before
		// attaching the tunnel-control token. Respond 401 here so the
		// preflight succeeds; the real assertion is on the authenticated
		// /tunnels/down 404 response.
		if r.URL.Path == "/tunnels" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/tunnels/down" {
			t.Fatalf("path = %q, want /tunnels/down", r.URL.Path)
		}
		http.Error(w, "tunnel not found: missing", http.StatusNotFound)
	}))
	defer ts.Close()

	hostPort := strings.TrimPrefix(ts.URL, "http://")
	_, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", hostPort, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}

	err = postTunnelDown(port, "missing")
	if !errors.Is(err, tunnel.ErrTunnelNotFound) {
		t.Fatalf("err = %v, want ErrTunnelNotFound", err)
	}
}

func TestPostTunnelRemoveRefusesUnknownListenerBeforeSendingToken(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	if _, _, err := token.LoadOrGenerateTunnelControlToken(); err != nil {
		t.Fatalf("LoadOrGenerateTunnelControlToken: %v", err)
	}

	removeCalls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tunnels":
			http.NotFound(w, r)
		case "/tunnels/remove":
			removeCalls++
			t.Fatalf("preflight should have refused the listener before POST /tunnels/remove")
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer ts.Close()

	hostPort := strings.TrimPrefix(ts.URL, "http://")
	_, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", hostPort, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}

	err = postTunnelRemove(port, "example")
	if err == nil || !errors.Is(err, errDaemonTunnelListUnreachable) {
		t.Fatalf("err = %v, want errDaemonTunnelListUnreachable", err)
	}
	if removeCalls != 0 {
		t.Fatalf("removeCalls = %d, want 0", removeCalls)
	}
}

func TestPostTunnelUpMissingTokenUsesTunnelControlExitCode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cache := filepath.Join(home, ".cache", "cc-clip")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	prev := token.TokenDirOverride
	token.TokenDirOverride = cache
	t.Cleanup(func() { token.TokenDirOverride = prev })

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tunnels" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	hostPort := strings.TrimPrefix(ts.URL, "http://")
	_, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", hostPort, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}

	err = postTunnelUp(port, "example", 19001)
	if err == nil || !errors.Is(err, errDaemonTunnelControlTokenUnavailable) {
		t.Fatalf("err = %v, want errDaemonTunnelControlTokenUnavailable", err)
	}
	if got := exitCodeForTunnelError(err); got != exitcode.DaemonTunnelControlTokenUnavailable {
		t.Fatalf("exitCodeForTunnelError = %d, want %d", got, exitcode.DaemonTunnelControlTokenUnavailable)
	}
}

func TestPostTunnelUpReturnsUnavailableForMissingRoute(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	if _, _, err := token.LoadOrGenerateTunnelControlToken(); err != nil {
		t.Fatalf("LoadOrGenerateTunnelControlToken: %v", err)
	}

	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()

	hostPort := strings.TrimPrefix(ts.URL, "http://")
	_, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", hostPort, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}

	err = postTunnelUp(port, "missing", 19001)
	if !errors.Is(err, errDaemonTunnelListUnreachable) {
		t.Fatalf("err = %v, want errDaemonTunnelListUnreachable", err)
	}
}

func TestPostTunnelDownReturnsUnavailableForMissingRoute(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	if _, _, err := token.LoadOrGenerateTunnelControlToken(); err != nil {
		t.Fatalf("LoadOrGenerateTunnelControlToken: %v", err)
	}

	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()

	hostPort := strings.TrimPrefix(ts.URL, "http://")
	_, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", hostPort, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}

	err = postTunnelDown(port, "missing")
	if !errors.Is(err, errDaemonTunnelListUnreachable) {
		t.Fatalf("err = %v, want errDaemonTunnelListUnreachable", err)
	}
}

func TestNormalizeOfflineTunnelStates(t *testing.T) {
	now := time.Now()
	states := []*tunnel.TunnelState{
		{
			Config: tunnel.TunnelConfig{Host: "enabled", Enabled: true},
			Status: tunnel.StatusConnected,
			PID:    4321,
		},
		{
			Config:    tunnel.TunnelConfig{Host: "disabled", Enabled: false},
			Status:    tunnel.StatusConnected,
			PID:       1234,
			StartedAt: &now,
		},
	}

	got := normalizeOfflineTunnelStates(states)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Status != tunnel.StatusDisconnected {
		t.Fatalf("enabled status = %q, want %q", got[0].Status, tunnel.StatusDisconnected)
	}
	if got[0].PID != 0 {
		t.Fatalf("enabled PID = %d, want 0", got[0].PID)
	}
	if got[1].Status != tunnel.StatusStopped {
		t.Fatalf("disabled status = %q, want %q", got[1].Status, tunnel.StatusStopped)
	}
	if got[1].PID != 0 {
		t.Fatalf("disabled PID = %d, want 0", got[1].PID)
	}
	if states[0].Status != tunnel.StatusConnected || states[0].PID != 4321 {
		t.Fatal("normalizeOfflineTunnelStates mutated the input slice")
	}
}

func TestPersistTunnelDownOffline(t *testing.T) {
	stateDir := t.TempDir()
	original := &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18339,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status:    tunnel.StatusConnected,
		PID:       1234,
		LastError: "previous error",
	}
	if err := tunnel.SaveState(stateDir, original); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if err := persistTunnelDownOfflineWithDir(stateDir, "example", 18339, offlineDownDeps{}); err != nil {
		t.Fatalf("persistTunnelDownOfflineWithDir: %v", err)
	}

	got, err := tunnel.LoadState(stateDir, "example", 18339)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Config.Enabled {
		t.Fatal("expected enabled=false")
	}
	if got.Status != tunnel.StatusStopped {
		t.Fatalf("status = %q, want %q", got.Status, tunnel.StatusStopped)
	}
	if got.PID != 0 {
		t.Fatalf("PID = %d, want 0", got.PID)
	}
	if got.LastError != "" {
		t.Fatalf("LastError = %q, want empty", got.LastError)
	}
	if got.StoppedAt == nil {
		t.Fatal("StoppedAt was not set")
	}
}

func TestPersistTunnelDownOfflineStopsRecordedProcess(t *testing.T) {
	stateDir := t.TempDir()
	original := &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18339,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
		PID:    4321,
	}
	if err := tunnel.SaveState(stateDir, original); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	cleanupCalls := 0
	err := persistTunnelDownOfflineWithDir(stateDir, "example", 18339, offlineDownDeps{
		cleanupFn: func(pid int, cfg tunnel.TunnelConfig) error {
			cleanupCalls++
			if pid != 4321 {
				t.Fatalf("pid = %d, want 4321", pid)
			}
			if cfg.Host != "example" {
				t.Fatalf("host = %q, want %q", cfg.Host, "example")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("persistTunnelDownOfflineWithDir: %v", err)
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanupCalls = %d, want 1", cleanupCalls)
	}
}

func TestPersistTunnelDownOfflineFindsRunningProcessWhenPIDMissing(t *testing.T) {
	stateDir := t.TempDir()
	original := &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18339,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnecting,
	}
	if err := tunnel.SaveState(stateDir, original); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	findCalls := 0
	cleanupCalls := 0
	err := persistTunnelDownOfflineWithDir(stateDir, "example", 18339, offlineDownDeps{
		cleanupFn: func(pid int, cfg tunnel.TunnelConfig) error {
			cleanupCalls++
			if pid != 4321 {
				t.Fatalf("pid = %d, want 4321", pid)
			}
			if cfg.Host != "example" {
				t.Fatalf("host = %q, want example", cfg.Host)
			}
			return nil
		},
		findFn: func(cfg tunnel.TunnelConfig) (int, bool, error) {
			findCalls++
			if cfg.Host != "example" {
				t.Fatalf("host = %q, want example", cfg.Host)
			}
			return 4321, true, nil
		},
	})
	if err != nil {
		t.Fatalf("persistTunnelDownOfflineWithDir: %v", err)
	}
	if findCalls != 1 {
		t.Fatalf("findCalls = %d, want 1", findCalls)
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanupCalls = %d, want 1", cleanupCalls)
	}
}

func TestPersistTunnelDownOfflineReturnsCleanupError(t *testing.T) {
	stateDir := t.TempDir()
	original := &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18339,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
		PID:    4321,
	}
	if err := tunnel.SaveState(stateDir, original); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	wantErr := errors.New("still running")
	err := persistTunnelDownOfflineWithDir(stateDir, "example", 18339, offlineDownDeps{
		cleanupFn: func(int, tunnel.TunnelConfig) error {
			return wantErr
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapped %v", err, wantErr)
	}

	got, loadErr := tunnel.LoadState(stateDir, "example", 18339)
	if loadErr != nil {
		t.Fatalf("LoadState: %v", loadErr)
	}
	if !got.Config.Enabled {
		t.Fatal("expected enabled=true after cleanup failure")
	}
	if got.Status != tunnel.StatusConnected {
		t.Fatalf("status = %q, want %q", got.Status, tunnel.StatusConnected)
	}
	if got.PID != 4321 {
		t.Fatalf("PID = %d, want 4321", got.PID)
	}
}

func TestPersistTunnelDownOfflineMissingState(t *testing.T) {
	err := persistTunnelDownOfflineWithDir(t.TempDir(), "missing", 18339, offlineDownDeps{})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

func TestStopTunnelFallsBackOfflineOnlyWhenDaemonUnreachable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	postCalls := 0
	persistCalls := 0

	err := stopTunnelWith(18339, "example",
		func(_ int, _ string) error {
			postCalls++
			return fmt.Errorf("%w (dial tcp 127.0.0.1:18339: connect: connection refused)", errDaemonUnreachable)
		},
		func(host string, localPort int) error {
			persistCalls++
			if host != "example" {
				t.Fatalf("host = %q, want %q", host, "example")
			}
			if localPort != 18339 {
				t.Fatalf("localPort = %d, want 18339", localPort)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("stopTunnelWith: %v", err)
	}
	if postCalls != 1 {
		t.Fatalf("postCalls = %d, want 1", postCalls)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
}

func TestDaemonHTTPErrorTreatsManagerShutdownAsRecoverable(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Body:       io.NopCloser(strings.NewReader(tunnel.ErrManagerShuttingDown.Error())),
	}

	err := daemonHTTPError(resp)
	if !errors.Is(err, errDaemonManagerShuttingDown) {
		t.Fatalf("err = %v, want errDaemonManagerShuttingDown", err)
	}
}

func TestStopTunnelFallsBackOfflineWhenManagerIsShuttingDown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	postCalls := 0
	persistCalls := 0

	err := stopTunnelWith(18339, "example",
		func(_ int, _ string) error {
			postCalls++
			return fmt.Errorf("%w: daemon returned 503: %s", errDaemonManagerShuttingDown, tunnel.ErrManagerShuttingDown.Error())
		},
		func(host string, localPort int) error {
			persistCalls++
			if host != "example" {
				t.Fatalf("host = %q, want %q", host, "example")
			}
			if localPort != 18339 {
				t.Fatalf("localPort = %d, want 18339", localPort)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("stopTunnelWith: %v", err)
	}
	if postCalls != 1 {
		t.Fatalf("postCalls = %d, want 1", postCalls)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
}

func TestStopTunnelFallsBackOfflineWhenTunnelControlRouteIsUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	postCalls := 0
	persistCalls := 0

	err := stopTunnelWith(18339, "example",
		func(_ int, _ string) error {
			postCalls++
			return fmt.Errorf("%w: daemon returned 404: Not Found", errDaemonTunnelControlUnavailable)
		},
		func(host string, localPort int) error {
			persistCalls++
			if host != "example" {
				t.Fatalf("host = %q, want %q", host, "example")
			}
			if localPort != 18339 {
				t.Fatalf("localPort = %d, want 18339", localPort)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("stopTunnelWith: %v", err)
	}
	if postCalls != 1 {
		t.Fatalf("postCalls = %d, want 1", postCalls)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
}

func TestStopTunnelReturnsAuthErrorWhenDaemonAuthFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	postCalls := 0
	persistCalls := 0

	err := stopTunnelWith(18339, "example",
		func(_ int, _ string) error {
			postCalls++
			return fmt.Errorf("%w: daemon returned 401: missing authorization", errDaemonAuth)
		},
		func(host string, localPort int) error {
			persistCalls++
			if host != "example" {
				t.Fatalf("host = %q, want %q", host, "example")
			}
			if localPort != 18339 {
				t.Fatalf("localPort = %d, want 18339", localPort)
			}
			return nil
		},
	)
	if err == nil || !errors.Is(err, errDaemonAuth) {
		t.Fatalf("err = %v, want daemon auth error", err)
	}
	if postCalls != 1 {
		t.Fatalf("postCalls = %d, want 1", postCalls)
	}
	if persistCalls != 0 {
		t.Fatalf("persistCalls = %d, want 0", persistCalls)
	}
}

func TestStopTunnelIgnoresMissingTunnelInDaemon(t *testing.T) {
	persistCalls := 0

	err := stopTunnelWith(18339, "example",
		func(_ int, _ string) error {
			return fmt.Errorf("%w: tunnel example not found", tunnel.ErrTunnelNotFound)
		},
		func(_ string, _ int) error {
			persistCalls++
			return os.ErrNotExist
		},
	)
	if err != nil {
		t.Fatalf("stopTunnelWith: %v", err)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
}

func TestStopTunnelRetriesSavedDaemonPortWhenInitialPortReportsNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
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
	err := stopTunnelWith(18339, "example",
		func(port int, _ string) error {
			ports = append(ports, port)
			if port == 18444 {
				return nil
			}
			return fmt.Errorf("%w: tunnel example not found", tunnel.ErrTunnelNotFound)
		},
		func(string, int) error {
			persistCalls++
			return nil
		},
	)
	if err != nil {
		t.Fatalf("stopTunnelWith: %v", err)
	}
	if got, want := fmt.Sprint(ports), "[18339 18444]"; got != want {
		t.Fatalf("ports = %s, want %s", got, want)
	}
	if persistCalls != 0 {
		t.Fatalf("persistCalls = %d, want 0", persistCalls)
	}
}

func TestStopTunnelRetriesSavedDaemonPortBeforeOfflineFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupLocalOnlyTokenDir(t)

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
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
	err := stopTunnelWith(18339, "example",
		func(port int, _ string) error {
			ports = append(ports, port)
			if port == 18444 {
				return nil
			}
			return fmt.Errorf("%w (dial tcp 127.0.0.1:%d: connect: connection refused)", errDaemonUnreachable, port)
		},
		func(string, int) error {
			persistCalls++
			return nil
		},
	)
	if err != nil {
		t.Fatalf("stopTunnelWith: %v", err)
	}
	if got, want := fmt.Sprint(ports), "[18339 18444]"; got != want {
		t.Fatalf("ports = %s, want %s", got, want)
	}
	if persistCalls != 0 {
		t.Fatalf("persistCalls = %d, want 0", persistCalls)
	}
}

func TestStopTunnelPersistsOfflineWhenSavedDaemonPortAlsoUnreachable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupLocalOnlyTokenDir(t)

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18444,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	persistCalls := 0
	persistPort := 0
	err := stopTunnelWith(18339, "example",
		func(port int, _ string) error {
			return fmt.Errorf("%w (dial tcp 127.0.0.1:%d: connect: connection refused)", errDaemonUnreachable, port)
		},
		func(_ string, localPort int) error {
			persistCalls++
			persistPort = localPort
			return nil
		},
	)
	if err != nil {
		t.Fatalf("stopTunnelWith: %v", err)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
	if persistPort != 18444 {
		t.Fatalf("persistPort = %d, want 18444", persistPort)
	}
}

func TestStopTunnelRetriesDifferentSavedPortWhenDaemonReportsNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18444,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var ports []int
	err := stopTunnelWith(18339, "example",
		func(port int, _ string) error {
			ports = append(ports, port)
			if port == 18444 {
				return nil
			}
			return fmt.Errorf("%w: tunnel example not found", tunnel.ErrTunnelNotFound)
		},
		func(string, int) error {
			t.Fatal("persistFn should not be called when saved-port retry succeeds")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("stopTunnelWith: %v", err)
	}
	if got, want := fmt.Sprint(ports), "[18339 18444]"; got != want {
		t.Fatalf("ports = %s, want %s", got, want)
	}
}

func TestFallbackDaemonPortRejectsAmbiguousSavedPorts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, port := range []int{18444, 18555} {
		if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
			Config: tunnel.TunnelConfig{
				Host:       "example",
				LocalPort:  port,
				RemotePort: 19001,
				Enabled:    true,
			},
			Status: tunnel.StatusConnected,
		}); err != nil {
			t.Fatalf("SaveState(%d): %v", port, err)
		}
	}

	_, retry, err := fallbackDaemonPort("example", 18339)
	if err == nil || !strings.Contains(err.Error(), "--port") {
		t.Fatalf("err = %v, want ambiguity guidance", err)
	}
	if retry {
		t.Fatal("retry should be false for ambiguous saved ports")
	}
}

func TestFallbackDaemonPortKeepsRequestedPortWhenSavedOwnerMatches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, port := range []int{18339, 18444} {
		if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
			Config: tunnel.TunnelConfig{
				Host:       "example",
				LocalPort:  port,
				RemotePort: 19001,
				Enabled:    true,
			},
			Status: tunnel.StatusConnected,
		}); err != nil {
			t.Fatalf("SaveState(%d): %v", port, err)
		}
	}

	_, retry, err := fallbackDaemonPort("example", 18339)
	if err != nil {
		t.Fatalf("err = %v, want nil when requested port already owns a saved state", err)
	}
	if retry {
		t.Fatal("retry should be false when requested port already matches a saved owner")
	}
}

func TestFallbackDaemonPortSurfacesStateLoadError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	statePath := tunnel.DefaultStateDir()
	if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, retry, err := fallbackDaemonPort("example", 18339)
	if err == nil || !strings.Contains(err.Error(), "load saved tunnels") {
		t.Fatalf("err = %v, want state load error", err)
	}
	if retry {
		t.Fatal("retry should be false on state load error")
	}
}

func TestFallbackDaemonPortRejectsLegacyStatesWithoutLocalPort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := tunnel.DefaultStateDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := tunnel.StateFilePath(dir, "example", 0)
	payload := []byte(`{"config":{"host":"example","local_port":0,"remote_port":19001,"enabled":true}}`)
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, retry, err := fallbackDaemonPort("example", 18339)
	if err == nil || !strings.Contains(err.Error(), "have no local_port") {
		t.Fatalf("err = %v, want explicit legacy-state guidance", err)
	}
	if retry {
		t.Fatal("retry should be false for legacy states without a local_port owner")
	}
}

func TestStopTunnelDoesNotPersistOfflineForDaemonHTTPError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupLocalOnlyTokenDir(t)

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
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

	err := stopTunnelWith(18339, "example",
		func(port int, _ string) error {
			ports = append(ports, port)
			return errors.New("daemon returned 401: missing authorization")
		},
		func(string, int) error {
			persistCalls++
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if got, want := fmt.Sprint(ports), "[18339]"; got != want {
		t.Fatalf("ports = %s, want %s", got, want)
	}
	if persistCalls != 0 {
		t.Fatalf("persistCalls = %d, want 0", persistCalls)
	}
}

func TestLoadTunnelStatesForListFallsBackToDiskWhenTokenMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupLocalOnlyTokenDir(t)

	port := reserveLocalPort(t)
	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  port,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	states, err := loadTunnelStatesForList(port)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(states) != 1 {
		t.Fatalf("states len = %d, want 1", len(states))
	}
	if states[0].Status != tunnel.StatusDisconnected || states[0].PID != 0 {
		t.Fatalf("states = %+v, want normalized offline state", states)
	}
}

func TestStopTunnelFallsBackOfflineWithoutTokenWhenDaemonUnreachable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupLocalOnlyTokenDir(t)

	port := reserveLocalPort(t)
	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  port,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if err := stopTunnel(port, "example", false); err != nil {
		t.Fatalf("stopTunnel: %v", err)
	}

	got, err := tunnel.LoadState(tunnel.DefaultStateDir(), "example", port)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Config.Enabled {
		t.Fatal("expected enabled=false")
	}
	if got.Status != tunnel.StatusStopped {
		t.Fatalf("status = %q, want %q", got.Status, tunnel.StatusStopped)
	}
}

func TestLoadTunnelStatesForListFallsBackOfflineOnlyWhenDaemonUnreachable(t *testing.T) {
	fetchCalls := 0
	loadCalls := 0
	offline := []*tunnel.TunnelState{
		{
			Config: tunnel.TunnelConfig{Host: "example", Enabled: true},
			Status: tunnel.StatusConnected,
			PID:    4321,
		},
	}

	states, err := loadTunnelStatesForListWith(18339,
		func(int) ([]*tunnel.TunnelState, error) {
			fetchCalls++
			return nil, fmt.Errorf("%w (dial tcp 127.0.0.1:18339: connect: connection refused)", errDaemonUnreachable)
		},
		func(string) ([]*tunnel.TunnelState, error) {
			loadCalls++
			return offline, nil
		},
	)
	if err != nil {
		t.Fatalf("loadTunnelStatesForListWith: %v", err)
	}
	if fetchCalls != 1 {
		t.Fatalf("fetchCalls = %d, want 1", fetchCalls)
	}
	if loadCalls != 1 {
		t.Fatalf("loadCalls = %d, want 1", loadCalls)
	}
	if len(states) != 1 || states[0].Status != tunnel.StatusDisconnected || states[0].PID != 0 {
		t.Fatalf("offline states were not normalized: %+v", states)
	}
}

func TestLoadTunnelStatesForListFallsBackToDiskWhenControlTokenUnavailable(t *testing.T) {
	loadCalls := 0

	states, err := loadTunnelStatesForListWith(18339,
		func(int) ([]*tunnel.TunnelState, error) {
			return nil, fmt.Errorf("%w; showing saved state only", errDaemonTunnelControlTokenUnavailable)
		},
		func(string) ([]*tunnel.TunnelState, error) {
			loadCalls++
			return []*tunnel.TunnelState{
				{
					Config: tunnel.TunnelConfig{Host: "example", Enabled: true},
					Status: tunnel.StatusConnected,
					PID:    4321,
				},
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if loadCalls != 1 {
		t.Fatalf("loadCalls = %d, want 1", loadCalls)
	}
	if len(states) != 1 || states[0].Status != tunnel.StatusDisconnected || states[0].PID != 0 {
		t.Fatalf("states = %+v, want normalized offline state", states)
	}
}

func TestLoadTunnelStatesForListReturnsAuthErrorWhenDaemonAuthFails(t *testing.T) {
	loadCalls := 0
	offline := []*tunnel.TunnelState{
		{
			Config: tunnel.TunnelConfig{Host: "example", Enabled: true},
			Status: tunnel.StatusConnected,
			PID:    4321,
		},
	}

	states, err := loadTunnelStatesForListWith(18339,
		func(int) ([]*tunnel.TunnelState, error) {
			return nil, fmt.Errorf("%w: daemon returned 401: missing authorization", errDaemonAuth)
		},
		func(string) ([]*tunnel.TunnelState, error) {
			loadCalls++
			return offline, nil
		},
	)
	if err == nil || !errors.Is(err, errDaemonAuth) {
		t.Fatalf("err = %v, want daemon auth error", err)
	}
	if loadCalls != 0 {
		t.Fatalf("loadCalls = %d, want 0", loadCalls)
	}
	if states != nil {
		t.Fatalf("states = %+v, want nil", states)
	}
}

func TestLoadTunnelStatesForListReturnsPrimaryDaemonStateWhenDiskLoadFails(t *testing.T) {
	states, err := loadTunnelStatesForListWith(18339,
		func(int) ([]*tunnel.TunnelState, error) {
			return []*tunnel.TunnelState{
				{
					Config: tunnel.TunnelConfig{Host: "example", LocalPort: 18339, RemotePort: 19001, Enabled: true},
					Status: tunnel.StatusConnected,
					PID:    4321,
				},
			}, nil
		},
		func(string) ([]*tunnel.TunnelState, error) {
			return nil, errors.New("state dir unreadable")
		},
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(states) != 1 {
		t.Fatalf("len(states) = %d, want 1", len(states))
	}
	if states[0].Config.Host != "example" || states[0].Status != tunnel.StatusConnected || states[0].PID != 4321 {
		t.Fatalf("states[0] = %+v, want connected primary daemon state", states[0])
	}
}

func TestLoadTunnelStatesForListSurfacesNonRecoverableDaemonHTTPError(t *testing.T) {
	loadCalls := 0

	_, err := loadTunnelStatesForListWith(18339,
		func(int) ([]*tunnel.TunnelState, error) {
			return nil, errors.New("daemon returned 500: internal error")
		},
		func(string) ([]*tunnel.TunnelState, error) {
			loadCalls++
			return nil, nil
		},
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if loadCalls != 0 {
		t.Fatalf("loadCalls = %d, want 0", loadCalls)
	}
}

func TestLoadTunnelStatesForListFallsBackToDiskWhenTunnelAPIUnavailable(t *testing.T) {
	loadCalls := 0

	states, err := loadTunnelStatesForListWith(18339,
		func(int) ([]*tunnel.TunnelState, error) {
			return nil, fmt.Errorf("%w: daemon returned 404: not found", errDaemonTunnelListUnreachable)
		},
		func(string) ([]*tunnel.TunnelState, error) {
			loadCalls++
			return []*tunnel.TunnelState{
				{
					Config: tunnel.TunnelConfig{Host: "example", Enabled: true},
					Status: tunnel.StatusConnected,
					PID:    4321,
				},
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if loadCalls != 1 {
		t.Fatalf("loadCalls = %d, want 1", loadCalls)
	}
	if len(states) != 1 || states[0].Status != tunnel.StatusDisconnected || states[0].PID != 0 {
		t.Fatalf("states = %+v, want normalized offline state", states)
	}
}

// TestLoadTunnelStatesForListOnlyQueriesSelectedDaemonPort pins the
// tunnel-control-token non-leak invariant: the CLI must send the local-only
// token to the selected daemon port and no other. Entries on disk whose
// LocalPort differs must fall through to their on-disk status rather than
// receiving a live token-bearing HTTP request.
func TestLoadTunnelStatesForListOnlyQueriesSelectedDaemonPort(t *testing.T) {
	offline := []*tunnel.TunnelState{
		{
			Config: tunnel.TunnelConfig{Host: "owned", LocalPort: 18339, RemotePort: 19001, Enabled: true},
			Status: tunnel.StatusDisconnected,
		},
		{
			Config: tunnel.TunnelConfig{Host: "foreign", LocalPort: 18444, RemotePort: 19002, Enabled: true},
			Status: tunnel.StatusDisconnected,
		},
	}

	calls := []int{}
	states, err := loadTunnelStatesForListWith(18339,
		func(port int) ([]*tunnel.TunnelState, error) {
			calls = append(calls, port)
			switch port {
			case 18339:
				return []*tunnel.TunnelState{
					{
						Config: tunnel.TunnelConfig{Host: "owned", LocalPort: 18339, RemotePort: 19001, Enabled: true},
						Status: tunnel.StatusConnected,
						PID:    111,
					},
				}, nil
			default:
				return nil, fmt.Errorf("unexpected port %d (must not be queried)", port)
			}
		},
		func(string) ([]*tunnel.TunnelState, error) {
			return offline, nil
		},
	)
	if err != nil {
		t.Fatalf("loadTunnelStatesForListWith: %v", err)
	}
	if got, want := fmt.Sprint(calls), "[18339]"; got != want {
		t.Fatalf("calls = %s, want %s (CLI must not fan tunnel-control token across ports)", got, want)
	}
	if len(states) != 2 {
		t.Fatalf("len(states) = %d, want 2", len(states))
	}
	if states[0].Config.Host != "foreign" || states[0].Status != tunnel.StatusDisconnected {
		t.Fatalf("foreign state = %+v, want disconnected from on-disk (no fan-out)", states[0])
	}
	if states[1].Config.Host != "owned" || states[1].Status != tunnel.StatusConnected || states[1].PID != 111 {
		t.Fatalf("owned state = %+v, want connected pid 111 from primary fetch", states[1])
	}
}

func TestFetchTunnelListReturnsUnavailableForMissingRoute(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	if _, _, err := token.LoadOrGenerateTunnelControlToken(); err != nil {
		t.Fatalf("LoadOrGenerateTunnelControlToken: %v", err)
	}

	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()

	hostPort := strings.TrimPrefix(ts.URL, "http://")
	_, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", hostPort, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}

	_, err = fetchTunnelList(port)
	if err == nil || !errors.Is(err, errDaemonTunnelListUnavailable) {
		t.Fatalf("err = %v, want daemon tunnel list unavailable", err)
	}
}

func reserveLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestRemoveTunnelFallsBackToDirectStateDeleteWhenDaemonUnreachable verifies
// CLAUDE.md's promise that `cc-clip tunnel remove` still succeeds when the
// daemon is down by deleting the on-disk state file via the persist callback.
// Without this test, a silent regression wrapping the fallback in an extra
// condition (e.g. "only if token present") would ship undetected.
func TestRemoveTunnelFallsBackToDirectStateDeleteWhenDaemonUnreachable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	postCalls := 0
	persistCalls := 0
	err := removeTunnelWith(18339, "example",
		func(_ int, _ string) error {
			postCalls++
			return fmt.Errorf("%w (dial tcp 127.0.0.1:18339: connect: connection refused)", errDaemonUnreachable)
		},
		func(host string, localPort int) error {
			persistCalls++
			if host != "example" {
				t.Fatalf("host = %q, want %q", host, "example")
			}
			if localPort != 18339 {
				t.Fatalf("localPort = %d, want 18339 (daemon port fallback)", localPort)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("removeTunnelWith: %v", err)
	}
	if postCalls != 1 {
		t.Fatalf("postCalls = %d, want 1", postCalls)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
}

// TestRemoveTunnelReturnsNilWhenDaemonUnknownAndStateMissing verifies that
// "already cleaned up" is treated as success — if the daemon reports
// ErrTunnelNotFound and the on-disk state file is also missing, the CLI
// exits 0.
func TestRemoveTunnelReturnsNilWhenDaemonUnknownAndStateMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	persistCalls := 0
	err := removeTunnelWith(18339, "example",
		func(_ int, _ string) error {
			return fmt.Errorf("%w: tunnel example not found", tunnel.ErrTunnelNotFound)
		},
		func(_ string, _ int) error {
			persistCalls++
			return os.ErrNotExist
		},
	)
	if err != nil {
		t.Fatalf("removeTunnelWith: %v", err)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
}

func TestRemoveTunnelRetriesSavedPortForHostOnlyScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18444,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var ports []int
	err := removeTunnelWith(18339, "example",
		func(port int, _ string) error {
			ports = append(ports, port)
			if port == 18444 {
				return nil
			}
			return fmt.Errorf("%w: tunnel example not found", tunnel.ErrTunnelNotFound)
		},
		func(string, int) error {
			t.Fatal("persistFn should not be called when saved-port retry succeeds")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("removeTunnelWith: %v", err)
	}
	if got, want := fmt.Sprint(ports), "[18339 18444]"; got != want {
		t.Fatalf("ports = %s, want %s", got, want)
	}
}

func TestRemoveTunnelDoesNotRetrySavedPortWhenDaemonPortWasExplicit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
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
	err := removeTunnelWithRetryPolicy(18339, "example", false,
		func(port int, _ string) error {
			ports = append(ports, port)
			return fmt.Errorf("%w: tunnel example not found", tunnel.ErrTunnelNotFound)
		},
		func(host string, localPort int) error {
			persistCalls++
			if host != "example" {
				t.Fatalf("host = %q, want %q", host, "example")
			}
			if localPort != 18339 {
				t.Fatalf("localPort = %d, want 18339", localPort)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("removeTunnelWithRetryPolicy: %v", err)
	}
	if got, want := fmt.Sprint(ports), "[18339]"; got != want {
		t.Fatalf("ports = %s, want %s", got, want)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
}

func TestRemoveTunnelOfflineFallbackUsesSavedPortForHostOnlyScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
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
	err := removeTunnelWith(18339, "example",
		func(port int, _ string) error {
			ports = append(ports, port)
			if port == 18444 {
				return fmt.Errorf("%w (dial tcp 127.0.0.1:%d: connect: connection refused)", errDaemonUnreachable, port)
			}
			return fmt.Errorf("%w: tunnel example not found", tunnel.ErrTunnelNotFound)
		},
		func(host string, localPort int) error {
			persistCalls++
			if host != "example" {
				t.Fatalf("host = %q, want %q", host, "example")
			}
			if localPort != 18444 {
				t.Fatalf("localPort = %d, want 18444 (saved port fallback)", localPort)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("removeTunnelWith: %v", err)
	}
	if got, want := fmt.Sprint(ports), "[18339 18444]"; got != want {
		t.Fatalf("ports = %s, want %s", got, want)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
}

// TestRemoveTunnelSurfacesNonRecoverableDaemonErrors verifies that, when
// postFn returns an error that is neither recoverable nor ErrTunnelNotFound,
// removeTunnelWith propagates it without touching the offline persist path.
func TestRemoveTunnelSurfacesNonRecoverableDaemonErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	persistCalls := 0
	want := errors.New("daemon returned 500: internal error")
	err := removeTunnelWith(18339, "example",
		func(_ int, _ string) error { return want },
		func(_ string, _ int) error {
			persistCalls++
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("err = %v, want containing %q", err, want)
	}
	if persistCalls != 0 {
		t.Fatalf("persistCalls = %d, want 0 (non-recoverable errors must not invoke offline fallback)", persistCalls)
	}
}

// TestTunnelRoutesRejectOversizedBody pins the 4 KiB MaxBytesReader cap:
// a body over the limit must return 413 and never reach the handler. This
// closes a DOS surface — the auth middleware runs before the body is read,
// so an authenticated attacker (or any caller who grabbed the control
// token file) must not be able to balloon daemon memory.
func TestTunnelRoutesRejectOversizedBody(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	mgr := tunnel.NewManager(t.TempDir())
	defer mgr.Shutdown()
	controlToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Build a well-formed JSON body that blows past the 4 KiB cap. Padding
	// inside an unknown field would be rejected at the DisallowUnknownFields
	// step before the size check; the host string field is a valid schema
	// location to stuff bytes into for this test.
	oversize := 8 * 1024
	payload := map[string]any{
		"host":        strings.Repeat("a", oversize),
		"remote_port": 19001,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(body) <= 4096 {
		t.Fatalf("test body size %d did not exceed 4 KiB cap", len(body))
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/tunnels/up", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	authorizeTunnelRouteRequest(req, controlToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

func TestIsCCClipUserAgent(t *testing.T) {
	cases := []struct {
		ua   string
		want bool
	}{
		{"cc-clip", true},
		{"cc-clip/tunnel", true},
		{"cc-clip/1.2.3", true},
		{"cc-clip 1.2.3", true},
		// Trailing-delimiter-only variants carry no identity: they look like
		// a client that half-built a UA string, and a browser that sniffed
		// the prefix check would find them trivial to produce. Reject.
		{"cc-clip/", false},
		{"cc-clip ", false},
		// Lookalike prefixes must not pass.
		{"cc-clipper", false},
		{"cc-clip-evil", false},
		{"cc-clip-evil/1", false},
		// Empty and wrong-scheme UAs.
		{"", false},
		{"Mozilla/5.0", false},
		{"curl/7.88.1", false},
	}
	for _, c := range cases {
		if got := isCCClipUserAgent(c.ua); got != c.want {
			t.Errorf("isCCClipUserAgent(%q) = %v, want %v", c.ua, got, c.want)
		}
	}
}

func TestIsLoopbackRemoteAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:18339", true},
		{"127.1.2.3:55555", true},
		{"[::1]:18339", true},
		// ::ffff:127.0.0.1 is the IPv4-mapped IPv6 loopback — still loopback.
		{"[::ffff:127.0.0.1]:18339", true},
		// Non-loopback peers must be rejected even if on the same LAN.
		{"10.0.0.1:18339", false},
		{"192.168.1.1:18339", false},
		{"[2001:db8::1]:18339", false},
		// Malformed / empty values must be rejected rather than permissive.
		{"", false},
		{"not-an-address", false},
	}
	for _, c := range cases {
		if got := isLoopbackRemoteAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackRemoteAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"localhost:18339", true},
		{"127.0.0.1:18339", true},
		{"[::1]:18339", true},
		{"[::ffff:127.0.0.1]:18339", true},
		// Bare hosts without a port are explicitly refused so a
		// `Host: localhost` header (not a shape any local HTTP client emits)
		// cannot slip past dns-rebinding protection.
		{"localhost", false},
		{"127.0.0.1", false},
		{"example.com:443", false},
		{"10.0.0.1:18339", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isLoopbackHost(c.host); got != c.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// TestStopTunnelRetriesAgainstAltDaemonPort verifies that when the CLI
// targets daemon 18339 but the only saved tunnel is owned by daemon
// 18444, stopTunnelWith discovers the alternate port via
// fallbackDaemonPort and retries the POST against it.
func TestStopTunnelRetriesAgainstAltDaemonPort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
			LocalPort:  18444,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: tunnel.StatusConnected,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var calls []int
	err := stopTunnelWith(18339, "example",
		func(daemonPort int, _ string) error {
			calls = append(calls, daemonPort)
			if daemonPort == 18444 {
				return nil
			}
			return fmt.Errorf("%w (dial tcp 127.0.0.1:%d: connect: connection refused)", errDaemonUnreachable, daemonPort)
		},
		func(string, int) error { return nil },
	)
	if err != nil {
		t.Fatalf("stopTunnelWith: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %+v, want 2 entries", calls)
	}
	if calls[1] != 18444 {
		t.Fatalf("retry call daemonPort = %d, want 18444", calls[1])
	}
}

func TestStopTunnelDoesNotRetrySavedPortWhenDaemonPortWasExplicit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := tunnel.SaveState(tunnel.DefaultStateDir(), &tunnel.TunnelState{
		Config: tunnel.TunnelConfig{
			Host:       "example",
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
	err := stopTunnelWithRetryPolicy(18339, "example", false,
		func(port int, _ string) error {
			ports = append(ports, port)
			return fmt.Errorf("%w: tunnel example not found", tunnel.ErrTunnelNotFound)
		},
		func(host string, localPort int) error {
			persistCalls++
			if host != "example" {
				t.Fatalf("host = %q, want %q", host, "example")
			}
			if localPort != 18339 {
				t.Fatalf("localPort = %d, want 18339", localPort)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("stopTunnelWithRetryPolicy: %v", err)
	}
	if got, want := fmt.Sprint(ports), "[18339]"; got != want {
		t.Fatalf("ports = %s, want %s", got, want)
	}
	if persistCalls != 1 {
		t.Fatalf("persistCalls = %d, want 1", persistCalls)
	}
}

// TestTunnelControlTokenReloadsPerRequest pins that the tunnel-control auth
// middleware re-reads the token file on every request. Earlier code closed
// over the token value at registration time, so any future runtime rotation
// path (a rotation endpoint, a file-watch reloader, a second `cc-clip serve
// --rotate-tunnel-token` invocation against the same daemon) would silently
// leave the running daemon accepting the old token. This test replaces the
// token on disk between two requests and verifies the old value stops
// working and the new value starts working, without restarting the server.
func TestTunnelControlTokenReloadsPerRequest(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	stateDir := t.TempDir()
	mgr := tunnel.NewManager(stateDir)
	defer mgr.Shutdown()
	originalToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	doList := func(tok string) int {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/tunnels", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		authorizeTunnelRouteRequest(req, tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if status := doList(originalToken); status != http.StatusOK {
		t.Fatalf("baseline list with original token = %d, want 200", status)
	}

	rotated, err := token.RotateTunnelControlToken()
	if err != nil {
		t.Fatalf("RotateTunnelControlToken: %v", err)
	}
	if rotated == originalToken {
		t.Fatalf("rotated token matches original; rotation did not produce a new value")
	}

	if status := doList(originalToken); status != http.StatusUnauthorized {
		t.Fatalf("post-rotate list with ORIGINAL token = %d, want 401 (middleware kept stale value)", status)
	}
	if status := doList(rotated); status != http.StatusOK {
		t.Fatalf("post-rotate list with ROTATED token = %d, want 200 (middleware did not re-read)", status)
	}
}

// TestTunnelRoutesRejectNonJSONContentType pins the CSRF-defense gate: a
// POST with Content-Type: application/x-www-form-urlencoded — the shape a
// browser form would use to dodge CORS preflight — must be rejected with
// 415 even when the caller is otherwise authenticated.
func TestTunnelRoutesRejectNonJSONContentType(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	stateDir := t.TempDir()
	mgr := tunnel.NewManager(stateDir)
	defer mgr.Shutdown()
	controlToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cases := []struct {
		name        string
		contentType string
	}{
		{name: "form urlencoded", contentType: "application/x-www-form-urlencoded"},
		{name: "plain text", contentType: "text/plain"},
		{name: "missing", contentType: ""},
		{name: "json-ish variant", contentType: "application/jsonx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.NewReader(`{"host":"foo","remote_port":19001}`)
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/tunnels/up", body)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			authorizeTunnelRouteRequest(req, controlToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnsupportedMediaType)
			}
		})
	}
}

// TestDecodeTunnelRequestRejectsEmptyJSONObject pins the empty-body guard:
// `{}` must be rejected with 400 rather than silently decoded into the
// zero-valued struct and passed to downstream validators. A future relaxation
// of ValidateSSHHost must not accidentally let `{}` through.
func TestDecodeTunnelRequestRejectsEmptyJSONObject(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	tm := token.NewManager(time.Hour)
	store := session.NewStore(12 * time.Hour)
	srv := daemon.NewServer("127.0.0.1:0", &testClipboard{}, tm, store)
	stateDir := t.TempDir()
	mgr := tunnel.NewManager(stateDir)
	defer mgr.Shutdown()
	controlToken := mustRegisterTunnelRoutes(t, srv.Mux(), mgr, 18339)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"/tunnels/up", "/tunnels/down", "/tunnels/remove"} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			authorizeTunnelRouteRequest(req, controlToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for {} body", resp.StatusCode)
			}
		})
	}
}

// TestExitCodeForTunnelErrorClassifiesSentinels pins the segmented-exit-code
// contract documented in exitcode.go. Scripted consumers (SwiftBar retry
// loops, install scripts, wrapper CLIs) branch on these codes; a silent
// reclassification breaks automation without a human-readable log line.
//
// Each case exercises a sentinel against a wrapped error shape that the
// real call sites emit (see fetchTunnelList, postTunnelDown, etc.), so the
// classifier is tested against the actual error shapes it has to handle.
func TestExitCodeForTunnelErrorClassifiesSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, exitcode.Success},
		{"manager shutting down", fmt.Errorf("%w: 503 shutting down", errDaemonManagerShuttingDown), exitcode.DaemonShuttingDown},
		{"ambiguous tunnel state", fmt.Errorf("%w: example", tunnel.ErrAmbiguousTunnelState), exitcode.AmbiguousTunnelState},
		{"tunnel not found", fmt.Errorf("%w: example", tunnel.ErrTunnelNotFound), exitcode.UsageError},
		{"control token unavailable (specific)", fmt.Errorf("%w; retry later", errDaemonTunnelControlTokenUnavailable), exitcode.DaemonTunnelControlTokenUnavailable},
		{"tunnel control unavailable", fmt.Errorf("%w: 503", errDaemonTunnelControlUnavailable), exitcode.DaemonTunnelControlTokenUnavailable},
		{"tunnel list unreachable (404/405)", fmt.Errorf("%w: daemon returned 404: not found", errDaemonTunnelListUnreachable), exitcode.TunnelUnreachable},
		{"tunnel list umbrella (legacy)", fmt.Errorf("%w", errDaemonTunnelListUnavailable), exitcode.DaemonTunnelControlTokenUnavailable},
		{"daemon unreachable", fmt.Errorf("%w (connection refused)", errDaemonUnreachable), exitcode.TunnelUnreachable},
		{"daemon auth", fmt.Errorf("%w: 401", errDaemonAuth), exitcode.TokenInvalid},
		{"unclassified", errors.New("some other failure"), exitcode.InternalError},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exitCodeForTunnelError(c.err); got != c.want {
				t.Fatalf("exitCodeForTunnelError(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

func TestIsTunnelUpPortResolutionUsageErrorClassifiesUsagePaths(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"usage sentinel", fmt.Errorf("%w: choose a daemon", errTunnelUpPortResolutionUsage), true},
		{"ambiguous state", fmt.Errorf("%w: example", tunnel.ErrAmbiguousTunnelState), true},
		{"load failure", errors.New("state dir unreadable"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTunnelUpPortResolutionUsageError(c.err); got != c.want {
				t.Fatalf("isTunnelUpPortResolutionUsageError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestExitCodeForTunnelErrorSpecificSentinelsStillMatchUmbrella documents
// the wire-contract promise: errDaemonTunnelControlTokenUnavailable and
// errDaemonTunnelListUnreachable wrap errDaemonTunnelListUnavailable, so
// downstream callers that match on the umbrella via errors.Is continue to
// work. If a future refactor makes the two specific sentinels standalone,
// this test fails loudly and the author must either adjust the caller or
// keep the wrap in place.
func TestExitCodeForTunnelErrorSpecificSentinelsStillMatchUmbrella(t *testing.T) {
	cases := []error{
		errDaemonTunnelControlTokenUnavailable,
		errDaemonTunnelListUnreachable,
	}
	for _, err := range cases {
		if !errors.Is(err, errDaemonTunnelListUnavailable) {
			t.Fatalf("errors.Is(%v, errDaemonTunnelListUnavailable) = false, want true", err)
		}
	}
}

// TestFetchTunnelListWrapsListUnreachableOn404 verifies that a 404 from
// GET /tunnels routes through the new errDaemonTunnelListUnreachable
// sentinel, so exitCodeForTunnelError emits TunnelUnreachable. Without
// this the P2-C split is defined in code but not actually reachable from
// the real HTTP call site (the feedback called out the "not just defined
// in exitcode.go and unreachable" audit requirement).
func TestFetchTunnelListWrapsListUnreachableOn404(t *testing.T) {
	setupLocalOnlyTokenDir(t)

	if _, _, err := token.LoadOrGenerateTunnelControlToken(); err != nil {
		t.Fatalf("LoadOrGenerateTunnelControlToken: %v", err)
	}

	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()

	hostPort := strings.TrimPrefix(ts.URL, "http://")
	_, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", hostPort, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}

	_, err = fetchTunnelList(port)
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !errors.Is(err, errDaemonTunnelListUnreachable) {
		t.Fatalf("err = %v, want errDaemonTunnelListUnreachable", err)
	}
	if got := exitCodeForTunnelError(err); got != exitcode.TunnelUnreachable {
		t.Fatalf("exitCodeForTunnelError = %d, want TunnelUnreachable=%d", got, exitcode.TunnelUnreachable)
	}
}

// TestFetchTunnelListWrapsControlTokenUnavailableWhenAuthMissing verifies
// that the "local control token missing" branch of fetchTunnelList routes
// through errDaemonTunnelControlTokenUnavailable, landing in the
// DaemonTunnelControlTokenUnavailable exit code. Together with the 404
// test above, this covers both P2-C call sites.
func TestFetchTunnelListWrapsControlTokenUnavailableWhenAuthMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Point the token store at the tmp home but do NOT generate a token;
	// ReadTunnelControlToken will return os.ErrNotExist.
	cache := filepath.Join(home, ".cache", "cc-clip")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	prev := token.TokenDirOverride
	token.TokenDirOverride = cache
	t.Cleanup(func() { token.TokenDirOverride = prev })

	// Point at any unreachable port — the function returns before dialing
	// because authed==false.
	_, err := fetchTunnelList(65500)
	if err == nil {
		t.Fatal("expected error when tunnel control token is missing")
	}
	if !errors.Is(err, errDaemonTunnelControlTokenUnavailable) {
		t.Fatalf("err = %v, want errDaemonTunnelControlTokenUnavailable", err)
	}
	if got := exitCodeForTunnelError(err); got != exitcode.DaemonTunnelControlTokenUnavailable {
		t.Fatalf("exitCodeForTunnelError = %d, want DaemonTunnelControlTokenUnavailable=%d", got, exitcode.DaemonTunnelControlTokenUnavailable)
	}
}
