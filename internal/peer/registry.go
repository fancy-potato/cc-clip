package peer

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const registryVersion = 1

// ErrPeerNotFound is returned by ReleasePort and Lookup when the requested
// peer ID has no entry in the registry. Idempotent cleanup paths (e.g.
// re-running `cc-clip uninstall --peer` after a successful release) use
// errors.Is to treat it as success without brittle stderr string matching.
var ErrPeerNotFound = errors.New("peer not found")

var (
	portAvailableCheck        = portAvailable
	registryLockRetryInterval = 100 * time.Millisecond
	registryLockMaxAttempts   = 50
	registryLockStaleAfter    = 30 * time.Second
	// registryLockHardCeiling caps how long a lock can be held even when the
	// recorded PID is still "alive". Protects against kernel PID rollover
	// (the original holder crashed, the PID was recycled to an unrelated
	// live process) which would otherwise pin the lock indefinitely.
	registryLockHardCeiling = 10 * time.Minute
)

type PortsFile struct {
	Version    int               `json:"version"`
	RangeStart int               `json:"range_start"`
	RangeEnd   int               `json:"range_end"`
	Ports      map[string]string `json:"ports"`
}

type PeersFile struct {
	Version int                     `json:"version"`
	Peers   map[string]Registration `json:"peers"`
}

func ReservePort(baseDir, peerID, label string, rangeStart, rangeEnd int) (Registration, error) {
	if err := ValidateID(peerID); err != nil {
		return Registration{}, err
	}
	unlock, err := lockRegistry(baseDir)
	if err != nil {
		return Registration{}, err
	}
	defer unlock()

	ports, peers, err := loadRegistry(baseDir)
	if err != nil {
		return Registration{}, err
	}

	now := RFC3339Now()
	if existing, ok := peers.Peers[peerID]; ok {
		portKey := strconv.Itoa(existing.ReservedPort)
		if existing.ReservedPort >= rangeStart && existing.ReservedPort <= rangeEnd {
			owner, portRecorded := ports.Ports[portKey]
			ownedByUs := owner == peerID
			unowned := !portRecorded || owner == ""
			if ownedByUs || unowned {
				// Verify the port is actually free to claim when the registry
				// says it's either ours or unowned. The common case is an
				// active SSH RemoteForward for THIS peer occupying its own
				// reserved port between reconnects — that's still "available"
				// from our perspective, so a bind failure in the ownedByUs
				// branch is accepted. But in the `unowned` branch, a bind
				// failure means an unrelated non-cc-clip process has grabbed
				// the port since the registry last recorded an owner; reusing
				// it would produce a reservation the daemon cannot actually
				// forward to. Fall through to the free-port scan in that
				// case so the user gets a working reservation.
				if unowned && !portAvailableCheck(existing.ReservedPort) {
					// Drop the stale registry entry for this port so the
					// scanner below doesn't skip it on a future peer.
					delete(ports.Ports, portKey)
				} else {
					existing.Label = label
					existing.UpdatedAt = now
					existing.LastConnect = now
					existing.StateDir = PeerStateDir(baseDir, peerID)
					peers.Peers[peerID] = existing
					ports.Ports[portKey] = peerID
					if err := writeRegistry(baseDir, ports, peers); err != nil {
						return Registration{}, err
					}
					if err := WritePeerState(existing.StateDir, existing); err != nil {
						return Registration{}, err
					}
					return existing, nil
				}
			}
		}
	}

	if existing, ok := peers.Peers[peerID]; ok {
		oldPortKey := strconv.Itoa(existing.ReservedPort)
		if ports.Ports[oldPortKey] == peerID {
			delete(ports.Ports, oldPortKey)
		}
	}

	for port := rangeStart; port <= rangeEnd; port++ {
		portKey := strconv.Itoa(port)
		if ports.Ports[portKey] != "" {
			continue
		}
		if !portAvailableCheck(port) {
			continue
		}
		reg := Registration{
			PeerID:       peerID,
			Label:        label,
			ReservedPort: port,
			StateDir:     PeerStateDir(baseDir, peerID),
			CreatedAt:    now,
			UpdatedAt:    now,
			LastConnect:  now,
		}
		if prev, ok := peers.Peers[peerID]; ok && prev.CreatedAt != "" {
			reg.CreatedAt = prev.CreatedAt
		}
		peers.Peers[peerID] = reg
		ports.Ports[portKey] = peerID
		if err := writeRegistry(baseDir, ports, peers); err != nil {
			return Registration{}, err
		}
		if err := WritePeerState(reg.StateDir, reg); err != nil {
			return Registration{}, err
		}
		return reg, nil
	}

	return Registration{}, fmt.Errorf("no free cc-clip ports available in range %d-%d", rangeStart, rangeEnd)
}

