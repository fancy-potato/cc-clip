package peer

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	portAvailableCheck = portAvailable
	// registryLockRetryInterval is the INITIAL backoff between mkdir
	// retries. After each EEXIST we double the wait up to
	// registryLockMaxBackoff so a long-held lock doesn't waste CPU on
	// 100ms polls for the full deadline. The previous fixed-interval
	// loop combined with registryLockMaxAttempts=50 capped total wait
	// at only ~5 s, which under contention from >5 concurrent
	// `cc-clip connect` runs caused legitimate holders to be reported
	// as timed-out before they finished.
	registryLockRetryInterval = 100 * time.Millisecond
	registryLockMaxBackoff    = 2 * time.Second
	// registryLockAcquireDeadline is the total time we wait for the
	// lock before reporting a hard timeout. 30 s is loose enough to
	// cover normal contention on a multi-laptop shared account but
	// still bounded so a wedged holder surfaces a real error rather
	// than blocking the CLI indefinitely. Tests shrink this to keep
	// suite latency low.
	registryLockAcquireDeadline = 30 * time.Second
	registryLockStaleAfter      = 30 * time.Second
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
			if ownedByUs || (unowned && portAvailableCheck(existing.ReservedPort)) {
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

	if existing, ok := peers.Peers[peerID]; ok {
		oldPortKey := strconv.Itoa(existing.ReservedPort)
		if ports.Ports[oldPortKey] == peerID {
			delete(ports.Ports, oldPortKey)
		}
	}

	for port := rangeStart; port <= rangeEnd; port++ {
		portKey := strconv.Itoa(port)
		// A non-empty owner here means some peer already has this port
		// reserved in the registry. We rely on loadRegistry's orphan-row
		// sweep (see loadRegistryFiles above) to have cleaned up rows
		// whose owner no longer exists in peers.json — without that
		// sweep, a crashed writer could leave a port reserved forever
		// and slowly starve the range. Keep the two in lockstep: any
		// future change that removes or defers the orphan sweep must
		// revisit this skip to avoid silent port-exhaustion regressions.
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
	_, peers, err := loadRegistryForRead(baseDir)
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
	_, peers, err := loadRegistryForRead(baseDir)
	if err != nil {
		return nil, err
	}
	out := make([]Registration, 0, len(peers.Peers))
	for _, reg := range peers.Peers {
		out = append(out, reg)
	}
	return out, nil
}

// loadRegistry loads both registry files for a read-write caller. It ensures
// the registry directory exists so a subsequent writeRegistry can succeed.
// Read-only callers (Lookup, ListAll) MUST use loadRegistryForRead instead
// to avoid the side effect of materializing ~/.cache/cc-clip/registry on
// hosts where the peer registry has never been written.
func loadRegistry(baseDir string) (PortsFile, PeersFile, error) {
	registryDir := filepath.Join(baseDir, "registry")
	if err := os.MkdirAll(registryDir, 0700); err != nil {
		return PortsFile{}, PeersFile{}, err
	}
	return loadRegistryFiles(registryDir)
}

// loadRegistryForRead loads both registry files without mutating the
// filesystem. If the registry directory does not exist yet, it returns
// empty (but initialized) PortsFile/PeersFile values — the semantic
// equivalent of "no peers registered yet" — so read-only probes like
// Lookup / ListAll cannot accidentally create cache directories on hosts
// that never ran `cc-clip connect`. That matters for uninstall-side
// callers that interpret an empty registry as "no other peers present";
// those callers must not trigger MkdirAll, and a missing-ENOENT read must
// return fail-safe (i.e. empty view) rather than an error that would be
// misclassified as a peer-count failure.
func loadRegistryForRead(baseDir string) (PortsFile, PeersFile, error) {
	registryDir := filepath.Join(baseDir, "registry")
	if _, err := os.Stat(registryDir); err != nil {
		if os.IsNotExist(err) {
			return emptyPortsFile(), emptyPeersFile(), nil
		}
		return PortsFile{}, PeersFile{}, err
	}
	return loadRegistryFiles(registryDir)
}

func emptyPortsFile() PortsFile {
	return PortsFile{
		Version:    registryVersion,
		RangeStart: DefaultRangeStart,
		RangeEnd:   DefaultRangeEnd,
		Ports:      map[string]string{},
	}
}

func emptyPeersFile() PeersFile {
	return PeersFile{
		Version: registryVersion,
		Peers:   map[string]Registration{},
	}
}

func loadRegistryFiles(registryDir string) (PortsFile, PeersFile, error) {
	ports := emptyPortsFile()
	peers := emptyPeersFile()

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
	// Refuse to load a registry written by a newer cc-clip that may carry
	// fields we would silently zero-out on rewrite. Run BEFORE the orphan
	// sweep: the sweep currently mutates only the in-memory value copy,
	// but a future refactor that adds persisted side effects must not run
	// against a schema this binary does not understand. Version skew is a
	// hard failure so the operator knows to upgrade or release stale
	// peers from the newer host.
	if peers.Version > registryVersion || ports.Version > registryVersion {
		return PortsFile{}, PeersFile{}, fmt.Errorf("peer registry version %d (ports) / %d (peers) is newer than cc-clip supports (%d); upgrade cc-clip on this host", ports.Version, peers.Version, registryVersion)
	}
	// Self-heal torn writes: peers.json and ports.json are written as two
	// separate atomic renames (see writeRegistry), so a crash between them
	// can leave a port row owned by a peer that no longer exists in
	// peers.json (or references a port the peer no longer holds). Drop
	// orphan port rows on load so a crash does not starve future
	// reservations — without this sweep, a stale port row would shadow its
	// slot from every subsequent ReservePort scan.
	for portKey, owner := range ports.Ports {
		if owner == "" {
			delete(ports.Ports, portKey)
			continue
		}
		reg, ok := peers.Peers[owner]
		if !ok {
			delete(ports.Ports, portKey)
			continue
		}
		if strconv.Itoa(reg.ReservedPort) != portKey {
			delete(ports.Ports, portKey)
		}
	}
	for peerID, reg := range peers.Peers {
		portKey := strconv.Itoa(reg.ReservedPort)
		if reg.ReservedPort <= 0 || ports.Ports[portKey] != peerID {
			delete(peers.Peers, peerID)
		}
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
	// Write ports.json BEFORE peers.json. A torn write between the renames
	// then leaves the safer intermediate state: an orphan port row whose
	// owner is missing from peers.json. loadRegistry already sweeps those
	// rows on read, so the next ReservePort/ReleasePort call self-heals.
	//
	// The opposite order (peers first) is unsafe: a crash after peers.json
	// lands but before ports.json does would leave a live peer reservation
	// with no occupied-port row, allowing a later ReservePort scan to hand
	// the same port to another peer.
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
	} else {
		// Best-effort: some platforms (Windows, certain FUSE mounts) refuse
		// O_RDONLY on directories. The rename itself already landed; the
		// only durability we forfeit is the parent-dir fsync. Surface the
		// failure on stderr so the operator has a breadcrumb if a torn
		// write does show up later, but do not fail the write — returning
		// an error here would spuriously break ReservePort on Windows/FUSE.
		fmt.Fprintf(os.Stderr, "cc-clip: registry dir open for fsync failed (best-effort): %v\n", err)
	}
	return nil
}

// lockRegistry acquires a directory-based mutex for the peer registry.
//
// Filesystem caveat: mkdir atomicity is a guarantee on local filesystems,
// NFSv4, and modern SMB. NFSv3 returns success on Mkdir races (the
// well-known NFSv3 Mkdir non-atomicity), and exotic FUSE mounts may
// behave similarly. The peer registry runs on BOTH the local laptop
// (~/.cache/cc-clip for self-identity) and the remote host (for
// shared-account port reservation) — on the remote the $HOME can legally
// be NFS-mounted. Two concurrent `cc-clip connect` runs from different
// laptops racing against an NFSv3 mkdir can each believe they hold the
// lock. The belt-and-braces defenses below keep state consistent in that
// regime:
//   - The PID-file stamp written inside the lock directory is performed
//     with os.WriteFile (NFS-safe), and readers of the stale-lock path
//     verify the stamped PID matches before reaping. A second writer who
//     races past mkdir will overwrite the pid file, but the first writer
//     will immediately observe the mismatch on release and leave the lock
//     in place for the second holder.
//   - loadRegistry self-heals orphan port rows from torn writes, so even
//     if two holders briefly coexist and each commits a partial pair, the
//     next full read restores a consistent view.
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
	// Deadline-based loop with exponential backoff. The previous form
	// (50 fixed attempts × 100ms = 5s total) was tight enough that 5+
	// concurrent connects on a shared account hit the timeout while
	// the legitimate holder was still making progress. We now budget
	// up to registryLockAcquireDeadline (default 30s) and double the
	// retry interval up to registryLockMaxBackoff so we don't spin.
	deadline := time.Now().Add(registryLockAcquireDeadline)
	backoff := registryLockRetryInterval
	for {
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
				if !sleepUntilOrFail(&backoff, deadline) {
					return nil, fmt.Errorf("timed out waiting for registry lock after %v (last writeLockHolderPID error: %w)", registryLockAcquireDeadline, writeErr)
				}
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
		if !sleepUntilOrFail(&backoff, deadline) {
			return nil, fmt.Errorf("timed out waiting for registry lock after %v", registryLockAcquireDeadline)
		}
	}
}

