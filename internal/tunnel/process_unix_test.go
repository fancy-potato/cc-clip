//go:build !windows

package tunnel

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestTunnelProcessCommandLineArgsUseWideOutput(t *testing.T) {
	got := tunnelProcessCommandLineArgs(4321)
	want := []string{"-ww", "-o", "command=", "-p", "4321"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tunnelProcessCommandLineArgs(4321) = %v, want %v", got, want)
	}
}

func TestSignalTunnelProcessIgnoresAlreadyExitedProcess(t *testing.T) {
	cfg := TunnelConfig{Host: "example", LocalPort: 18339, RemotePort: 19001}

	called := 0
	err := signalTunnelProcessWith(func(os.Signal) error {
		called++
		return syscall.ESRCH
	}, func(int) (string, error) {
		t.Fatal("lookup should not be called when ESRCH is returned")
		return "", nil
	}, 4321, cfg, syscall.SIGTERM)
	if err != nil {
		t.Fatalf("signalTunnelProcess returned error: %v", err)
	}
	if called != 1 {
		t.Fatalf("signal called %d times, want 1", called)
	}
}

func TestSignalTunnelProcessReturnsErrorWhenProcessStillMatches(t *testing.T) {
	cfg := TunnelConfig{Host: "example", LocalPort: 18339, RemotePort: 19001}

	// Use a managed-form cmdline — matchesTunnelProcess requires the
	// -F cc-clip-ssh-config- anchor plus the full managed -o set to
	// recognise a process as ours.
	const managed = "/usr/bin/ssh -F /tmp/cc-clip-ssh-config-abc.conf -N -v " +
		"-o BatchMode=yes -o ExitOnForwardFailure=yes " +
		"-o ServerAliveInterval=15 -o ServerAliveCountMax=3 " +
		"-o ControlMaster=no -o ControlPath=none " +
		"-R 19001:127.0.0.1:18339 example"

	err := signalTunnelProcessWith(func(os.Signal) error {
		return errors.New("permission denied")
	}, func(int) (string, error) {
		return managed, nil
	}, os.Getpid(), cfg, syscall.SIGTERM)
	if err == nil || !strings.Contains(err.Error(), "signal pid") || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want wrapped signal failure", err)
	}
}

func TestSignalTunnelProcessReturnsLookupErrorWhenInspectionFails(t *testing.T) {
	cfg := TunnelConfig{Host: "example", LocalPort: 18339, RemotePort: 19001}
	wantErr := errors.New("ps failed")

	err := signalTunnelProcessWith(func(os.Signal) error {
		return errors.New("permission denied")
	}, func(int) (string, error) {
		return "", wantErr
	}, os.Getpid(), cfg, syscall.SIGTERM)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapped %v", err, wantErr)
	}
}

func TestSignalTunnelProcessGroupTargetsProcessGroup(t *testing.T) {
	gotPID := 0
	gotSig := syscall.Signal(0)

	err := signalTunnelProcessGroupWith(func(pid int, sig syscall.Signal) error {
		gotPID = pid
		gotSig = sig
		return nil
	}, 4321, syscall.SIGTERM)
	if err != nil {
		t.Fatalf("signalTunnelProcessGroupWith: %v", err)
	}
	if gotPID != -4321 {
		t.Fatalf("pid = %d, want %d", gotPID, -4321)
	}
	if gotSig != syscall.SIGTERM {
		t.Fatalf("sig = %v, want %v", gotSig, syscall.SIGTERM)
	}
}

func TestShouldSignalTunnelProcessSkipsOnInspectError(t *testing.T) {
	if shouldSignalTunnelProcess(4321, "SIGTERM", false, errors.New("ps failed")) {
		t.Fatal("shouldSignalTunnelProcess returned true on inspect error, want false")
	}
}

func TestShouldSignalTunnelProcessSkipsOnMismatch(t *testing.T) {
	if shouldSignalTunnelProcess(4321, "SIGKILL", false, nil) {
		t.Fatal("shouldSignalTunnelProcess returned true on mismatch, want false")
	}
}

func TestShouldSignalTunnelProcessAllowsMatchedTunnel(t *testing.T) {
	if !shouldSignalTunnelProcess(4321, "SIGTERM", true, nil) {
		t.Fatal("shouldSignalTunnelProcess returned false on matching tunnel, want true")
	}
}
