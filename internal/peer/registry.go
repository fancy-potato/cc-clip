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
	// readBootIDFn indirects readBootID so tests can simulate cross-boot PID
	// reuse without needing to touch /proc.
	readBootIDFn = readBootID
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
				// Keep reusing a port we already own even when it is currently
				// bound: the common case is our own active SSH reverse forward
				// holding the socket between reconnects. Only the `unowned`
				// branch treats a bind failure as evidence that some unrelated
				// process grabbed the port and we should reallocate.
				if ownedByUs || portAvailableCheck(existing.ReservedPort) {
					existing.Label = label
					existing.UpdatedAt = now
					existing.LastConnect = now
					stateDir, err := PeerStateDir(baseDir, peerID)
					if err != nil {
						return Registration{}, err
					}
					existing.StateDir = stateDir
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
				// Drop the stale registry entry for this port so the scanner
				// below doesn't skip it on a future peer when the registry no
				// longer has a trustworthy owner mapping.
				if unowned {
					delete(ports.Ports, portKey)
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
		stateDir, err := PeerStateDir(baseDir, peerID)
		if err != nil {
			return Registration{}, err
		}
		reg := Registration{
			PeerID:       peerID,
			Label:        label,
			ReservedPort: port,
			StateDir:     stateDir,
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
	// Sweep every port row owned by peerID, not just reg.ReservedPort. A prior
	// ReservePort migration can leave a stale row pointing at an old port if
	// the peer was ever moved across ports (the scan at the top of ReservePort
	// handles the common case, but a crash between writeRegistry calls or a
	// manual registry edit can leave orphans). Releasing only the currently-
	// recorded port would let those orphan rows permanently shadow that port
	// from future reservations by *any* peer on the host — silent starvation
	// on shared-account hosts. Mirror the NotFound branch above so the contract
	// "release removes every trace of peerID from the registry" holds regardless
	// of how the registry reached its current state.
	for portKey, owner := range ports.Ports {
		if owner == peerID {
			delete(ports.Ports, portKey)
		}
	}
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
	// Parent-dir fsync makes the rename durable across a crash. If we
	// couldn't even Open the parent, treat that as best-effort (some
	// platforms — Windows, certain FUSE mounts — refuse O_RDONLY on
	// directories). But if Open succeeded, a Sync failure is a real
	// durability signal that this function was designed to surface:
	// swallowing it would reintroduce the torn-write window the empty-
	// file guard in loadJSON already treats as a hard error.
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		syncErr := d.Sync()
		closeErr := d.Close()
		if syncErr != nil {
			return fmt.Errorf("fsync registry dir after rename: %w", syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close registry dir handle: %w", closeErr)
		}
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
//
// Stale locks are reclaimed via rename-then-remove (see stealStaleLock).
// A plain os.RemoveAll would race: two processes that both decided the
// lock was stale could each remove it, with the second remove destroying
// the first one's freshly created replacement and leaving both processes
// believing they hold the lock.
func lockRegistry(baseDir string) (func(), error) {
	lockPath := filepath.Join(baseDir, "registry", "lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return nil, err
	}
	for i := 0; i < registryLockMaxAttempts; i++ {
		if err := os.Mkdir(lockPath, 0700); err == nil {
			ownerPID := os.Getpid()
			ownerBootID, _ := readBootIDFn()
			// Stamp the pid file before returning. If the stamp fails we
			// MUST tear down the lock and retry: a held-but-unstamped lock
			// races with stealStaleLock, which would observe an empty pid
			// file in the renamed directory and (without the matching fix
			// below) delete it, leaving two processes both believing they
			// own lockPath. Failing fast here keeps that window closed.
			if writeErr := writeLockHolderPID(lockPath, ownerPID, ownerBootID); writeErr != nil {
				_ = os.RemoveAll(lockPath)
				time.Sleep(registryLockRetryInterval)
				continue
			}
			return func() { releaseRegistryLock(lockPath, ownerPID, ownerBootID) }, nil
		} else if !os.IsExist(err) {
			return nil, err
		}
		stale, observedPID, err := staleRegistryLock(lockPath)
		if err != nil {
			return nil, err
		}
		if stale {
			if err := stealStaleLock(lockPath, observedPID); err != nil {
				return nil, err
			}
			continue
		}
		time.Sleep(registryLockRetryInterval)
	}
	return nil, fmt.Errorf("timed out waiting for registry lock")
}

// stealStaleLock atomically reclaims a stale lock via rename-then-verify.
// If the rename succeeds but the reclaimed directory holds a pid other
// than the one we observed as stale, a competitor process released the
// stale lock and created a fresh one between our stale check and our
// rename — in that case we restore the fresh lock so the competitor keeps
// ownership and let the loop retry mkdir. If restoration fails (yet
// another process has since claimed lockPath), we drop the dir we stole
// and accept that some other process now holds the lock; the outer loop
// will observe their lock on the next iteration.
func stealStaleLock(lockPath string, observedPID int) error {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	stolenPath := lockPath + ".stolen." + hex.EncodeToString(suffix)
	if err := os.Rename(lockPath, stolenPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if observedPID == 0 {
		// We judged the original lock stale via the mtime-only fallback, so we
		// have no PID proving the renamed directory is still the same lock we
		// inspected. If a competitor recreated a fresh lock in the race window
		// before Rename, the stolen directory will either have a readable pid
		// file or a recent mtime. Restore it instead of deleting a live
		// replacement lock.
		if currentPID, _, ok := readLockHolderPID(stolenPath); ok && currentPID > 0 {
			if restoreErr := os.Rename(stolenPath, lockPath); restoreErr == nil {
				return nil
			}
			_ = os.RemoveAll(stolenPath)
			return nil
		}
		if info, err := os.Stat(stolenPath); err == nil && time.Since(info.ModTime()) <= registryLockStaleAfter {
			if restoreErr := os.Rename(stolenPath, lockPath); restoreErr == nil {
				return nil
			}
			_ = os.RemoveAll(stolenPath)
			return nil
		}
	}
	if observedPID > 0 {
		// PID alone is the right discriminator here even after the boot-id
		// addition: stealStaleLock and staleRegistryLock are called within
		// the same lockRegistry attempt — a single boot — so the boot-id of
		// any fresh competitor lock written between our stale check and our
		// rename is necessarily the current boot, identical to ours. The
		// PID-mismatch test is what tells us "a competitor swooped in".
		//
		// `!ok` (no readable pid file in the renamed directory) is treated
		// as the same "competitor swooped" signal: we entered this branch
		// because the original lock had a parseable pid file (that's how
		// observedPID got set), so an absent pid file proves the directory
		// was switched between our stat and our rename. Without restoring
		// here, two processes can race past `os.Mkdir` and both believe
		// they hold the lock — see lockRegistry's writeLockHolderPID
		// failure path which is the symmetric defense.
		currentPID, _, ok := readLockHolderPID(stolenPath)
		if !ok || currentPID != observedPID {
			if restoreErr := os.Rename(stolenPath, lockPath); restoreErr == nil {
				return nil
			}
			_ = os.RemoveAll(stolenPath)
			return nil
		}
	}
	_ = os.RemoveAll(stolenPath)
	return nil
}

// writeLockHolderPID records our PID and (on Linux) the kernel boot-id
// inside the lock directory so staleRegistryLock can distinguish "the same
// holder is still alive" from "a new process happens to share the recycled
// PID after a reboot". The on-disk format is one or two newline-terminated
// lines:
//
//	${pid}\n
//	${boot_id}\n   (only when readBootID succeeds — i.e. on Linux)
//
// Older single-line files are still parsed correctly by readLockHolderPID
// so an in-place upgrade does not invalidate any pre-existing lock.
//
// Returns an error so lockRegistry can tear the lock down and retry rather
// than return an unstamped lock — an unstamped lock is the precise
// pre-condition for the stealStaleLock "delete the wrong directory" race.
func writeLockHolderPID(lockPath string, pid int, bootID string) error {
	pidPath := filepath.Join(lockPath, "pid")
	payload := strconv.Itoa(pid) + "\n"
	if bootID != "" {
		payload += bootID + "\n"
	}
	return os.WriteFile(pidPath, []byte(payload), 0600)
}

func releaseRegistryLock(lockPath string, ownerPID int, ownerBootID string) {
	currentPID, currentBootID, ok := readLockHolderPID(lockPath)
	if !ok || currentPID != ownerPID {
		return
	}
	if ownerBootID != "" && currentBootID != ownerBootID {
		return
	}
	_ = os.RemoveAll(lockPath)
}

// staleRegistryLock reports whether lockPath refers to an abandoned lock
// directory. The second return value is the observed holder PID (or 0 when
// no pid file is readable); stealStaleLock uses it to verify that the dir
// it reclaims is the one we judged stale, not a fresh lock taken by a
// competitor between our stat and our rename.
func staleRegistryLock(lockPath string) (bool, int, error) {
	info, err := os.Stat(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, err
	}
	// When a pid file is present inside the lock dir, use it as the
	// primary staleness signal. If the holder process is alive we are
	// NOT stale until the hard ceiling hits — so a long-running concurrent
	// cc-clip process (e.g. waiting on SSH passphrase, slow network) no
	// longer gets its lock forcibly stolen at the 30 s mark, but a recycled
	// PID from a crashed holder cannot pin the lock forever either.
	if pid, savedBootID, ok := readLockHolderPID(lockPath); ok {
		// Boot-id mismatch is the strongest staleness signal we have: the
		// PID was recorded under a previous boot, so even if processAlive
		// says "alive" right now it must be a recycled PID belonging to an
		// unrelated process. We can short-circuit straight to "stale" and
		// reap without waiting for the hard ceiling — that's the whole
		// reason the boot-id was added (review P2: "PID-reuse across boot
		// can falsely keep registry lock"). Only Linux currently records a
		// boot-id (see readBootID); other platforms fall through to the
		// PID-only check below, which is the previous behavior.
		//
		// Empty savedBootID is NOT treated as implicit staleness: a pre-
		// upgrade cc-clip wrote only the PID line, and the original process
		// may still be alive and genuinely holding the lock. Reaping on the
		// "absence" of a boot-id would punish the upgrade path (kill a live
		// holder on the first lock-check after the binary was swapped). The
		// hard-ceiling path below still reaps recycled PIDs on upgrade; the
		// cost is the 10-minute wait, which is the correct safety trade.
		if savedBootID != "" {
			if currentBootID, bootErr := readBootIDFn(); bootErr == nil && currentBootID != "" && currentBootID != savedBootID {
				return true, pid, nil
			}
		}
		alive, aliveErr := processAlive(pid)
		// processAlive returns advisory errors as (alive=true, err!=nil) for
		// transient kernel hiccups (Windows GetExitCodeProcess failure, an
		// unfamiliar Unix kill(2) errno). Treating those as hard failures
		// would abort lock acquisition on flaky systems; treating them as
		// "definitely alive" would let a recycled-PID holder pin the lock
		// past the hard ceiling. The middle path: ignore the advisory error
		// and fall through to the mtime/hard-ceiling check, which still
		// reaps the lock at registryLockHardCeiling. A genuine hard failure
		// (alive=false with err) — currently unreachable but defended for
		// future probe code — bubbles up so the operator sees it.
		if aliveErr != nil && !alive {
			return false, pid, aliveErr
		}
		if alive {
			if time.Since(info.ModTime()) > registryLockHardCeiling {
				return true, pid, nil
			}
			return false, pid, nil
		}
		return true, pid, nil
	}
	// No readable pid file — fall back to mtime heuristic.
	return time.Since(info.ModTime()) > registryLockStaleAfter, 0, nil
}

// readLockHolderPID parses the lock pid file written by writeLockHolderPID.
// The first line is the PID; an optional second line is the boot-id captured
// when the lock was taken. An empty bootID return means either the writer
// was running on a non-Linux platform (readBootID is a no-op there) or the
// pid file predates the boot-id format.
func readLockHolderPID(lockPath string) (pid int, bootID string, ok bool) {
	data, err := os.ReadFile(filepath.Join(lockPath, "pid"))
	if err != nil {
		return 0, "", false
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		return 0, "", false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || pid <= 0 {
		return 0, "", false
	}
	if len(lines) >= 2 {
		bootID = strings.TrimSpace(lines[1])
	}
	return pid, bootID, true
}

func portAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
