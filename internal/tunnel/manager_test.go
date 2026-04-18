package tunnel

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitForCondition polls fn until true or fails after timeout — a deflake
// for tests that watch async state transitions.
func waitForCondition(t *testing.T, timeout time.Duration, msg string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// requireSSH skips when no `ssh` binary is on PATH; manager.Up shells out
// and would otherwise produce flake-prone Start failures.
func requireSSH(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skipf("ssh binary not available: %v", err)
	}
}

// isActiveTunnelStatus distinguishes live/in-flight from terminal states,
// so != Stopped assertions cannot pass just because ssh never started.
func isActiveTunnelStatus(s Status) bool {
	return s == StatusConnecting || s == StatusConnected
}

func TestManagerUpAndList(t *testing.T) {
	requireSSH(t)
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Shutdown()

	cfg := TunnelConfig{
		Host:       "testhost",
		LocalPort:  18339,
		RemotePort: 18340,
		Enabled:    true,
	}

	if err := mgr.Up(cfg); err != nil {
		t.Fatalf("Up: %v", err)
	}

	key := stateKey("testhost", 18339)
	entry, ok := mgr.entries[key]
	if !ok {
		t.Fatal("expected live tunnel entry after Up")
	}
	if !entry.state.Config.Enabled {
		t.Fatal("expected live entry to stay enabled")
	}
	// Assert the live entry actually reaches an active status rather than
	// just "anything but Stopped". A bare != StatusStopped assertion would
	// pass even when ssh fails Start() and the state flips straight to
	// Disconnected, hiding a broken manager.
	waitForCondition(t, 2*time.Second, "live entry active (connecting/connected)", func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		e, ok := mgr.entries[key]
		return ok && e.state != nil && isActiveTunnelStatus(e.state.Status)
	})

	states, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("got %d tunnels, want 1", len(states))
	}
	if states[0].Config.Host != "testhost" {
		t.Errorf("host = %q, want testhost", states[0].Config.Host)
	}
	if !states[0].Config.Enabled {
		t.Error("expected enabled=true")
	}
	if !isActiveTunnelStatus(states[0].Status) {
		t.Fatalf("status = %q, want Connecting or Connected", states[0].Status)
	}
}

func TestManagerUpReturnsErrorWhenInitialStateSaveFails(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "states.json")
	if err := os.WriteFile(stateDir, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	mgr := NewManager(stateDir)
	defer mgr.Shutdown()

	err := mgr.Up(TunnelConfig{
		Host:       "testhost",
		LocalPort:  18339,
		RemotePort: 18340,
		Enabled:    true,
	})
	if err == nil || !strings.Contains(err.Error(), "persist tunnel state") {
		t.Fatalf("err = %v, want initial state persistence error", err)
	}
	if len(mgr.entries) != 0 {
		t.Fatalf("got %d live entries, want 0", len(mgr.entries))
	}
}

func TestManagerListKeepsDistinctLocalPortsForSameHost(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Shutdown()

	for _, cfg := range []TunnelConfig{
		{Host: "shared", LocalPort: 18339, RemotePort: 19001, Enabled: true},
		{Host: "shared", LocalPort: 18444, RemotePort: 19002, Enabled: true},
	} {
		if err := mgr.Up(cfg); err != nil {
			t.Fatalf("Up(%d): %v", cfg.LocalPort, err)
		}
	}

	waitForCondition(t, 2*time.Second, "both tunnel entries to register", func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		return len(mgr.entries) == 2
	})

	states, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("got %d tunnels, want 2", len(states))
	}
	if states[0].Config.LocalPort != 18339 || states[1].Config.LocalPort != 18444 {
		t.Fatalf("ports = [%d %d], want [18339 18444]", states[0].Config.LocalPort, states[1].Config.LocalPort)
	}
}

func TestManagerListIncludesForeignLocalPorts(t *testing.T) {
	dir := t.TempDir()
	for _, state := range []*TunnelState{
		{
			Config: TunnelConfig{Host: "owned", LocalPort: 18339, RemotePort: 19001, Enabled: true},
			Status: StatusConnected,
		},
		{
			Config: TunnelConfig{Host: "foreign", LocalPort: 18444, RemotePort: 19002, Enabled: true},
			Status: StatusConnected,
		},
	} {
		if err := SaveState(dir, state); err != nil {
			t.Fatalf("SaveState(%s): %v", state.Config.Host, err)
		}
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	mgr.SetLocalPort(18339)

	states, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("got %d tunnels, want 2", len(states))
	}
	if states[0].Config.Host != "foreign" || states[1].Config.Host != "owned" {
		t.Fatalf("hosts = [%q %q], want [foreign owned]", states[0].Config.Host, states[1].Config.Host)
	}
}

func TestManagerDown(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Shutdown()

	cfg := TunnelConfig{
		Host:       "downhost",
		LocalPort:  18339,
		RemotePort: 18341,
		Enabled:    true,
	}
	if err := mgr.Up(cfg); err != nil {
		t.Fatalf("Up: %v", err)
	}
	waitForCondition(t, 2*time.Second, "downhost entry to register", func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		_, ok := mgr.entries[stateKey("downhost", 18339)]
		return ok
	})

	if err := mgr.Down("downhost", 18339); err != nil {
		t.Fatalf("Down: %v", err)
	}

	// State file should still exist but disabled.
	s, err := LoadState(dir, "downhost", 18339)
	if err != nil {
		t.Fatalf("LoadState after Down: %v", err)
	}
	if s.Config.Enabled {
		t.Error("expected enabled=false after Down")
	}
	if s.Status != StatusStopped {
		t.Errorf("status = %q, want stopped", s.Status)
	}
}

func TestManagerDownNonexistent(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Shutdown()

	err := mgr.Down("nohost", 18339)
	if err == nil {
		t.Fatal("expected error for nonexistent tunnel")
	}
	if !errors.Is(err, ErrTunnelNotFound) {
		t.Fatalf("err = %v, want ErrTunnelNotFound", err)
	}
}

func TestManagerDownWithoutLiveEntryStopsRecordedProcess(t *testing.T) {
	dir := t.TempDir()
	s := &TunnelState{
		Config: TunnelConfig{
			Host:       "offlinehost",
			LocalPort:  18339,
			RemotePort: 18341,
			Enabled:    true,
		},
		Status: StatusDisconnected,
		PID:    7654,
	}
	if err := SaveState(dir, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	mgr.SetLocalPort(18339)
	cleanupCalls := 0
	mgr.cleanupStaleProcess = func(pid int, cfg TunnelConfig) error {
		cleanupCalls++
		if pid != 7654 {
			t.Fatalf("pid = %d, want 7654", pid)
		}
		if cfg.Host != "offlinehost" {
			t.Fatalf("host = %q, want offlinehost", cfg.Host)
		}
		return nil
	}

	if err := mgr.Down("offlinehost", 18339); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanupCalls = %d, want 1", cleanupCalls)
	}

	got, err := LoadState(dir, "offlinehost", 18339)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Status != StatusStopped {
		t.Fatalf("status = %q, want %q", got.Status, StatusStopped)
	}
	if got.PID != 0 {
		t.Fatalf("PID = %d, want 0", got.PID)
	}
	if got.Config.Enabled {
		t.Fatal("expected enabled=false")
	}
}

