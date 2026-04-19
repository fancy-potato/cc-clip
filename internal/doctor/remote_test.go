package doctor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/shunmei/cc-clip/internal/peer"
	"github.com/shunmei/cc-clip/internal/shim"
	"github.com/shunmei/cc-clip/internal/tunnel"
)

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

func TestPeerLookupCheckResultFallsBackWhenPeerMissing(t *testing.T) {
	got := peerLookupCheckResult(nil, fmt.Errorf("peer show failed: peer peer-a not found"))
	if !got.OK || got.Message != "peer registry not configured on remote; using legacy state path" {
		t.Fatalf("expected missing peer fallback success, got %#v", got)
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