func ReleasePort(baseDir, peerID string) (Registration, error) {
	if err := ValidateID(peerID); err != nil {
		return Registration{}, err
	}
	unlock, err := lockRegistry(baseDir)
	if err != nil {
		return Registration{}, err
	}
	defer unlock()

	ports, peers, err := loadRegistry(baseDir)
	if err != nil {
		return Registration{}, err
	}

	reg, ok := peers.Peers[peerID]
	if !ok {
		cleaned := false
		for portKey, owner := range ports.Ports {
			if owner != peerID {
				continue
			}
			delete(ports.Ports, portKey)
			cleaned = true
		}
		if cleaned {
			if err := writeRegistry(baseDir, ports, peers); err != nil {
				return Registration{}, err
			}
		}
		return Registration{}, fmt.Errorf("peer %s: %w", peerID, ErrPeerNotFound)
	}

	delete(peers.Peers, peerID)
	delete(ports.Ports, strconv.Itoa(reg.ReservedPort))
	if err := writeRegistry(baseDir, ports, peers); err != nil {
		return Registration{}, err
	}
	return reg, nil
}

func Lookup(baseDir, peerID string) (Registration, error) {
	if err := ValidateID(peerID); err != nil {
		return Registration{}, err
	}
	_, peers, err := loadRegistry(baseDir)
	if err != nil {
		return Registration{}, err
	}
	reg, ok := peers.Peers[peerID]
	if !ok {
		return Registration{}, fmt.Errorf("peer %s: %w", peerID, ErrPeerNotFound)
	}
	return reg, nil
}

// ListAll returns every registration currently in the registry. The uninstall
// path uses this to detect whether other laptops (sharing the same remote
// Unix account) still depend on the shared `~/.local/bin/clipcc`,
// `cc-clip-hook`, Codex config, and PATH marker before deleting them.
// Order is not guaranteed — callers that need stable ordering must sort.
func ListAll(baseDir string) ([]Registration, error) {
	_, peers, err := loadRegistry(baseDir)
	if err != nil {
		return nil, err
	}
	out := make([]Registration, 0, len(peers.Peers))
	for _, reg := range peers.Peers {
		out = append(out, reg)
	}
	return out, nil
}

func loadRegistry(baseDir string) (PortsFile, PeersFile, error) {
	registryDir := filepath.Join(baseDir, "registry")
	if err := os.MkdirAll(registryDir, 0700); err != nil {
		return PortsFile{}, PeersFile{}, err
	}

	ports := PortsFile{
		Version:    registryVersion,
		RangeStart: DefaultRangeStart,
		RangeEnd:   DefaultRangeEnd,
		Ports:      map[string]string{},
	}
	peers := PeersFile{
		Version: registryVersion,
		Peers:   map[string]Registration{},
	}

	if err := loadJSON(filepath.Join(registryDir, "ports.json"), &ports); err != nil {
		return PortsFile{}, PeersFile{}, fmt.Errorf("failed to load ports registry: %w", err)
	}
	if err := loadJSON(filepath.Join(registryDir, "peers.json"), &peers); err != nil {
		return PortsFile{}, PeersFile{}, fmt.Errorf("failed to load peers registry: %w", err)
	}
	if ports.Ports == nil {
		ports.Ports = map[string]string{}
	}
	if peers.Peers == nil {
		peers.Peers = map[string]Registration{}
	}
	return ports, peers, nil
}

func loadJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return errors.New("registry file is empty")
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return err
	}
	return nil
}