func TestManagerLoadAndStartAll(t *testing.T) {
	dir := t.TempDir()

	// Pre-write an enabled state file that should be adopted as already running.
	s := &TunnelState{
		Config: TunnelConfig{
			Host:              "savedhost",
			LocalPort:         18339,
			RemotePort:        18342,
			Enabled:           true,
			SSHConfigResolved: true,
		},
		Status: StatusConnected,
		PID:    4321,
	}
	if err := SaveState(dir, s); err != nil {
		t.Fatalf("SaveState savedhost: %v", err)
	}

	// Also write a disabled one.
	s2 := &TunnelState{
		Config: TunnelConfig{
			Host:       "disabledhost",
			LocalPort:  18339,
			RemotePort: 18343,
			Enabled:    false,
		},
		Status: StatusStopped,
	}
	if err := SaveState(dir, s2); err != nil {
		t.Fatalf("SaveState disabledhost: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	mgr.processMatchesPID = func(pid int, cfg TunnelConfig) (bool, error) {
		if cfg.Host != "savedhost" {
			return false, nil
		}
		return pid == 4321, nil
	}
	mgr.LoadAndStartAll(18339)

	// Adopted tunnels now pass through StatusConnecting first — adoptedLoop
	// flips to Connected only after the first successful PID inspect. Poll
	// for the confirmed state before asserting so the test does not race
	// the adoption goroutine's first poll.
	waitForCondition(t, 2*time.Second, "savedhost adoption to confirm connected", func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		e, ok := mgr.entries[stateKey("savedhost", 18339)]
		if !ok || e == nil || e.state == nil {
			return false
		}
		return e.state.Status == StatusConnected
	})

	states, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Both should appear in the list.
	if len(states) != 2 {
		t.Fatalf("got %d tunnels, want 2", len(states))
	}

	// savedhost should be adopted as a live connected tunnel.
	// disabledhost should remain stopped.
	for _, st := range states {
		if st.Config.Host == "savedhost" {
			if st.Status != StatusConnected {
				t.Fatalf("savedhost status = %q, want %q", st.Status, StatusConnected)
			}
			if st.PID != 4321 {
				t.Fatalf("savedhost PID = %d, want 4321", st.PID)
			}
		}
		if st.Config.Host == "disabledhost" && st.Status != StatusStopped {
			t.Errorf("disabledhost status = %q, want stopped", st.Status)
		}
	}
	entry, ok := mgr.entries[stateKey("savedhost", 18339)]
	if !ok {
		t.Fatal("savedhost was not started")
	}
	if entry.state.Status != StatusConnected {
		t.Fatalf("live savedhost status = %q, want %q", entry.state.Status, StatusConnected)
	}
	if entry.state.PID != 4321 {
		t.Fatalf("live savedhost PID = %d, want 4321", entry.state.PID)
	}
	if _, ok := mgr.entries[stateKey("disabledhost", 18339)]; ok {
		t.Fatal("disabledhost should not be started")
	}

	// adoptedLoop promotes to StatusConnected in-memory first, then
	// saveEntryState persists asynchronously. Poll the on-disk state so
	// the assertion does not race the persist.
	waitForCondition(t, 2*time.Second, "savedhost persisted as connected", func() bool {
		got, err := LoadState(dir, "savedhost", 18339)
		if err != nil {
			return false
		}
		return got.Status == StatusConnected
	})

	got, err := LoadState(dir, "savedhost", 18339)
	if err != nil {
		t.Fatalf("LoadState savedhost: %v", err)
	}
	if got.Status != StatusConnected {
		t.Fatalf("persisted savedhost status = %q, want %q", got.Status, StatusConnected)
	}
	if got.PID != 4321 {
		t.Fatalf("persisted savedhost PID = %d, want 4321", got.PID)
	}
}