// sleepUntilOrFail sleeps the current backoff (clamped so we never
// overshoot the deadline), then doubles the backoff up to
// registryLockMaxBackoff. Returns false when the deadline has already
// passed (the caller should fail) and true after a successful sleep.
func sleepUntilOrFail(backoff *time.Duration, deadline time.Time) bool {
	now := time.Now()
	if !now.Before(deadline) {
		return false
	}
	wait := *backoff
	if remaining := deadline.Sub(now); wait > remaining {
		wait = remaining
	}
	time.Sleep(wait)
	*backoff *= 2
	if *backoff > registryLockMaxBackoff {
		*backoff = registryLockMaxBackoff
	}
	return true
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

// writeLockHolderPID records our PID, (on Linux) the kernel boot-id, and
// the wall-clock timestamp at lock-claim inside the lock directory so
// staleRegistryLock can distinguish "the same holder is still alive" from
// "a new process happens to share the recycled PID after a reboot". The
// on-disk format is up to three newline-terminated lines:
//
//	${pid}\n
//	${boot_id}\n       (only when readBootID succeeds — i.e. on Linux)
//	${claim_wall}\n    (RFC3339 UTC — belt-and-braces TTL sidecar)
//
// The third line pairs with the directory's mtime so a wall-clock jump
// (NTP step, container clock skew) cannot make a fresh lock appear stale.
// staleRegistryLock treats the lock as stale via TTL only when BOTH the
// mtime AND the recorded wall-clock are older than the threshold.
//
// Older single-line or two-line files are still parsed correctly by
// readLockHolderPID so an in-place upgrade does not invalidate any
// pre-existing lock.
//
// Returns an error so lockRegistry can tear the lock down and retry rather
// than return an unstamped lock — an unstamped lock is the precise
// pre-condition for the stealStaleLock "delete the wrong directory" race.
func writeLockHolderPID(lockPath string, pid int, bootID string) error {
	pidPath := filepath.Join(lockPath, "pid")
	payload := strconv.Itoa(pid) + "\n"
	if bootID != "" {
		payload += bootID + "\n"
	} else {
		// Keep the format positional: a missing boot-id gets an empty line
		// so the wall-clock timestamp stays on line 3 regardless of
		// platform. readLockHolderPID tolerates empty lines.
		payload += "\n"
	}
	payload += time.Now().UTC().Format(time.RFC3339Nano) + "\n"
	return os.WriteFile(pidPath, []byte(payload), 0600)
}

func releaseRegistryLock(lockPath string, ownerPID int, ownerBootID string) {
	currentPID, currentBootID, ok := readLockHolderPID(lockPath)
	if !ok {
		// Unreadable pid file: another party rewrote the lock after
		// we took it. Don't delete — that might destroy the new
		// holder's directory. Log so operators can see it happened.
		log.Printf("peer: releaseRegistryLock: pid file at %s is unreadable; lock may have been stolen by another process (our owner PID=%d). Leaving lock intact.", lockPath, ownerPID)
		return
	}
	if currentPID != ownerPID {
		log.Printf("peer: releaseRegistryLock: lock at %s has been re-stamped by another process (current PID=%d, expected our PID=%d) — possible lock steal via stealStaleLock during our hold. Leaving lock intact; registry writes under our hold are still on disk but the mutex was not exclusive across that window.", lockPath, currentPID, ownerPID)
		return
	}
	if ownerBootID != "" && currentBootID != ownerBootID {
		log.Printf("peer: releaseRegistryLock: lock at %s has a boot-id mismatch (current=%q, ours=%q) — machine likely rebooted during our hold. Leaving lock intact.", lockPath, currentBootID, ownerBootID)
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
	if pid, savedBootID, claimedAt, ok := readLockHolderFull(lockPath); ok {
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
		// unfamiliar Unix kill(2) errno). Treat those as "alive, flaky
		// probe" and fall through to the mtime/hard-ceiling check — the
		// ceiling still reaps the lock at registryLockHardCeiling if the
		// holder really is gone.
		//
		// The contract (documented in each processAlive implementation) is
		// that a (false, err != nil) return is NEVER produced: definite
		// "not alive" signals always come with nil err. Enforcing that
		// invariant via silent advisory-error suppression is deliberate:
		// a future probe author accidentally returning (false, someErr)
		// must NOT abort lock acquisition for every caller — that would
		// convert a probe regression into a registry-wide outage. Logging
		// the advisory error and proceeding with the heuristic keeps the
		// blast radius bounded.
		_ = aliveErr
		if alive {
			// Hard-ceiling: require BOTH the directory mtime AND the
			// recorded claimedAt to exceed the ceiling.
			//
			// For pid files written by cc-clip >= this release the pair
			// (mtime AND claimedAt) protects against wall-clock skew: a
			// backward NTP step or container clock jump would make
			// mtime alone appear stale on a freshly claimed lock, but
			// claimedAt — a single recorded wall-clock timestamp —
			// would still be in the future (or near-present) and keep
			// claimStale false.
			//
			// LEGACY PID FILES CAVEAT: cc-clip versions before the
			// wall-clock sidecar wrote only the pid (and optionally the
			// boot-id) — claimedAt comes back zero on those, so
			// `claimedAt.IsZero()` collapses the guard to mtime-only.
			// If the filesystem's mtime is reset by a backward NTP
			// step or clock skew AFTER such a legacy lock was taken,
			// that legacy lock CAN be prematurely reaped by the hard
			// ceiling path even when the holder is genuinely alive.
			// The fix is to retake the lock under a current cc-clip
			// (which will write the three-line format); we deliberately
			// do NOT upgrade-in-place here — rewriting a foreign
			// holder's pid file would defeat the whole ownership check.
			// On a shared-account host this window closes as soon as
			// every live holder has taken the lock at least once under
			// the new format.
			mtimeStale := time.Since(info.ModTime()) > registryLockHardCeiling
			claimStale := claimedAt.IsZero() || time.Since(claimedAt) > registryLockHardCeiling
			if mtimeStale && claimStale {
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
// The first line is the PID; an optional second line is the boot-id
// captured when the lock was taken; an optional third line is the
// claim-time RFC3339 wall-clock. An empty bootID return means either the
// writer was running on a non-Linux platform (readBootID is a no-op
// there) or the pid file predates the boot-id format. A zero-value
// claimedAt means the pid file predates the wall-clock format.
func readLockHolderPID(lockPath string) (pid int, bootID string, ok bool) {
	pid, bootID, _, ok = readLockHolderFull(lockPath)
	return pid, bootID, ok
}

func readLockHolderFull(lockPath string) (pid int, bootID string, claimedAt time.Time, ok bool) {
	data, err := os.ReadFile(filepath.Join(lockPath, "pid"))
	if err != nil {
		return 0, "", time.Time{}, false
	}
	// Do NOT TrimSpace the whole payload: that collapses a legitimate empty
	// line-2 (platform without boot-id) before we can index line-3.
	raw := strings.TrimRight(string(data), "\n")
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 {
		return 0, "", time.Time{}, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(lines[0]))
	// Reject pid <= 1. pid 0 is never a valid process; pid 1 is init
	// (systemd/launchd) on Unix — it's "always alive" but obviously not a
	// cc-clip holder, so a corrupt pid file containing `1` would otherwise
	// pin the lock until the 10-minute hard ceiling. Refusing to parse the
	// pid file in that case falls back to the mtime-only staleness heuristic,
	// which reaps the lock at registryLockStaleAfter (30 s) as expected for
	// a holder that left no valid PID trace.
	if err != nil || pid <= 1 {
		return 0, "", time.Time{}, false
	}
	if len(lines) >= 2 {
		candidate := strings.TrimSpace(lines[1])
		// Structural validation: boot-id values come from two narrow
		// formats — Linux writes a 36-char UUID from
		// /proc/sys/kernel/random/boot_id (e.g.
		// `11111111-1111-1111-1111-111111111111`), and Darwin writes
		// `<sec>.<usec>` from kern.boottime. Anything else (shell
		// escapes, injected junk, a corrupt second line that swallowed
		// the newline before the timestamp) is treated as "no boot-id"
		// so we fall back to PID+mtime rather than compare against
		// whatever readBootIDFn returns now. An empty line is
		// legitimate (non-Linux/non-Darwin platforms write one) and
		// stays empty.
		//
		// Corrupt boot-id used to cause the whole pid file to be
		// discarded (return ok=false), which collapsed the staleness
		// check from PID-liveness + hard-ceiling down to mtime-only
		// with a 30 s window — letting a flaky-IO partial-write or
		// truncation mid-append steal the lock from an actually-live
		// holder. Now we preserve the valid PID, blank the boot-id
		// (skipping the boot-mismatch shortcut), and let processAlive
		// drive the staleness decision. If the PID really is dead we
		// still reap immediately; if it's alive we wait for the hard
		// ceiling — both safer than the previous mtime-only path.
		if candidate == "" || isValidBootID(candidate) {
			bootID = candidate
		} else {
			bootID = ""
		}
	}
	if len(lines) >= 3 {
		if ts, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(lines[2])); parseErr == nil {
			claimedAt = ts
		}
	}
	return pid, bootID, claimedAt, true
}

// isValidBootID accepts the two on-disk boot-id formats cc-clip writes:
//   - Linux UUID from /proc/sys/kernel/random/boot_id: 36 chars, hex +
//     hyphens.
//   - Darwin `<sec>.<usec>` from kern.boottime: digits + a single dot.
//
// Anything else is a corrupt or injected value. Deliberately permissive
// on the Linux side (accepts any 32+ char hex-with-hyphens string, not
// just strict canonical UUID form) because cc-clip already treats the
// file contents as opaque-except-for-equality — we just need "this
// looks structurally like the kind of thing we would have written" as a
// sanity gate.
func isValidBootID(s string) bool {
	if len(s) < 8 {
		return false
	}
	// Darwin: digits, exactly one dot, digits. Cheap path first.
	if dot := strings.IndexByte(s, '.'); dot > 0 && dot < len(s)-1 {
		if strings.Count(s, ".") == 1 &&
			strings.IndexFunc(s, func(r rune) bool {
				return !(r == '.' || (r >= '0' && r <= '9'))
			}) == -1 {
			return true
		}
	}
	// Linux: UUID-ish — hex + hyphens, reasonable length.
	if len(s) < 32 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// portAvailable probes whether `port` is free for a local listener on
// both the loopback and any-address. When the registry runs on the
// REMOTE side (the shared-account multi-laptop path), "free on 127.0.0.1"
// is insufficient because an unrelated remote service may hold the same
// port on 0.0.0.0. Testing both addresses catches that: if either bind
// fails, treat the port as unavailable so the registry tries the next
// slot instead of letting `ExitOnForwardFailure` surface a late failure
// that the registry has no self-heal path for.
//
// A bind may transiently fail for TIME_WAIT / SO_REUSEADDR reasons even
// when the port is effectively free for a moment; that's acceptable —
// the registry will allocate the next port in the range and move on.
func portAvailable(port int) bool {
	if !tryListen(fmt.Sprintf("127.0.0.1:%d", port)) {
		return false
	}
	return tryListen(fmt.Sprintf("0.0.0.0:%d", port))
}

func tryListen(addr string) bool {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
