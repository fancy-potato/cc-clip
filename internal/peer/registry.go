package peer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const registryVersion = 1

var (
	portAvailableCheck        = portAvailable
	registryLockRetryInterval = 100 * time.Millisecond
	registryLockMaxAttempts   = 50
	registryLockStaleAfter    = 30 * time.Second
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
			if owner, ok := ports.Ports[portKey]; owner == peerID || !ok || owner == "" {
				// Preserve the existing peer mapping even when the port is already
				// bound, because an active SSH RemoteForward for this peer is
				// expected to occupy the reserved port between reconnects.
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
		return Registration{}, fmt.Errorf("peer %s not found", peerID)
	}

	delete(peers.Peers, peerID)
	delete(ports.Ports, strconv.Itoa(reg.ReservedPort))
	if err := writeRegistry(baseDir, ports, peers); err != nil {
		return Registration{}, err
	}
	return reg, nil
}

func Lookup(baseDir, peerID string) (Registration, error) {
	_, peers, err := loadRegistry(baseDir)
	if err != nil {
		return Registration{}, err
	}
	reg, ok := peers.Peers[peerID]
	if !ok {
		return Registration{}, fmt.Errorf("peer %s not found", peerID)
	}
	return reg, nil
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
	tmpPath := fmt.Sprintf("%s.tmp.%d", path, time.Now().UnixNano())
	if err := os.WriteFile(tmpPath, append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// lockRegistry acquires a directory-based mutex for the peer registry.
// This relies on mkdir atomicity on local filesystems; it is not safe on NFS.
// The registry lives under ~/.cache/cc-clip which is always local.
func lockRegistry(baseDir string) (func(), error) {
	lockPath := filepath.Join(baseDir, "registry", "lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return nil, err
	}
	for i := 0; i < registryLockMaxAttempts; i++ {
		if err := os.Mkdir(lockPath, 0700); err == nil {
			return func() { _ = os.Remove(lockPath) }, nil
		} else if !os.IsExist(err) {
			return nil, err
		}
		stale, err := staleRegistryLock(lockPath)
		if err != nil {
			return nil, err
		}
		if stale {
			if err := os.Remove(lockPath); err == nil || os.IsNotExist(err) {
				continue
			}
		}
		time.Sleep(registryLockRetryInterval)
	}
	return nil, fmt.Errorf("timed out waiting for registry lock")
}

func staleRegistryLock(lockPath string) (bool, error) {
	info, err := os.Stat(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return time.Since(info.ModTime()) > registryLockStaleAfter, nil
}

func portAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
