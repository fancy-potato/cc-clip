package tunnel

import (
	"errors"
	"fmt"
	"testing"
)

func TestTunnelProcessExitedOrChangedWithTreatsMissingProcessAsExited(t *testing.T) {
	cfg := TunnelConfig{Host: "example", LocalPort: 18339, RemotePort: 19001}

	exited, err := tunnelProcessExitedOrChangedWith(func(int) (string, error) {
		return "", fmt.Errorf("%w: pid %d", errTunnelProcessNotRunning, 4321)
	}, 4321, cfg)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !exited {
		t.Fatal("expected exited=true")
	}
}

func TestTunnelProcessExitedOrChangedWithReturnsLookupError(t *testing.T) {
	cfg := TunnelConfig{Host: "example", LocalPort: 18339, RemotePort: 19001}
	wantErr := errors.New("ps failed")

	exited, err := tunnelProcessExitedOrChangedWith(func(int) (string, error) {
		return "", wantErr
	}, 4321, cfg)
	if exited {
		t.Fatal("expected exited=false")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapped %v", err, wantErr)
	}
}

func TestRetryTunnelProcessLookupRetriesIndeterminateErrors(t *testing.T) {
	attempts := 0
	got, err := retryTunnelProcessLookup(func(int) (string, error) {
		attempts++
		if attempts < 3 {
			return "", fmt.Errorf("%w: transient", errTunnelProcessLookupIndeterminate)
		}
		return "ssh -N example", nil
	}, 4321, 3, 0)
	if err != nil {
		t.Fatalf("retryTunnelProcessLookup err = %v, want nil", err)
	}
	if got != "ssh -N example" {
		t.Fatalf("cmdline = %q, want %q", got, "ssh -N example")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRetryTunnelProcessLookupReturnsLastIndeterminateError(t *testing.T) {
	attempts := 0
	_, err := retryTunnelProcessLookup(func(int) (string, error) {
		attempts++
		return "", fmt.Errorf("%w: still indeterminate", errTunnelProcessLookupIndeterminate)
	}, 4321, 2, 0)
	if !errors.Is(err, errTunnelProcessLookupIndeterminate) {
		t.Fatalf("err = %v, want errTunnelProcessLookupIndeterminate", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}