func TestManagerLoadAndStartAllSkipsDifferentLocalPort(t *testing.T) {
	dir := t.TempDir()

	current := &TunnelState{
		Config: TunnelConfig{
			Host:              "current-port-host",
			LocalPort:         18339,
			RemotePort:        18342,
			Enabled:           true,
			SSHConfigResolved: true,
		},
		Status: StatusStopped,
	}
	if err := SaveState(dir, current); err != nil {
		t.Fatalf("SaveState current: %v", err)
	}

	other := &TunnelState{
		Config: TunnelConfig{
			Host:              "other-port-host",
			LocalPort:         18444,
			RemotePort:        18343,
			Enabled:           true,
			SSHConfigResolved: true,
		},
		Status: StatusStopped,
	}
	if err := SaveState(dir, other); err != nil {
		t.Fatalf("SaveState other: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	mgr.LoadAndStartAll(18339)

	waitForCondition(t, 2*time.Second, "current-port-host to start", func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		_, ok := mgr.entries[stateKey("current-port-host", 18339)]
		return ok
	})

	mgr.mu.Lock()
	_, hasOther := mgr.entries[stateKey("other-port-host", 18444)]
	mgr.mu.Unlock()
	if hasOther {
		t.Fatal("other-port-host should be skipped for a different daemon port")
	}
}

func TestManagerLoadAndStartAllDoesNotRestartWhenRecordedProcessInspectFails(t *testing.T) {
	dir := t.TempDir()

	s := &TunnelState{
		Config: TunnelConfig{
			Host:              "stalehost",
			LocalPort:         18339,
			RemotePort:        18342,
			Enabled:           true,
			SSHConfigResolved: true,
		},
		Status: StatusConnected,
		PID:    4321,
	}
	if err := SaveState(dir, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	mgr.processMatchesPID = func(pid int, cfg TunnelConfig) (bool, error) {
		if pid != 4321 {
			t.Fatalf("pid = %d, want 4321", pid)
		}
		if cfg.Host != "stalehost" {
			t.Fatalf("host = %q, want stalehost", cfg.Host)
		}
		return false, errors.New("inspect failed")
	}
	cleanupCalls := 0
	mgr.cleanupStaleProcess = func(pid int, cfg TunnelConfig) error {
		cleanupCalls++
		return nil
	}

	mgr.LoadAndStartAll(18339)
	if cleanupCalls != 0 {
		t.Fatalf("cleanupCalls = %d, want 0 during startup adoption", cleanupCalls)
	}

	if _, ok := mgr.entries[stateKey("stalehost", 18339)]; ok {
		t.Fatal("stalehost should not be restarted when pid inspection fails")
	}

	got, err := LoadState(dir, "stalehost", 18339)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Status != StatusDisconnected {
		t.Fatalf("status = %q, want %q", got.Status, StatusDisconnected)
	}
	if got.PID != 0 {
		t.Fatalf("PID = %d, want stale pid to be cleared", got.PID)
	}
}

func TestManagerLoadAndStartAllCleansDisabledRecordedProcess(t *testing.T) {
	dir := t.TempDir()

	s := &TunnelState{
		Config: TunnelConfig{
			Host:       "disabled-stale",
			LocalPort:  18339,
			RemotePort: 18342,
			Enabled:    false,
		},
		Status: StatusStopped,
		PID:    4321,
	}
	if err := SaveState(dir, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	cleanupCalls := 0
	mgr.cleanupStaleProcess = func(pid int, cfg TunnelConfig) error {
		cleanupCalls++
		if pid != 4321 {
			t.Fatalf("pid = %d, want 4321", pid)
		}
		if cfg.Host != "disabled-stale" {
			t.Fatalf("host = %q, want disabled-stale", cfg.Host)
		}
		return nil
	}

	mgr.LoadAndStartAll(18339)

	if cleanupCalls != 1 {
		t.Fatalf("cleanupCalls = %d, want 1", cleanupCalls)
	}
	if _, ok := mgr.entries[stateKey("disabled-stale", 18339)]; ok {
		t.Fatal("disabled-stale should not be started")
	}

	got, err := LoadState(dir, "disabled-stale", 18339)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.PID != 0 {
		t.Fatalf("PID = %d, want 0", got.PID)
	}
	if got.Status != StatusStopped {
		t.Fatalf("status = %q, want %q", got.Status, StatusStopped)
	}
}

func TestManagerLoadAndStartAllFindsRunningProcessWhenPIDMissing(t *testing.T) {
	dir := t.TempDir()

	s := &TunnelState{
		Config: TunnelConfig{
			Host:              "recover-no-pid",
			LocalPort:         18339,
			RemotePort:        18342,
			Enabled:           true,
			SSHConfigResolved: true,
		},
		Status: StatusConnecting,
	}
	if err := SaveState(dir, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	findCalls := 0
	cleanupCalls := 0
	mgr.findRunningProcess = func(cfg TunnelConfig) (int, bool, error) {
		findCalls++
		if cfg.Host != "recover-no-pid" {
			t.Fatalf("host = %q, want recover-no-pid", cfg.Host)
		}
		return 4321, true, nil
	}
	mgr.cleanupStaleProcess = func(pid int, cfg TunnelConfig) error {
		cleanupCalls++
		if pid != 4321 {
			t.Fatalf("pid = %d, want 4321", pid)
		}
		if cfg.Host != "recover-no-pid" {
			t.Fatalf("host = %q, want recover-no-pid", cfg.Host)
		}
		return nil
	}
	// Stub the adoption-confirm probe so adoptedLoop can promote the state
	// to StatusConnected without us having to spawn a real process with
	// the recorded PID.
	mgr.processMatchesPID = func(pid int, cfg TunnelConfig) (bool, error) {
		if pid != 4321 {
			t.Fatalf("pid = %d, want 4321", pid)
		}
		if cfg.Host != "recover-no-pid" {
			t.Fatalf("host = %q, want recover-no-pid", cfg.Host)
		}
		return true, nil
	}

	mgr.LoadAndStartAll(18339)

	if findCalls != 1 {
		t.Fatalf("findCalls = %d, want 1", findCalls)
	}
	if cleanupCalls != 0 {
		t.Fatalf("cleanupCalls = %d, want 0", cleanupCalls)
	}
	if _, ok := mgr.entries[stateKey("recover-no-pid", 18339)]; !ok {
		t.Fatal("recover-no-pid should be adopted")
	}

	// Wait for adoptedLoop's first confirm to flip the persisted state
	// from StatusConnecting (the placeholder LoadAndStartAll writes before
	// the first poll) to StatusConnected.
	waitForCondition(t, 2*time.Second, "recover-no-pid confirm connected", func() bool {
		got, err := LoadState(dir, "recover-no-pid", 18339)
		if err != nil {
			return false
		}
		return got.Status == StatusConnected
	})

	got, err := LoadState(dir, "recover-no-pid", 18339)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Status != StatusConnected {
		t.Fatalf("status = %q, want %q", got.Status, StatusConnected)
	}
	if got.PID != 4321 {
		t.Fatalf("PID = %d, want 4321", got.PID)
	}
}

func TestManagerLoadAndStartAllAdoptsRecordedProcessWithoutCleanup(t *testing.T) {
	dir := t.TempDir()

	s := &TunnelState{
		Config: TunnelConfig{
			Host:              "recover-existing",
			LocalPort:         18339,
			RemotePort:        18342,
			Enabled:           true,
			SSHConfigResolved: true,
		},
		Status: StatusConnected,
		PID:    4321,
	}
	if err := SaveState(dir, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	mgr.processMatchesPID = func(pid int, cfg TunnelConfig) (bool, error) {
		if pid != 4321 {
			t.Fatalf("pid = %d, want 4321", pid)
		}
		if cfg.Host != "recover-existing" {
			t.Fatalf("host = %q, want recover-existing", cfg.Host)
		}
		return true, nil
	}
	cleanupCalls := 0
	mgr.cleanupStaleProcess = func(pid int, cfg TunnelConfig) error {
		cleanupCalls++
		return nil
	}

	mgr.LoadAndStartAll(18339)

	if cleanupCalls != 0 {
		t.Fatalf("cleanupCalls = %d, want 0", cleanupCalls)
	}
	if _, ok := mgr.entries[stateKey("recover-existing", 18339)]; !ok {
		t.Fatal("recover-existing should be adopted")
	}
}

func TestManagerLoadAndStartAllKeepsAdoptionWhenImmediateReinspectFails(t *testing.T) {
	dir := t.TempDir()

	s := &TunnelState{
		Config: TunnelConfig{
			Host:              "recover-transient",
			LocalPort:         18339,
			RemotePort:        18342,
			Enabled:           true,
			SSHConfigResolved: true,
		},
		Status: StatusConnected,
		PID:    4321,
	}
	if err := SaveState(dir, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()

	var calls atomic.Int32
	mgr.processMatchesPID = func(pid int, cfg TunnelConfig) (bool, error) {
		if pid != 4321 {
			t.Fatalf("pid = %d, want 4321", pid)
		}
		if cfg.Host != "recover-transient" {
			t.Fatalf("host = %q, want recover-transient", cfg.Host)
		}
		switch calls.Add(1) {
		case 1:
			return true, nil
		case 2:
			return false, errors.New("transient inspect failed")
		default:
			return true, nil
		}
	}

	cleanupCalls := 0
	mgr.cleanupStaleProcess = func(pid int, cfg TunnelConfig) error {
		cleanupCalls++
		return nil
	}

	mgr.LoadAndStartAll(18339)

	waitForCondition(t, 2*time.Second, "recover-transient adoption to confirm connected", func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		e, ok := mgr.entries[stateKey("recover-transient", 18339)]
		return ok && e != nil && e.state != nil && e.state.Status == StatusConnected
	})

	if cleanupCalls != 0 {
		t.Fatalf("cleanupCalls = %d, want 0", cleanupCalls)
	}

	waitForCondition(t, 2*time.Second, "recover-transient persisted as connected", func() bool {
		got, err := LoadState(dir, "recover-transient", 18339)
		return err == nil && got.Status == StatusConnected && got.PID == 4321
	})

	got, err := LoadState(dir, "recover-transient", 18339)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Status != StatusConnected {
		t.Fatalf("status = %q, want %q", got.Status, StatusConnected)
	}
	if got.PID != 4321 {
		t.Fatalf("PID = %d, want 4321", got.PID)
	}
	if calls.Load() < 3 {
		t.Fatalf("processMatchesPID called %d times, want >= 3", calls.Load())
	}
}

func TestManagerLoadAndStartAllSkipsAlreadyLiveTunnel(t *testing.T) {
	dir := t.TempDir()
	s := &TunnelState{
		Config: TunnelConfig{
			Host:              "livehost",
			LocalPort:         18339,
			RemotePort:        18342,
			Enabled:           true,
			SSHConfigResolved: true,
		},
		Status: StatusStopped,
		PID:    4321,
	}
	if err := SaveState(dir, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	done := make(chan struct{})
	close(done)
	live := &entry{
		cancel: func() {},
		done:   done,
		state: &TunnelState{
			Config: TunnelConfig{
				Host:       "livehost",
				LocalPort:  18339,
				RemotePort: 18342,
				Enabled:    true,
			},
			Status: StatusConnected,
			PID:    9876,
		},
	}
	mgr.entries[stateKey("livehost", 18339)] = live
	cleanupCalls := 0
	mgr.cleanupStaleProcess = func(pid int, cfg TunnelConfig) error {
		cleanupCalls++
		return nil
	}

	mgr.LoadAndStartAll(18339)

	if cleanupCalls != 0 {
		t.Fatalf("cleanupCalls = %d, want 0", cleanupCalls)
	}
	if mgr.entries[stateKey("livehost", 18339)] != live {
		t.Fatal("expected existing live entry to be preserved")
	}
}

func TestWatchTunnelReadyKeepsDrainingAfterReady(t *testing.T) {
	cfg := TunnelConfig{
		Host:       "example",
		LocalPort:  18339,
		RemotePort: 19001,
	}

	pr, pw := io.Pipe()
	readyCh := watchTunnelReady(pr, cfg)

	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(pw, "debug1\nremote forward success for: listen 19001, connect 127.0.0.1:18339\ndebug2\ndebug3\n")
		if err == nil {
			err = pw.Close()
		}
		writeDone <- err
	}()

	select {
	case ready := <-readyCh:
		if !ready {
			t.Fatal("ready = false, want true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for readiness")
	}

	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("writer failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stderr reader stopped draining after readiness")
	}
}

func TestManagerRemove(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Shutdown()

	cfg := TunnelConfig{
		Host:       "rmhost",
		LocalPort:  18339,
		RemotePort: 18344,
		Enabled:    true,
	}
	if err := mgr.Up(cfg); err != nil {
		t.Fatalf("Up: %v", err)
	}
	waitForCondition(t, 2*time.Second, "rmhost entry to register", func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		_, ok := mgr.entries[stateKey("rmhost", 18339)]
		return ok
	})

	if err := mgr.Remove("rmhost", 18339); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// State file should be gone.
	if _, err := LoadState(dir, "rmhost", 18339); err == nil {
		t.Fatal("expected error after Remove")
	}
}

func TestManagerRemoveKeepsStateWhenDownFails(t *testing.T) {
	dir := t.TempDir()
	state := &TunnelState{
		Config: TunnelConfig{
			Host:       "stalehost",
			LocalPort:  18339,
			RemotePort: 18344,
			Enabled:    true,
		},
		Status: StatusConnected,
		PID:    4321,
	}
	if err := SaveState(dir, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	mgr.SetLocalPort(18339)
	mgr.cleanupStaleProcess = func(pid int, cfg TunnelConfig) error {
		if pid != 4321 {
			t.Fatalf("pid = %d, want 4321", pid)
		}
		if cfg.Host != "stalehost" {
			t.Fatalf("host = %q, want stalehost", cfg.Host)
		}
		return errors.New("still running")
	}

	err := mgr.Remove("stalehost", 18339)
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("err = %v, want cleanup failure", err)
	}

	got, loadErr := LoadState(dir, "stalehost", 18339)
	if loadErr != nil {
		t.Fatalf("LoadState: %v", loadErr)
	}
	if got.PID != 4321 {
		t.Fatalf("PID = %d, want 4321", got.PID)
	}
	if !got.Config.Enabled {
		t.Fatal("expected state to remain enabled after failed remove")
	}
}

func TestManagerDownWithoutLiveEntryDoesNotTouchOtherLocalPort(t *testing.T) {
	dir := t.TempDir()
	s := &TunnelState{
		Config: TunnelConfig{
			Host:       "sharedhost",
			LocalPort:  18444,
			RemotePort: 18341,
			Enabled:    true,
		},
		Status: StatusDisconnected,
	}
	if err := SaveState(dir, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	mgr.SetLocalPort(18339)

	err := mgr.Down("sharedhost", 18339)
	if !errors.Is(err, ErrTunnelNotFound) {
		t.Fatalf("err = %v, want ErrTunnelNotFound", err)
	}

	got, err := LoadState(dir, "sharedhost", 18444)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !got.Config.Enabled {
		t.Fatal("expected other-port state to remain enabled")
	}
}

func TestManagerUpRejectsForeignLocalPort(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Shutdown()
	mgr.SetLocalPort(18339)

	err := mgr.Up(TunnelConfig{
		Host:       "foreign-up",
		LocalPort:  18444,
		RemotePort: 19001,
		Enabled:    true,
	})
	if !errors.Is(err, ErrTunnelLocalPortMismatch) {
		t.Fatalf("err = %v, want ErrTunnelLocalPortMismatch", err)
	}
	if len(mgr.entries) != 0 {
		t.Fatalf("got %d live entries, want 0", len(mgr.entries))
	}
}

func TestManagerDownRejectsForeignLocalPort(t *testing.T) {
	dir := t.TempDir()
	s := &TunnelState{
		Config: TunnelConfig{
			Host:       "foreign-down",
			LocalPort:  18444,
			RemotePort: 18341,
			Enabled:    true,
		},
		Status: StatusDisconnected,
	}
	if err := SaveState(dir, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	mgr.SetLocalPort(18339)

	err := mgr.Down("foreign-down", 18444)
	if !errors.Is(err, ErrTunnelLocalPortMismatch) {
		t.Fatalf("err = %v, want ErrTunnelLocalPortMismatch", err)
	}

	got, err := LoadState(dir, "foreign-down", 18444)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !got.Config.Enabled {
		t.Fatal("expected foreign state to remain enabled")
	}
}

func TestManagerDownWithoutLiveEntryFindsRunningProcessWhenPIDMissing(t *testing.T) {
	dir := t.TempDir()
	s := &TunnelState{
		Config: TunnelConfig{
			Host:       "recover-down",
			LocalPort:  18339,
			RemotePort: 18341,
			Enabled:    true,
		},
		Status: StatusConnecting,
	}
	if err := SaveState(dir, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	mgr.SetLocalPort(18339)
	findCalls := 0
	cleanupCalls := 0
	mgr.findRunningProcess = func(cfg TunnelConfig) (int, bool, error) {
		findCalls++
		if cfg.Host != "recover-down" {
			t.Fatalf("host = %q, want recover-down", cfg.Host)
		}
		return 7654, true, nil
	}
	mgr.cleanupStaleProcess = func(pid int, cfg TunnelConfig) error {
		cleanupCalls++
		if pid != 7654 {
			t.Fatalf("pid = %d, want 7654", pid)
		}
		if cfg.Host != "recover-down" {
			t.Fatalf("host = %q, want recover-down", cfg.Host)
		}
		return nil
	}

	if err := mgr.Down("recover-down", 18339); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if findCalls != 1 {
		t.Fatalf("findCalls = %d, want 1", findCalls)
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanupCalls = %d, want 1", cleanupCalls)
	}

	got, err := LoadState(dir, "recover-down", 18339)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Status != StatusStopped {
		t.Fatalf("status = %q, want %q", got.Status, StatusStopped)
	}
	if got.PID != 0 {
		t.Fatalf("PID = %d, want 0", got.PID)
	}
	if got.Config.Enabled {
		t.Fatal("expected enabled=false")
	}
}

func TestManagerListReturnsSnapshotCopies(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Shutdown()

	now := time.Now()
	done := make(chan struct{})
	close(done)
	mgr.entries[stateKey("snapshothost", 18339)] = &entry{
		cancel: func() {},
		done:   done,
		state: &TunnelState{
			Config:    TunnelConfig{Host: "snapshothost", LocalPort: 18339, RemotePort: 18340, Enabled: true},
			Status:    StatusConnected,
			PID:       1234,
			StartedAt: &now,
		},
	}

	states, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("got %d tunnels, want 1", len(states))
	}

	states[0].PID = 9999
	states[0].Status = StatusStopped

	underlying := mgr.entries[stateKey("snapshothost", 18339)].state
	if underlying.PID != 1234 {
		t.Fatalf("underlying PID = %d, want %d", underlying.PID, 1234)
	}
	if underlying.Status != StatusConnected {
		t.Fatalf("underlying status = %q, want %q", underlying.Status, StatusConnected)
	}
}

func TestManagerListReturnsStateLoadError(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "states.json")
	if err := os.WriteFile(stateDir, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	mgr := NewManager(stateDir)
	defer mgr.Shutdown()

	_, err := mgr.List()
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("err = %v, want state load error", err)
	}
}

func TestManagerUpdateStateIncrementsReconnectCountAndPersists(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Shutdown()

	e := &entry{
		cancel: func() {},
		done:   make(chan struct{}),
		state: &TunnelState{
			Config: TunnelConfig{
				Host:       "reconnect-host",
				LocalPort:  18339,
				RemotePort: 18340,
				Enabled:    true,
			},
			Status: StatusConnected,
		},
	}
	close(e.done)
	mgr.entries[stateKey("reconnect-host", 18339)] = e

	mgr.updateState(e, StatusDisconnected, 0, "ssh exited", true)

	if e.state.ReconnectCount != 1 {
		t.Fatalf("ReconnectCount = %d, want 1", e.state.ReconnectCount)
	}

	s, err := LoadState(dir, "reconnect-host", 18339)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if s.ReconnectCount != 1 {
		t.Fatalf("persisted ReconnectCount = %d, want 1", s.ReconnectCount)
	}
	if s.Status != StatusDisconnected {
		t.Fatalf("status = %q, want %q", s.Status, StatusDisconnected)
	}
}

func TestManagerUpdateStateTracksPersistenceError(t *testing.T) {
	badStateDir := filepath.Join(t.TempDir(), "states.json")
	if err := os.WriteFile(badStateDir, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	mgr := NewManager(badStateDir)
	defer mgr.Shutdown()

	e := &entry{
		state: &TunnelState{
			Config: TunnelConfig{
				Host:       "persist-host",
				LocalPort:  18339,
				RemotePort: 18340,
				Enabled:    true,
			},
			Status: StatusConnected,
		},
	}

	mgr.updateState(e, StatusDisconnected, 0, "ssh exited", true)
	if e.state.PersistenceError == "" {
		t.Fatal("expected persistence error to be recorded in live state")
	}

	goodStateDir := t.TempDir()
	mgr.stateDir = goodStateDir
	mgr.updateState(e, StatusDisconnected, 0, "ssh exited", false)

	if e.state.PersistenceError != "" {
		t.Fatalf("PersistenceError = %q, want empty after successful save", e.state.PersistenceError)
	}

	s, err := LoadState(goodStateDir, "persist-host", 18339)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if s.PersistenceError != "" {
		t.Fatalf("persisted PersistenceError = %q, want empty", s.PersistenceError)
	}
}

func TestManagerPersistStateTimeoutDoesNotOverwriteNewerState(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Shutdown()

	mgr.saveStateTimeout = 20 * time.Millisecond

	var calls atomic.Int32
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	mgr.persistState = func(stateDir string, st *TunnelState) error {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		return saveStateForDisk(stateDir, st)
	}

	first := &TunnelState{
		Config: TunnelConfig{
			Host:       "persist-host",
			LocalPort:  18339,
			RemotePort: 19001,
			Enabled:    true,
		},
		Status: StatusConnecting,
	}

	firstErrCh := make(chan error, 1)
	go func() {
		firstErrCh <- mgr.persistStateForDisk(first)
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for first persist to start")
	}

	if err := <-firstErrCh; err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("first persist err = %v, want timeout", err)
	}

	second := cloneState(first)
	second.Status = StatusDisconnected
	second.LastError = "newer state"

	if err := mgr.persistStateForDisk(second); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("second persist err = %v, want timeout", err)
	}

	close(releaseFirst)

	waitForCondition(t, time.Second, "newest state persisted", func() bool {
		got, err := LoadState(dir, "persist-host", 18339)
		if err != nil {
			return false
		}
		return got.Status == StatusDisconnected && got.LastError == "newer state"
	})

	if calls.Load() != 2 {
		t.Fatalf("persist call count = %d, want 2", calls.Load())
	}
}

func TestManagerUpPreservesExistingTunnelWhenPersistenceFails(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "states.json")
	if err := os.WriteFile(stateDir, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	mgr := NewManager(stateDir)
	defer mgr.Shutdown()
	cancelCalls := 0
	done := make(chan struct{})
	close(done)
	old := &entry{
		cancel: func() { cancelCalls++ },
		done:   done,
		state: &TunnelState{
			Config: TunnelConfig{
				Host:       "testhost",
				LocalPort:  18339,
				RemotePort: 18340,
				Enabled:    true,
			},
			Status: StatusConnected,
			PID:    4321,
		},
	}
	mgr.entries[stateKey("testhost", 18339)] = old

	err := mgr.Up(TunnelConfig{
		Host:       "testhost",
		LocalPort:  18339,
		RemotePort: 19001,
		Enabled:    true,
	})
	if err == nil || !strings.Contains(err.Error(), "persist tunnel state") {
		t.Fatalf("err = %v, want initial state persistence error", err)
	}
	if cancelCalls != 0 {
		t.Fatalf("cancelCalls = %d, want 0", cancelCalls)
	}
	if mgr.entries[stateKey("testhost", 18339)] != old {
		t.Fatal("expected existing tunnel entry to remain active")
	}
}

func TestManagerUpAfterShutdownReturnsErrManagerShuttingDown(t *testing.T) {
	mgr := NewManager(t.TempDir())
	mgr.Shutdown()

	err := mgr.Up(TunnelConfig{
		Host:       "testhost",
		LocalPort:  18339,
		RemotePort: 18340,
		Enabled:    true,
	})
	if !errors.Is(err, ErrManagerShuttingDown) {
		t.Fatalf("err = %v, want ErrManagerShuttingDown", err)
	}
}

func TestManagerStopEntryWaitsForLatePublishedCommand(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Shutdown()

	done := make(chan struct{})
	stopCalled := make(chan struct{})
	mgr.stopProcess = func(cmd *exec.Cmd) error {
		if cmd == nil {
			t.Fatal("expected stopProcess to receive a command")
		}
		close(stopCalled)
		close(done)
		return nil
	}

	// Signal from the cancel func so we can wait deterministically for
	// stopEntry to have begun its shutdown sequence (and thus be inside
	// waitForEntryCommand) instead of sleeping an arbitrary 25ms that
	// would flake on slow CI.
	cancelCalled := make(chan struct{})
	e := &entry{
		cancel: func() { close(cancelCalled) },
		done:   done,
	}

	returned := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.stopEntry(e)
		close(returned)
	}()

	// Wait for stopEntry to reach the point after cancel and entryTargetSnapshot.
	select {
	case <-cancelCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("stopEntry never called cancel")
	}

	mgr.mu.Lock()
	e.cmd = &exec.Cmd{}
	mgr.mu.Unlock()

	select {
	case <-stopCalled:
	case <-time.After(time.Second):
		t.Fatal("stopEntry did not wait for the late-published command")
	}

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("stopEntry did not return after stopping the command")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("stopEntry: %v", err)
	}
}

func TestManagerStopEntryWaitsForLatePublishedCommandAfterContextCancel(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Shutdown()

	done := make(chan struct{})
	stopCalled := make(chan struct{})
	mgr.stopProcess = func(cmd *exec.Cmd) error {
		if cmd == nil {
			t.Fatal("expected stopProcess to receive a command")
		}
		close(stopCalled)
		close(done)
		return nil
	}

	ctx, cancelCtx := context.WithCancel(context.Background())
	cancelCalled := make(chan struct{})
	e := &entry{
		ctx: ctx,
		cancel: func() {
			cancelCtx()
			close(cancelCalled)
		},
		done: done,
	}

	returned := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.stopEntry(e)
		close(returned)
	}()

	select {
	case <-cancelCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("stopEntry never called cancel")
	}

	mgr.mu.Lock()
	e.cmd = &exec.Cmd{}
	mgr.mu.Unlock()

	select {
	case <-stopCalled:
	case <-time.After(time.Second):
		t.Fatal("stopEntry did not wait for the late-published command after context cancellation")
	}

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("stopEntry did not return after stopping the command")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("stopEntry: %v", err)
	}
}

