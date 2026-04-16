package doctor

import (
	"fmt"
	"testing"

	"github.com/shunmei/cc-clip/internal/peer"
	"github.com/shunmei/cc-clip/internal/shim"
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

func TestCheckAliasPortSkipsWithoutPeerReservation(t *testing.T) {
	got := checkAliasPort("myserver", nil, 18339)
	if len(got) != 1 || !got[0].OK {
		t.Fatalf("expected alias check skip success, got %#v", got)
	}
	if got[0].Message != "peer SSH forwarding not configured; skipping SSH config port check" {
		t.Fatalf("unexpected alias skip message: %#v", got)
	}
}

func TestCheckAliasPortPrefersBaseHostConfig(t *testing.T) {
	oldQuery := sshConfigQuery
	t.Cleanup(func() { sshConfigQuery = oldQuery })

	var seen []string
	sshConfigQuery = func(candidate string) (string, error) {
		seen = append(seen, candidate)
		if candidate == "myserver" {
			return "remoteforward 18340 127.0.0.1:18339\n", nil
		}
		return "", fmt.Errorf("unexpected fallback")
	}

	got := checkAliasPort("myserver", &peer.Registration{Label: "macbook", ReservedPort: 18340}, 18339)
	if len(got) != 1 || !got[0].OK {
		t.Fatalf("expected success, got %#v", got)
	}
	if got[0].Message != "myserver forwards 18340 127.0.0.1:18339" {
		t.Fatalf("unexpected message %#v", got[0])
	}
	if len(seen) != 1 || seen[0] != "myserver" {
		t.Fatalf("expected only base host query, got %v", seen)
	}
}

func TestCheckAliasPortSurfacesSSHQueryError(t *testing.T) {
	oldQuery := sshConfigQuery
	t.Cleanup(func() { sshConfigQuery = oldQuery })

	sshConfigQuery = func(candidate string) (string, error) {
		return "", fmt.Errorf("ssh config parse error")
	}

	got := checkAliasPort("myserver", &peer.Registration{Label: "macbook", ReservedPort: 18340}, 18339)
	if len(got) != 1 || got[0].OK {
		t.Fatalf("expected failure, got %#v", got)
	}
	if got[0].Message != "ssh -G myserver failed: ssh config parse error; cannot verify RemoteForward" {
		t.Fatalf("unexpected message %#v", got[0])
	}
}

func TestCheckAliasPortFailsWhenForwardMissing(t *testing.T) {
	oldQuery := sshConfigQuery
	t.Cleanup(func() { sshConfigQuery = oldQuery })

	sshConfigQuery = func(candidate string) (string, error) {
		return "hostname 10.0.0.1\nuser admin\n", nil
	}

	got := checkAliasPort("myserver", &peer.Registration{Label: "macbook", ReservedPort: 18340}, 18339)
	if len(got) != 1 || got[0].OK {
		t.Fatalf("expected failure, got %#v", got)
	}
	if got[0].Message != "ssh config missing RemoteForward 18340 127.0.0.1:18339" {
		t.Fatalf("unexpected message %#v", got[0])
	}
}

func TestMatchesRemoteForwardAcceptsBracketedLoopback(t *testing.T) {
	line := "remoteforward 18340 [127.0.0.1]:18339"
	if !matchesRemoteForward(line, 18340, "127.0.0.1", 18339) {
		t.Fatalf("expected bracketed loopback RemoteForward to match: %q", line)
	}
}

func TestMatchesRemoteForwardAcceptsPlainLoopback(t *testing.T) {
	line := "remoteforward 18340 127.0.0.1:18339"
	if !matchesRemoteForward(line, 18340, "127.0.0.1", 18339) {
		t.Fatalf("expected plain loopback RemoteForward to match: %q", line)
	}
}

func TestMatchesRemoteForwardRejectsWrongTarget(t *testing.T) {
	line := "remoteforward 18340 [127.0.0.1]:9999"
	if matchesRemoteForward(line, 18340, "127.0.0.1", 18339) {
		t.Fatalf("expected mismatched RemoteForward to be rejected: %q", line)
	}
}