func writeRegistry(baseDir string, ports PortsFile, peers PeersFile) error {
	registryDir := filepath.Join(baseDir, "registry")
	if err := os.MkdirAll(registryDir, 0700); err != nil {
		return err
	}
	if err := writeJSONAtomic(filepath.Join(registryDir, "ports.json"), ports); err != nil {
		return err
	}
	if err := writeJSONAtomic(filepath.Join(registryDir, "peers.json"), peers); err != nil {
		return err
	}
	return nil
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	// UnixNano alone collides under CI with a frozen/mocked clock and
	// (theoretically) under the real clock when two goroutines write in
	// the same nanosecond. Appending a crypto/rand suffix makes the temp
	// filename uniqueness independent of clock monotonicity.
	suffix := make([]byte, 4)
	if _, randErr := rand.Read(suffix); randErr != nil {
		return randErr
	}
	tmpPath := fmt.Sprintf("%s.tmp.%d.%s", path, time.Now().UnixNano(), hex.EncodeToString(suffix))
	// Write via fsync(temp) → rename → fsync(parent dir) so a crash between
	// the rename and the next page-cache flush cannot leave peers.json /
	// ports.json as a zero-byte file. loadJSON treats an empty file as an
	// error, so a torn write without this discipline would wedge subsequent
	// ReservePort/ReleasePort calls until the user hand-fixes the registry.
	payload := append(data, '\n')
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := f.Write(payload); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	// Best-effort parent-dir fsync. Some filesystems reject O_RDONLY+Sync
	// on a directory (Windows, certain FUSE mounts); a failure here does
	// not invalidate the write — the rename already succeeded.
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// lockRegistry acquires a directory-based mutex for the peer registry.
// This relies on mkdir atomicity on local filesystems; it is not safe on NFS.
// The registry lives under ~/.cache/cc-clip which is always local.
//
// On acquisition we stamp a pid file inside the lock directory so the
// staleness check can key off "holder is dead" instead of the 30-second
// mtime heuristic alone. The mtime fallback still exists for the case
// where the pid file is missing/unreadable (e.g. partially-written by a
// crashed writer), but the PID path closes the review P2-1 race where
// two processes both stall past 30 s and each declare the other stale.
func lockRegistry(baseDir string) (func(), error) {
	lockPath := filepath.Join(baseDir, "registry", "lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return nil, err
	}
	for i := 0; i < registryLockMaxAttempts; i++ {
		if err := os.Mkdir(lockPath, 0700); err == nil {
			writeLockHolderPID(lockPath)
			return func() { _ = os.RemoveAll(lockPath) }, nil
		} else if !os.IsExist(err) {
			return nil, err
		}
		stale, err := staleRegistryLock(lockPath)
		if err != nil {
			return nil, err
		}
		if stale {
			if err := os.RemoveAll(lockPath); err == nil || os.IsNotExist(err) {
				continue
			}
		}
		time.Sleep(registryLockRetryInterval)
	}
	return nil, fmt.Errorf("timed out waiting for registry lock")
}

// writeLockHolderPID records our PID inside the lock directory so
// staleRegistryLock can detect a dead holder without relying on the mtime
// heuristic alone. Best-effort: a write failure here just degrades us back
// to the mtime-only path for this lock acquisition.
func writeLockHolderPID(lockPath string) {
	pidPath := filepath.Join(lockPath, "pid")
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0600)
}

func staleRegistryLock(lockPath string) (bool, error) {
	info, err := os.Stat(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	// When a pid file is present inside the lock dir, use it as the
	// primary staleness signal. If the holder process is alive we are
	// NOT stale until the hard ceiling hits — so a long-running concurrent
	// cc-clip process (e.g. waiting on SSH passphrase, slow network) no
	// longer gets its lock forcibly stolen at the 30 s mark, but a recycled
	// PID from a crashed holder cannot pin the lock forever either.
	if pid, ok := readLockHolderPID(lockPath); ok {
		alive, aliveErr := processAlive(pid)
		if aliveErr != nil {
			return false, aliveErr
		}
		if alive {
			if time.Since(info.ModTime()) > registryLockHardCeiling {
				return true, nil
			}
			return false, nil
		}
		return true, nil
	}
	// No readable pid file — fall back to mtime heuristic.
	return time.Since(info.ModTime()) > registryLockStaleAfter, nil
}

func readLockHolderPID(lockPath string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(lockPath, "pid"))
	if err != nil {
		return 0, false
	}
	trimmed := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(trimmed)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func portAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