func TestManagerStopEntryIgnoresProcessAlreadyFinishedOnKill(t *testing.T) {
	mgr := NewManager(t.TempDir())
	// Shrink stopEntry's internal timeouts so the test exercises the
	// graceful-timeout → force-kill → done-closes-during-kill-window
	// path. Margins are deliberately wider than the previous 20/60/200ms
	// triple — that combo could lose the timing race on a loaded CI
	// runner (scheduler jitter > grace period), causing the test to
	// silently skip the force-kill branch it claims to validate.
	mgr.stopGracePeriod = 50 * time.Millisecond
	mgr.stopKillPeriod = 250 * time.Millisecond
	defer mgr.Shutdown()

	cmd := exec.Command(os.Args[0], "-test.run=TestManagerStopEntryHelperProcess")
	cmd.Env = append(os.Environ(), "CC_CLIP_MANAGER_STOPENTRY_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start helper: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait helper: %v", err)
	}

	done := make(chan struct{})
	cancelWait := make(chan struct{})
	t.Cleanup(func() { close(cancelWait) })
	go func() {
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-cancelWait:
		}
		close(done)
	}()

	e := &entry{
		state: &TunnelState{
			Config: TunnelConfig{
				Host:       "already-finished",
				LocalPort:  18339,
				RemotePort: 18340,
				Enabled:    true,
			},
			Status: StatusConnected,
		},
		cmd:    cmd,
		cancel: func() {},
		done:   done,
	}
	mgr.stopProcess = func(*exec.Cmd) error { return nil }

	if err := mgr.stopEntry(e); err != nil {
		t.Fatalf("stopEntry: %v", err)
	}
}

