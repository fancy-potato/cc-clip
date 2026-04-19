package peer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

func TestLoadLocalIdentityWithoutCreating(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if _, err := LoadLocalIdentity(); !errors.Is(err, ErrLocalIdentityNotFound) {
		t.Fatalf("LoadLocalIdentity missing files: got %v, want ErrLocalIdentityNotFound", err)
	}

	baseDir, err := BaseDir()
	if err != nil {
		t.Fatalf("BaseDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "local-peer-id"), []byte("peer-a\n"), 0600); err != nil {
		t.Fatalf("write id: %v", err)
	}
	got, err := LoadLocalIdentity()
	if err != nil {
		t.Fatalf("LoadLocalIdentity: %v", err)
	}
	if got.ID != "peer-a" {
		t.Fatalf("ID = %q, want peer-a", got.ID)
	}
	if got.Label != "peer" {
		t.Fatalf("Label = %q, want sanitized empty fallback \"peer\"", got.Label)
	}
}

// TestLoadLocalIdentityPropagatesIDReadErrors pins that a hard read error
// on the peer-id file (e.g. a directory at the expected path) is surfaced
// as a non-sentinel error, NOT misclassified as ErrLocalIdentityNotFound.
// That distinction matters for `cc-clip uninstall --peer` which uses the
// sentinel to fail closed; turning an I/O error into "not found" would
// orphan a real remote reservation.
func TestLoadLocalIdentityPropagatesIDReadErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	baseDir, err := BaseDir()
	if err != nil {
		t.Fatalf("BaseDir: %v", err)
	}

	if err := os.Mkdir(filepath.Join(baseDir, "local-peer-id"), 0700); err != nil {
		t.Fatalf("mkdir local-peer-id: %v", err)
	}
	if _, err := LoadLocalIdentity(); err == nil || errors.Is(err, ErrLocalIdentityNotFound) {
		t.Fatalf("LoadLocalIdentity id read error = %v, want non-sentinel read failure", err)
	}
}

// TestLoadLocalIdentitySwallowsLabelReadErrors pins the INTENTIONAL
// asymmetry with the id file: a label read error is not fatal because the
// self-targeted uninstall path only needs the id. A hard I/O failure on
// local-peer-label (e.g. a directory at the path) falls back to an empty
// label, which sanitizeLabel normalizes to "peer". Contrast with
// TestLoadLocalIdentityPropagatesIDReadErrors where id read errors DO
// propagate.
func TestLoadLocalIdentitySwallowsLabelReadErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	baseDir, err := BaseDir()
	if err != nil {
		t.Fatalf("BaseDir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(baseDir, "local-peer-id"), []byte("peer-a\n"), 0600); err != nil {
		t.Fatalf("write id: %v", err)
	}
	if err := os.Mkdir(filepath.Join(baseDir, "local-peer-label"), 0700); err != nil {
		t.Fatalf("mkdir local-peer-label: %v", err)
	}
	got, err := LoadLocalIdentity()
	if err != nil {
		t.Fatalf("LoadLocalIdentity label read error should fall back, got %v", err)
	}
	if got.ID != "peer-a" {
		t.Fatalf("ID = %q, want peer-a", got.ID)
	}
	if got.Label != "peer" {
		t.Fatalf("Label = %q, want sanitized fallback \"peer\"", got.Label)
	}
}

