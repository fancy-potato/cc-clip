package tunnel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizeHost(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"myhost", "myhost"},
		{"my.host.com", "my-host-com"},
		{"user@host", "user-host"},
		{"HOST-NAME", "host-name"},
		{"---", "unknown"},
		{"", "unknown"},
		{"a.b_c:d", "a-b-c-d"},
	}
	for _, tt := range tests {
		got := SanitizeHost(tt.input)
		if got != tt.want {
			t.Errorf("SanitizeHost(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSaveAndLoadState(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)

	original := &TunnelState{
		Config: TunnelConfig{
			Host:       "testhost",
			LocalPort:  18339,
			RemotePort: 18340,
			Enabled:    true,
		},
		Status:    StatusConnected,
		PID:       12345,
		StartedAt: &now,
	}

	if err := SaveState(dir, original); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Verify file exists.
	path := StateFilePath(dir, "testhost", 18339)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file missing: %v", err)
	}

	loaded, err := LoadState(dir, "testhost", 18339)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.Config.Host != "testhost" {
		t.Errorf("Host = %q, want testhost", loaded.Config.Host)
	}
	if loaded.Config.RemotePort != 18340 {
		t.Errorf("RemotePort = %d, want 18340", loaded.Config.RemotePort)
	}
	if loaded.Status != StatusConnected {
		t.Errorf("Status = %q, want connected", loaded.Status)
	}
	if loaded.PID != 12345 {
		t.Errorf("PID = %d, want 12345", loaded.PID)
	}
}

func TestStateFilePathDoesNotCollideForPunctuationVariants(t *testing.T) {
	dir := t.TempDir()
	pathA := StateFilePath(dir, "my.host", 18339)
	pathB := StateFilePath(dir, "my-host", 18339)
	pathC := StateFilePath(dir, "my_host", 18339)
	pathD := StateFilePath(dir, "my.host", 18444)

	if pathA == pathB || pathA == pathC || pathB == pathC || pathA == pathD {
		t.Fatalf("state paths collided: %q %q %q %q", pathA, pathB, pathC, pathD)
	}
}

func TestLoadAllStates(t *testing.T) {
	dir := t.TempDir()

	hosts := []string{"alpha", "beta", "gamma"}
	for _, h := range hosts {
		s := &TunnelState{
			Config: TunnelConfig{Host: h, LocalPort: 18339, RemotePort: 18340, Enabled: true},
			Status: StatusStopped,
		}
		if err := SaveState(dir, s); err != nil {
			t.Fatalf("SaveState(%s): %v", h, err)
		}
	}

	// Write a non-JSON file to verify it's skipped.
	os.WriteFile(filepath.Join(dir, "README.txt"), []byte("not json"), 0644)

	states, err := LoadAllStates(dir)
	if err != nil {
		t.Fatalf("LoadAllStates: %v", err)
	}
	if len(states) != 3 {
		t.Fatalf("got %d states, want 3", len(states))
	}
}

func TestSaveStateRejectsInvalidState(t *testing.T) {
	err := SaveState(t.TempDir(), &TunnelState{
		Config: TunnelConfig{
			Host:       "",
			LocalPort:  18339,
			RemotePort: 18340,
		},
		Status: StatusStopped,
	})
	if err == nil || !strings.Contains(err.Error(), "missing host") {
		t.Fatalf("err = %v, want missing-host validation", err)
	}
}

func TestSaveStateRejectsZeroLocalPort(t *testing.T) {
	err := SaveState(t.TempDir(), &TunnelState{
		Config: TunnelConfig{
			Host:       "example",
			LocalPort:  0,
			RemotePort: 18340,
		},
		Status: StatusStopped,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid local port 0") {
		t.Fatalf("err = %v, want zero-local-port validation", err)
	}
}

func TestLoadAllStatesEmptyDir(t *testing.T) {
	states, err := LoadAllStates(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("got %d states, want 0", len(states))
	}
}

func TestLoadAllStatesMissingDir(t *testing.T) {
	states, err := LoadAllStates("/nonexistent/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if states != nil {
		t.Fatalf("expected nil, got %v", states)
	}
}

func TestLoadAllStatesSkipsInvalidStateFile(t *testing.T) {
	dir := t.TempDir()

	valid := &TunnelState{
		Config: TunnelConfig{
			Host:       "valid",
			LocalPort:  18339,
			RemotePort: 18340,
			Enabled:    true,
		},
		Status: StatusStopped,
	}
	if err := SaveState(dir, valid); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	invalidPath := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(invalidPath, []byte(`{"config":{"host":"","local_port":0,"remote_port":18340}}`), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	states, err := LoadAllStates(dir)
	if err != nil {
		t.Fatalf("LoadAllStates: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("got %d states, want 1", len(states))
	}
	if states[0].Config.Host != "valid" {
		t.Fatalf("host = %q, want %q", states[0].Config.Host, "valid")
	}
}

func TestLoadAllStatesSkipsCorruptCurrentStateFile(t *testing.T) {
	// A single corrupt current-format file must not block the daemon from
	// resuming the rest of the tunnels. LoadAllStates logs the bad file
	// and moves on.
	dir := t.TempDir()
	currentPath := StateFilePath(dir, "example", 18339)
	if err := os.WriteFile(currentPath, []byte(`{"config":{"host":"","local_port":0,"remote_port":18340}}`), 0600); err != nil {
		t.Fatalf("WriteFile current: %v", err)
	}
	other := &TunnelState{
		Config: TunnelConfig{Host: "other", LocalPort: 18340, RemotePort: 19010, Enabled: true},
		Status: StatusStopped,
	}
	if err := SaveState(dir, other); err != nil {
		t.Fatalf("SaveState other: %v", err)
	}

	states, err := LoadAllStates(dir)
	if err != nil {
		t.Fatalf("LoadAllStates: %v", err)
	}
	if len(states) != 1 || states[0].Config.Host != "other" {
		t.Fatalf("states = %+v, want single state for 'other'", states)
	}
}

func TestLoadAllStatesAllowsLegacyZeroLocalPort(t *testing.T) {
	dir := t.TempDir()
	legacyPath := StateFilePath(dir, "legacy", 0)
	if err := os.WriteFile(legacyPath, []byte(`{"config":{"host":"legacy","local_port":0,"remote_port":18340,"enabled":true},"status":"stopped"}`), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	states, err := LoadAllStates(dir)
	if err != nil {
		t.Fatalf("LoadAllStates: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("got %d states, want 1", len(states))
	}
	if states[0].Config.LocalPort != 0 {
		t.Fatalf("LocalPort = %d, want 0", states[0].Config.LocalPort)
	}
}

func TestLoadStateRejectsInvalidCurrentStateFile(t *testing.T) {
	dir := t.TempDir()
	path := StateFilePath(dir, "example", 18339)
	if err := os.WriteFile(path, []byte(`{"config":{"host":"","local_port":0,"remote_port":18340}}`), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadState(dir, "example", 18339)
	if err == nil || !strings.Contains(err.Error(), "parse tunnel state") {
		t.Fatalf("err = %v, want invalid-state parse error", err)
	}
}

func TestLoadAllStatesKeepsDifferentLocalPortsForSameHost(t *testing.T) {
	dir := t.TempDir()

	for _, localPort := range []int{18339, 18444} {
		s := &TunnelState{
			Config: TunnelConfig{
				Host:       "shared-host",
				LocalPort:  localPort,
				RemotePort: localPort + 1,
				Enabled:    true,
			},
			Status: StatusStopped,
		}
		if err := SaveState(dir, s); err != nil {
			t.Fatalf("SaveState(%d): %v", localPort, err)
		}
	}

	states, err := LoadAllStates(dir)
	if err != nil {
		t.Fatalf("LoadAllStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("got %d states, want 2", len(states))
	}
	if states[0].Config.LocalPort != 18339 || states[1].Config.LocalPort != 18444 {
		t.Fatalf("local ports = %d, %d; want 18339, 18444", states[0].Config.LocalPort, states[1].Config.LocalPort)
	}
}

func TestRemoveState(t *testing.T) {
	dir := t.TempDir()
	s := &TunnelState{
		Config: TunnelConfig{Host: "removeme", LocalPort: 18339, RemotePort: 18340},
		Status: StatusStopped,
	}
	SaveState(dir, s)

	if err := RemoveState(dir, "removeme", 18339); err != nil {
		t.Fatalf("RemoveState: %v", err)
	}
	if _, err := LoadState(dir, "removeme", 18339); err == nil {
		t.Fatal("expected error after removal")
	}
}