func TestManagerStopEntryHelperProcess(t *testing.T) {
	if os.Getenv("CC_CLIP_MANAGER_STOPENTRY_HELPER") != "1" {
		return
	}
	os.Exit(0)
}

func TestManagerDownKeepsLiveEntryWhenStopFails(t *testing.T) {
	dir := t.TempDir()
	state := &TunnelState{
		Config: TunnelConfig{
			Host:       "live-stop-fail",
			LocalPort:  18339,
			RemotePort: 18340,
			Enabled:    true,
		},
		Status: StatusConnected,
	}
	if err := SaveState(dir, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	mgr := NewManager(dir)
	defer mgr.Shutdown()
	mgr.SetLocalPort(18339)
	done := make(chan struct{})
	e := &entry{
		state:  cloneState(state),
		cmd:    &exec.Cmd{},
		cancel: func() {},
		done:   done,
	}
	mgr.entries[stateKey("live-stop-fail", 18339)] = e
	mgr.stopProcess = func(cmd *exec.Cmd) error {
		return errors.New("signal failed")
	}

	err := mgr.Down("live-stop-fail", 18339)
	if err == nil || !strings.Contains(err.Error(), "signal failed") {
		t.Fatalf("err = %v, want stop failure", err)
	}
	if _, ok := mgr.entries[stateKey("live-stop-fail", 18339)]; !ok {
		t.Fatal("expected live entry to remain after failed stop")
	}

	got, err := LoadState(dir, "live-stop-fail", 18339)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !got.Config.Enabled {
		t.Fatal("expected state to remain enabled after failed stop")
	}
}

func TestManagerAdoptedLoopRetriesInspectErrors(t *testing.T) {
	mgr := NewManager(t.TempDir())
	defer mgr.Shutdown()

	retried := make(chan struct{})
	var calls atomic.Int32
	mgr.processMatchesPID = func(pid int, cfg TunnelConfig) (bool, error) {
		c := calls.Add(1)
		if pid != 4321 {
			t.Fatalf("pid = %d, want 4321", pid)
		}
		if cfg.Host != "adopted-host" {
			t.Fatalf("host = %q, want adopted-host", cfg.Host)
		}
		if c == 1 {
			return false, errors.New("inspect failed")
		}
		select {
		case <-retried:
		default:
			close(retried)
		}
		return true, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &entry{
		state: &TunnelState{
			Config: TunnelConfig{
				Host:       "adopted-host",
				LocalPort:  18339,
				RemotePort: 18340,
				Enabled:    true,
			},
			Status: StatusConnected,
			PID:    4321,
		},
		cancel: func() {},
		done:   make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		mgr.adoptedLoop(ctx, e, e.state.Config, e.state.PID)
		close(done)
	}()

	select {
	case <-retried:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("adoptedLoop did not retry after inspect failure")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("adoptedLoop did not exit after cancellation")
	}

	if got := calls.Load(); got < 2 {
		t.Fatalf("calls = %d, want at least 2", got)
	}
	if e.state.Status != StatusConnected {
		t.Fatalf("status = %q, want %q", e.state.Status, StatusConnected)
	}
	if e.state.ReconnectCount != 0 {
		t.Fatalf("ReconnectCount = %d, want 0", e.state.ReconnectCount)
	}
}

func TestWaitAndCleanupRunsCleanupAfterWaitReturns(t *testing.T) {
	waitStarted := make(chan struct{})
	releaseWait := make(chan struct{})
	cleanupCalled := make(chan struct{})
	errCh := make(chan error, 1)

	go func() {
		errCh <- waitAndCleanup(func() error {
			close(waitStarted)
			<-releaseWait
			return nil
		}, func() {
			close(cleanupCalled)
		})
	}()

	<-waitStarted
	select {
	case <-cleanupCalled:
		t.Fatal("cleanup ran before wait returned")
	default:
	}

	close(releaseWait)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("waitAndCleanup returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waitAndCleanup did not return")
	}

	select {
	case <-cleanupCalled:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not run after wait returned")
	}
}

func TestTunnelForwardSuccessLineRejectsPortSubstring(t *testing.T) {
	cfg := TunnelConfig{Host: "example", LocalPort: 18339, RemotePort: 19001}
	substringLine := "debug1: remote forward success for: listen 190019999, connect 127.0.0.1:18339"
	if tunnelForwardSuccessLine(substringLine, cfg) {
		t.Fatalf("tunnelForwardSuccessLine should reject port substring: %q", substringLine)
	}

	exactLine := "debug1: remote forward success for: listen 19001, connect 127.0.0.1:18339"
	if !tunnelForwardSuccessLine(exactLine, cfg) {
		t.Fatalf("tunnelForwardSuccessLine should accept exact port: %q", exactLine)
	}
}

func TestWatchTunnelReadyIgnoresInteractiveFallbackBeforeForwardSuccess(t *testing.T) {
	cfg := TunnelConfig{Host: "example", LocalPort: 18339, RemotePort: 19001}

	pr, pw := io.Pipe()
	readyCh := watchTunnelReady(pr, cfg)

	// Write the interactive-session fallback on its own, without a
	// prior forward-success line. watchTunnelReady must NOT fire on
	// this — doing so would flip state to "connected" even though our
	// specific reverse forward never bound.
	go func() {
		io.WriteString(pw, "debug1: Entering interactive session.\n")
		io.WriteString(pw, "debug1: Connecting to something unrelated.\n")
		pw.Close()
	}()

	select {
	case ready := <-readyCh:
		if ready {
			t.Fatal("watchTunnelReady fired on interactive-session fallback without forward success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchTunnelReady did not close readyCh after stderr EOF")
	}
}

func TestWatchTunnelReadyHonoursFallbackAfterForwardSuccess(t *testing.T) {
	cfg := TunnelConfig{Host: "example", LocalPort: 18339, RemotePort: 19001}

	pr, pw := io.Pipe()
	readyCh := watchTunnelReady(pr, cfg)

	go func() {
		io.WriteString(pw, "debug1: remote forward success for: listen 19001, connect 127.0.0.1:18339\n")
		io.WriteString(pw, "debug1: Entering interactive session.\n")
		pw.Close()
	}()

	select {
	case ready := <-readyCh:
		if !ready {
			t.Fatal("watchTunnelReady did not fire on forward-success line")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchTunnelReady did not fire within timeout")
	}
}

func TestWatchTunnelReadyRejectsPortSubstring(t *testing.T) {
	cfg := TunnelConfig{Host: "example", LocalPort: 18339, RemotePort: 19001}

	pr, pw := io.Pipe()
	readyCh := watchTunnelReady(pr, cfg)

	// A different tunnel's forward success line whose port happens
	// to embed this tunnel's RemotePort as a substring must NOT fire
	// the readiness signal.
	go func() {
		io.WriteString(pw, "debug1: remote forward success for: listen 190019999, connect 127.0.0.1:18339\n")
		pw.Close()
	}()

	select {
	case ready := <-readyCh:
		if ready {
			t.Fatal("watchTunnelReady matched port substring")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchTunnelReady did not close readyCh")
	}
}

func TestSSHTunnelArgsDoNotInheritForwardings(t *testing.T) {
	cfg := TunnelConfig{
		Host:       "example",
		LocalPort:  18339,
		RemotePort: 19001,
	}

	resolved := strings.Join([]string{
		"hostname example.internal",
		"localforward 8080 [127.0.0.1]:8080",
		"remoteforward 19002 [127.0.0.1]:18339",
	}, "\n")
	cfg.SSHOptions = resolvedTunnelOptionsFromSSHConfig(resolved)
	cfg.SSHConfigResolved = true
	args := buildSSHTunnelArgs(cfg, "/tmp/empty-ssh-config")
	joined := strings.ToLower(strings.Join(args, " "))

	for _, unexpected := range []string{"localforward", "remoteforward 19002"} {
		if strings.Contains(joined, unexpected) {
			t.Fatalf("ssh args unexpectedly preserved %q: %v", unexpected, args)
		}
	}
	if !strings.Contains(joined, "-r 19001:127.0.0.1:18339") {
		t.Fatalf("ssh args missing reverse forward: %v", args)
	}
}

// TestManagerShutdownClosesDoneChannels verifies that Shutdown actually
// drains the supervising goroutine for each entry (regression guard for
// a prior bug where Shutdown returned while reconnect goroutines were
// still running and could race cleanup).
func TestManagerShutdownClosesDoneChannels(t *testing.T) {
	requireSSH(t)
	dir := t.TempDir()
	mgr := NewManager(dir)
	mgr.stopGracePeriod = 50 * time.Millisecond
	mgr.stopKillPeriod = 100 * time.Millisecond
	mgr.shutdownWaitPerEntry = 500 * time.Millisecond

	cfg := TunnelConfig{Host: "shutdown-drain", LocalPort: 19101, RemotePort: 19102, Enabled: true}
	if err := mgr.Up(cfg); err != nil {
		t.Fatalf("Up: %v", err)
	}

	mgr.mu.Lock()
	var done chan struct{}
	for _, e := range mgr.entries {
		done = e.done
	}
	mgr.mu.Unlock()
	if done == nil {
		t.Fatal("expected a live entry to exist before Shutdown")
	}

	shutDone := make(chan struct{})
	go func() {
		mgr.Shutdown()
		close(shutDone)
	}()
	select {
	case <-shutDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return within 2s")
	}

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Shutdown returned but per-entry done channel was not closed")
	}
}

// TestReconnectLoopExitsOnContextCancel drives reconnectLoop against an
// invalid cached ssh option, then cancels the manager and asserts the
// goroutine exits within the bounded window. Covers the backoff+cancel
// path without re-introducing background ssh -G resolution.
func TestReconnectLoopExitsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	mgr.shutdownWaitPerEntry = 500 * time.Millisecond

	cfg := TunnelConfig{
		Host:              "reconnect-cancel",
		LocalPort:         19110,
		RemotePort:        19111,
		Enabled:           true,
		SSHOptions:        []string{"BadOption=value"},
		SSHConfigResolved: true,
	}
	if err := mgr.startTunnel(cfg); err != nil {
		t.Fatalf("startTunnel: %v", err)
	}

	// Wait until at least one backoff iteration completed so we know the
	// loop is alive and in the sleepCtx path.
	waitForCondition(t, 2*time.Second, "reconnect attempt recorded", func() bool {
		states, _ := mgr.List()
		for _, s := range states {
			if s.Config.Host == cfg.Host && (s.LastError != "" || s.ReconnectCount > 0) {
				return true
			}
		}
		return false
	})

	mgr.mu.Lock()
	var done chan struct{}
	for _, e := range mgr.entries {
		done = e.done
	}
	mgr.mu.Unlock()
	if done == nil {
		t.Fatal("expected a live entry before Shutdown")
	}

	shutdownDone := make(chan struct{})
	go func() {
		mgr.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return within 2s")
	}
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("reconnect goroutine did not close its done channel after Shutdown")
	}
}

// TestPersistDrainPreservesUpOrdering pins the invariant that a late
// cancel-path "stopped" persist from an outgoing entry cannot overwrite
// the replacement entry's "connecting" placeholder on disk. Without the
// drain, persistMu is not FIFO, so a detached persist goroutine still
// queued from the old reconnect loop could win the mutex race against
// Up()'s line-383 restore persist and leave disk state stale.
//
// Test shape: inject a latched persistState hook that blocks the FIRST
// write (simulating a slow disk / blocked persistMu queue), trigger an
// entry-tracked persist for "stopped", then use waitPersistDrain to
// flush it before a separate "connecting" persist fires. Assert the
// recorded call order and final on-disk status.
func TestPersistDrainPreservesUpOrdering(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Shutdown()

	// Tighten the timeout so saveEntryState returns quickly once the
	// detached goroutine is confirmed running (it will time out while
	// the hook still blocks, and that is expected — the wg Done does
	// not fire until the hook returns).
	mgr.saveStateTimeout = 50 * time.Millisecond

	type call struct {
		status    Status
		lastError string
	}
	var (
		callsMu     sync.Mutex
		calls       []call
		firstDone   atomic.Bool
		firstBegan  = make(chan struct{})
		releaseHook = make(chan struct{})
	)
	mgr.persistState = func(_ string, st *TunnelState) error {
		if firstDone.CompareAndSwap(false, true) {
			close(firstBegan)
			<-releaseHook
		}
		callsMu.Lock()
		calls = append(calls, call{st.Status, st.LastError})
		callsMu.Unlock()
		return saveStateForDisk(dir, st)
	}

	old := &entry{
		state: &TunnelState{
			Config: TunnelConfig{
				Host:       "drain-host",
				LocalPort:  18339,
				RemotePort: 19001,
				Enabled:    true,
			},
			Status:    StatusStopped,
			LastError: "old-stopped",
		},
		done: make(chan struct{}),
	}
	close(old.done)

	saveErrCh := make(chan error, 1)
	go func() {
		saveErrCh <- mgr.saveEntryState(old)
	}()

	// Confirm the detached persist goroutine is actually running under
	// the hook; without this the test could race the goroutine's
	// persistWg.Add(1).
	select {
	case <-firstBegan:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for first persist to begin inside hook")
	}

	// saveEntryState itself should time out (hook blocks past 50ms) but
	// the detached writer continues to hold persistWg.
	if err := <-saveErrCh; err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("saveEntryState err = %v, want timeout", err)
	}

	// Drain with a short budget should NOT succeed — the hook is still
	// blocking the underlying write and persistWg still has +1.
	if drained := waitPersistDrain(&old.persistWg, 30*time.Millisecond); drained {
		t.Fatal("waitPersistDrain returned true while old persist is still blocked")
	}

	close(releaseHook)

	// After release, drain should complete quickly.
	if drained := waitPersistDrain(&old.persistWg, time.Second); !drained {
		t.Fatal("waitPersistDrain did not return true after releasing hook")
	}

	// Now the replacement "connecting" persist. Because the old wg is
	// empty, this call is guaranteed to land after the stopped write.
	connecting := &TunnelState{
		Config:    old.state.Config,
		Status:    StatusConnecting,
		LastError: "new-connecting",
	}
	if err := mgr.persistStateForDisk(connecting); err != nil {
		t.Fatalf("persistStateForDisk connecting: %v", err)
	}

	callsMu.Lock()
	got := append([]call(nil), calls...)
	callsMu.Unlock()

	if len(got) != 2 {
		t.Fatalf("persist calls = %d, want 2: %+v", len(got), got)
	}
	if got[0].status != StatusStopped || got[0].lastError != "old-stopped" {
		t.Fatalf("first call = %+v, want {Stopped, old-stopped}", got[0])
	}
	if got[1].status != StatusConnecting || got[1].lastError != "new-connecting" {
		t.Fatalf("second call = %+v, want {Connecting, new-connecting}", got[1])
	}

	final, err := LoadState(dir, "drain-host", 18339)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if final.Status != StatusConnecting {
		t.Fatalf("final disk status = %v, want Connecting (drain did not preserve ordering)", final.Status)
	}
	if final.LastError != "new-connecting" {
		t.Fatalf("final disk lastError = %q, want %q", final.LastError, "new-connecting")
	}
}