func TestValidateID(t *testing.T) {
	for _, id := range []string{"peer-a", "peer_a", "peer.a", "abc123"} {
		if err := ValidateID(id); err != nil {
			t.Fatalf("ValidateID(%q): unexpected error %v", id, err)
		}
	}
	for _, id := range []string{"", " peer", "../peer", "peer/slash", "peer space", "peer\nid"} {
		if err := ValidateID(id); err == nil {
			t.Fatalf("ValidateID(%q): expected error", id)
		}
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

// TestReservePortReallocatesWhenExistingPortBoundByUnrelatedProcess pins
// the P2-2 review fix: if the registry says an existing reservation is
// either unowned or unrecorded AND the port is now bound by a process we
// cannot verify as our own, the reservation must fall through to the
// free-port scan instead of silently handing back a port the daemon
// cannot forward to. The `ownedByUs` branch keeps its prior semantics
// (the port being bound by OUR active tunnel is expected and does not
// force reallocation).
func TestReservePortReallocatesWhenExistingPortBoundByUnrelatedProcess(t *testing.T) {
	dir := t.TempDir()
	first, err := ReservePort(dir, "peer-a", "macbook", 18339, 18341)
	if err != nil {
		t.Fatal(err)
	}

	// Manually clear the port-owner mapping so the next reserve hits the
	// `unowned` branch — simulates a crashed writer that committed
	// peers.json but not ports.json.
	ports, peers, err := loadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	delete(ports.Ports, strconv.Itoa(first.ReservedPort))
	if err := writeRegistry(dir, ports, peers); err != nil {
		t.Fatal(err)
	}

	// Now claim the first.ReservedPort is bound by an unrelated process;
	// reservation for peer-a should skip it and pick a different port.
	oldCheck := portAvailableCheck
	portAvailableCheck = func(port int) bool { return port != first.ReservedPort }
	defer func() { portAvailableCheck = oldCheck }()

	second, err := ReservePort(dir, "peer-a", "macbook", 18339, 18341)
	if err != nil {
		t.Fatal(err)
	}
	if second.ReservedPort == first.ReservedPort {
		t.Fatalf("expected reallocation when unowned port is bound; got same %d", second.ReservedPort)
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

// TestListAllReturnsRegisteredPeers pins the contract the uninstall path
// depends on: after a multi-peer account has N reservations, ListAll
// returns all N. If this regresses to "empty-on-populated-registry", the
// shared-asset cleanup in `cc-clip uninstall --host H --peer` would
// wrongly believe it's the last peer and delete `~/.local/bin/clipcc`
// out from under the other laptops.
func TestListAllReturnsRegisteredPeers(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReservePort(dir, "peer-a", "macbook", 18339, 18341); err != nil {
		t.Fatal(err)
	}
	if _, err := ReservePort(dir, "peer-b", "imac", 18339, 18341); err != nil {
		t.Fatal(err)
	}
	regs, err := ListAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 2 {
		t.Fatalf("ListAll returned %d regs, want 2", len(regs))
	}
	seen := map[string]bool{}
	for _, r := range regs {
		seen[r.PeerID] = true
	}
	if !seen["peer-a"] || !seen["peer-b"] {
		t.Fatalf("ListAll missing peer(s); got %v", regs)
	}
}

// TestListAllEmptyOnFreshRegistry pins the "no peers → empty slice"
// branch used by the last-peer-standing check: a missing registry file
// must not surface as an error (it's the normal "nothing registered yet"
// state) and must return a non-nil empty slice so JSON-encoding the
// result produces `[]` rather than `null`.
func TestListAllEmptyOnFreshRegistry(t *testing.T) {
	dir := t.TempDir()
	regs, err := ListAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 0 {
		t.Fatalf("ListAll on empty registry returned %d regs, want 0", len(regs))
	}
	if regs == nil {
		t.Fatalf("ListAll must return non-nil slice so JSON encodes as [] not null")
	}
}

// TestListAllAfterReleaseReportsSurvivors pins the exact semantics the
// uninstall path consumes: after peer A releases, a subsequent ListAll
// sees only peer B. This is what tells the caller "another laptop is
// still using the shared assets — don't nuke them".
func TestListAllAfterReleaseReportsSurvivors(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReservePort(dir, "peer-a", "macbook", 18339, 18341); err != nil {
		t.Fatal(err)
	}
	if _, err := ReservePort(dir, "peer-b", "imac", 18339, 18341); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleasePort(dir, "peer-a"); err != nil {
		t.Fatal(err)
	}
	regs, err := ListAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 1 || regs[0].PeerID != "peer-b" {
		t.Fatalf("ListAll after peer-a release: got %v, want [peer-b]", regs)
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

// TestLookupAndReleaseWrapErrPeerNotFound pins the typed sentinel the
// remote SSH caller classifies via exit code. Prior code relied on a
// strings.Contains("peer not found") match that could swallow unrelated
// errors; the errors.Is contract is what the uninstall idempotency
// path now depends on.
func TestLookupAndReleaseWrapErrPeerNotFound(t *testing.T) {
	dir := t.TempDir()
	_, lookupErr := Lookup(dir, "nobody")
	if !errors.Is(lookupErr, ErrPeerNotFound) {
		t.Fatalf("Lookup missing peer: want errors.Is ErrPeerNotFound, got %v", lookupErr)
	}
	_, releaseErr := ReleasePort(dir, "nobody")
	if !errors.Is(releaseErr, ErrPeerNotFound) {
		t.Fatalf("ReleasePort missing peer: want errors.Is ErrPeerNotFound, got %v", releaseErr)
	}
}

func TestReleasePortCleansDanglingPortMappingsWhenPeerMissing(t *testing.T) {
	dir := t.TempDir()
	registryDir := filepath.Join(dir, "registry")
	if err := os.MkdirAll(registryDir, 0700); err != nil {
		t.Fatal(err)
	}
	ports := PortsFile{
		Version:    registryVersion,
		RangeStart: 18339,
		RangeEnd:   18341,
		Ports: map[string]string{
			"18340": "peer-a",
		},
	}
	peers := PeersFile{
		Version: registryVersion,
		Peers:   map[string]Registration{},
	}
	if err := writeRegistry(dir, ports, peers); err != nil {
		t.Fatalf("writeRegistry: %v", err)
	}

	_, err := ReleasePort(dir, "peer-a")
	if !errors.Is(err, ErrPeerNotFound) {
		t.Fatalf("ReleasePort dangling peer: got %v, want ErrPeerNotFound", err)
	}

	afterPorts, _, err := loadRegistry(dir)
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	if afterPorts.Ports["18340"] != "" {
		t.Fatalf("dangling port mapping survived release: %#v", afterPorts.Ports)
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

// TestReservePortFailsOnEmptyRegistryFile pins the fail-closed contract for a
// zero-byte registry file (crash between create and first write, or a stray
// `touch`). Without this, a truncated ports.json would silently become an
// empty map and let the allocator re-hand out ports that another live peer
// still thinks it owns.
func TestReservePortFailsOnEmptyRegistryFile(t *testing.T) {
	dir := t.TempDir()
	registryDir := filepath.Join(dir, "registry")
	if err := os.MkdirAll(registryDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registryDir, "peers.json"), []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := ReservePort(dir, "peer-a", "macbook", 18339, 18341)
	if err == nil || !strings.Contains(err.Error(), "failed to load peers registry") {
		t.Fatalf("expected empty-file registry error, got %v", err)
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

func TestStaleRegistryLockHonorsHardCeilingEvenWhenPIDIsAlive(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "registry", "lock")
	if err := os.MkdirAll(lockPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockPath, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	oldHardCeiling := registryLockHardCeiling
	registryLockHardCeiling = time.Minute
	defer func() { registryLockHardCeiling = oldHardCeiling }()

	staleAt := time.Now().Add(-2 * registryLockHardCeiling)
	if err := os.Chtimes(lockPath, staleAt, staleAt); err != nil {
		t.Fatal(err)
	}

	stale, err := staleRegistryLock(lockPath)
	if err != nil {
		t.Fatalf("staleRegistryLock: %v", err)
	}
	if !stale {
		t.Fatal("expected hard ceiling to reap an old lock even when the recorded PID is alive")
	}
}

func TestStaleRegistryLockKeepsRecentLivePID(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "registry", "lock")
	if err := os.MkdirAll(lockPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockPath, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	oldHardCeiling := registryLockHardCeiling
	registryLockHardCeiling = time.Minute
	defer func() { registryLockHardCeiling = oldHardCeiling }()

	recent := time.Now().Add(-time.Second)
	if err := os.Chtimes(lockPath, recent, recent); err != nil {
		t.Fatal(err)
	}

	stale, err := staleRegistryLock(lockPath)
	if err != nil {
		t.Fatalf("staleRegistryLock: %v", err)
	}
	if stale {
		t.Fatal("expected recent live PID to keep the lock valid")
	}
}

// TestWriteJSONAtomicLeavesNoTempCruftAndIsSynced pins the durability contract
// for writeJSONAtomic: (1) after a successful write the target file is
// non-empty, valid JSON, and matches the input; (2) no `.tmp.*` siblings are
// left behind (a leaked temp would mean the rename failed silently); (3)
// repeated overwrites stay consistent and don't accumulate cruft. This is the
// companion to TestReservePortFailsOnEmptyRegistryFile — that test pins "an
// empty file is a hard error"; this one pins "writeJSONAtomic will never
// produce an empty file on the happy path". Together they close the torn-write
// gap surfaced in review P2.
func TestWriteJSONAtomicLeavesNoTempCruftAndIsSynced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry", "peers.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		pf := PeersFile{
			Version: registryVersion,
			Peers: map[string]Registration{
				"peer-a": {PeerID: "peer-a", ReservedPort: 18339 + i, UpdatedAt: RFC3339Now()},
			},
		}
		if err := writeJSONAtomic(path, pf); err != nil {
			t.Fatalf("writeJSONAtomic iter %d: %v", i, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat iter %d: %v", i, err)
		}
		if info.Size() == 0 {
			t.Fatalf("iter %d: wrote zero-byte file", i)
		}
		var got PeersFile
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read iter %d: %v", i, err)
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("invalid JSON iter %d: %v (contents: %q)", i, err, string(data))
		}
		if got.Peers["peer-a"].ReservedPort != 18339+i {
			t.Fatalf("iter %d: round-trip mismatch: %+v", i, got.Peers["peer-a"])
		}
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Fatalf("temp file leaked after successful writes: %s", e.Name())
		}
	}
}
