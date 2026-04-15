package peer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	portAvailableCheck = func(int) bool { return true }
}

func TestLoadOrCreateLocalIdentity(t *testing.T) {
	home := t.TempDir()
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", home)
	defer func() { _ = os.Setenv("HOME", oldHome) }()

	got, err := LoadOrCreateLocalIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ID) != 32 {
		t.Fatalf("expected 32-char peer id, got %q", got.ID)
	}
	if got.Label == "" {
		t.Fatal("expected non-empty label")
	}

	got2, err := LoadOrCreateLocalIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if got != got2 {
		t.Fatalf("expected stable identity, got %#v then %#v", got, got2)
	}
}

func TestAliasForHost(t *testing.T) {
	got := AliasForHost("myserver", "MacBook Pro")
	if got != "myserver-cc-clip-macbook-pro" {
		t.Fatalf("unexpected alias %q", got)
	}
}

func TestAliasForHostKeepsUserScopedDestinationsDistinct(t *testing.T) {
	got := AliasForHost("alice@example.com", "MacBook Pro")
	if got != "alice-example-com-cc-clip-macbook-pro" {
		t.Fatalf("unexpected alias %q", got)
	}
}

func TestReservePortReusesExistingPeer(t *testing.T) {
	dir := t.TempDir()
	first, err := ReservePort(dir, "peer-a", "macbook", 18339, 18341)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReservePort(dir, "peer-a", "macbook", 18339, 18341)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReservedPort != second.ReservedPort {
		t.Fatalf("expected same port, got %d then %d", first.ReservedPort, second.ReservedPort)
	}
}

func TestReservePortPreservesExistingPeerPortWhenAlreadyBound(t *testing.T) {
	dir := t.TempDir()
	first, err := ReservePort(dir, "peer-a", "macbook", 18339, 18341)
	if err != nil {
		t.Fatal(err)
	}

	oldCheck := portAvailableCheck
	portAvailableCheck = func(port int) bool { return port != first.ReservedPort }
	defer func() { portAvailableCheck = oldCheck }()

	second, err := ReservePort(dir, "peer-a", "macbook", 18339, 18341)
	if err != nil {
		t.Fatal(err)
	}
	if second.ReservedPort != first.ReservedPort {
		t.Fatalf("expected reconnect to keep reserved port %d, got %d", first.ReservedPort, second.ReservedPort)
	}
}

func TestReservePortAllocatesDistinctPorts(t *testing.T) {
	dir := t.TempDir()
	a, err := ReservePort(dir, "peer-a", "macbook", 18339, 18341)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ReservePort(dir, "peer-b", "imac", 18339, 18341)
	if err != nil {
		t.Fatal(err)
	}
	if a.ReservedPort == b.ReservedPort {
		t.Fatalf("expected distinct ports, both got %d", a.ReservedPort)
	}
}

func TestReleasePortFreesPort(t *testing.T) {
	dir := t.TempDir()
	a, err := ReservePort(dir, "peer-a", "macbook", 18339, 18340)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReleasePort(dir, "peer-a"); err != nil {
		t.Fatal(err)
	}
	b, err := ReservePort(dir, "peer-b", "imac", 18339, 18340)
	if err != nil {
		t.Fatal(err)
	}
	if a.ReservedPort != b.ReservedPort {
		t.Fatalf("expected released port reuse, got %d then %d", a.ReservedPort, b.ReservedPort)
	}
}

func TestReservePortFailsOnCorruptRegistry(t *testing.T) {
	dir := t.TempDir()
	registryDir := filepath.Join(dir, "registry")
	if err := os.MkdirAll(registryDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registryDir, "ports.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := ReservePort(dir, "peer-a", "macbook", 18339, 18341)
	if err == nil || !strings.Contains(err.Error(), "failed to load ports registry") {
		t.Fatalf("expected corrupt registry error, got %v", err)
	}
}

func TestReservePortRangeExhausted(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReservePort(dir, "peer-a", "macbook", 18339, 18339); err != nil {
		t.Fatal(err)
	}
	if _, err := ReservePort(dir, "peer-b", "imac", 18339, 18339); err == nil {
		t.Fatal("expected range exhaustion error")
	}
}

func TestReservePortReassignClearsOldPortMapping(t *testing.T) {
	dir := t.TempDir()

	first, err := ReservePort(dir, "peer-a", "macbook", 18339, 18340)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReservedPort != 18339 {
		t.Fatalf("expected first port 18339, got %d", first.ReservedPort)
	}

	second, err := ReservePort(dir, "peer-a", "macbook", 18340, 18340)
	if err != nil {
		t.Fatal(err)
	}
	if second.ReservedPort != 18340 {
		t.Fatalf("expected reassigned port 18340, got %d", second.ReservedPort)
	}

	ports, peers, err := loadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ports.Ports["18339"] != "" {
		t.Fatalf("expected old port mapping to be cleared, got %q", ports.Ports["18339"])
	}
	if ports.Ports["18340"] != "peer-a" {
		t.Fatalf("expected new port mapping for peer-a, got %q", ports.Ports["18340"])
	}
	if peers.Peers["peer-a"].ReservedPort != 18340 {
		t.Fatalf("expected peer registry to track new port, got %d", peers.Peers["peer-a"].ReservedPort)
	}
}

func TestReservePortSkipsOccupiedSocket(t *testing.T) {
	dir := t.TempDir()
	oldCheck := portAvailableCheck
	portAvailableCheck = func(port int) bool { return port != 18339 }
	defer func() { portAvailableCheck = oldCheck }()

	reg, err := ReservePort(dir, "peer-a", "macbook", 18339, 18340)
	if err != nil {
		t.Fatal(err)
	}
	if reg.ReservedPort != 18340 {
		t.Fatalf("expected allocator to skip occupied port 18339, got %d", reg.ReservedPort)
	}
}

func TestReservePortRemovesStaleRegistryLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "registry", "lock")
	if err := os.MkdirAll(lockPath, 0700); err != nil {
		t.Fatal(err)
	}
	staleAt := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(lockPath, staleAt, staleAt); err != nil {
		t.Fatal(err)
	}

	oldRetryInterval := registryLockRetryInterval
	oldMaxAttempts := registryLockMaxAttempts
	oldStaleAfter := registryLockStaleAfter
	registryLockRetryInterval = time.Millisecond
	registryLockMaxAttempts = 2
	registryLockStaleAfter = time.Minute
	defer func() {
		registryLockRetryInterval = oldRetryInterval
		registryLockMaxAttempts = oldMaxAttempts
		registryLockStaleAfter = oldStaleAfter
	}()

	reg, err := ReservePort(dir, "peer-a", "macbook", 18339, 18339)
	if err != nil {
		t.Fatalf("expected stale lock recovery, got %v", err)
	}
	if reg.ReservedPort != 18339 {
		t.Fatalf("expected port reservation after stale lock cleanup, got %d", reg.ReservedPort)
	}
}
