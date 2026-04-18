package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// tunnelJitterRand is a package-level rand.Rand used to disperse periodic
// polls (adoptedLoop, and any future periodic consumers) across a host
// running many tunnels. It is seeded at init; correctness does not depend on
// unpredictability, only on the polls not all firing on the same tick.
var tunnelJitterRand = rand.New(rand.NewSource(time.Now().UnixNano()))
var tunnelJitterRandMu sync.Mutex

// Manager manages persistent SSH tunnels with auto-reconnect.
type Manager struct {
	// opMu serializes control operations so Up/Down/Remove/Shutdown do not race
	// with each other for the same tunnel entry.
	opMu                sync.Mutex
	mu                  sync.Mutex
	persistMu           sync.Mutex
	entries             map[string]*entry // keyed by host/local-port
	localPort           int
	stateDir            string
	ctx                 context.Context
	cancel              context.CancelFunc
	persistState        func(string, *TunnelState) error
	cleanupStaleProcess func(int, TunnelConfig) error
	findRunningProcess  func(TunnelConfig) (int, bool, error)
	processMatchesPID   func(int, TunnelConfig) (bool, error)
	stopProcess         func(*exec.Cmd) error
	// Timeouts — exported zero values fall back to defaults so tests can
	// shorten them without needing new constructors.
	stopGracePeriod      time.Duration
	stopKillPeriod       time.Duration
	saveStateTimeout     time.Duration
	shutdownWaitPerEntry time.Duration
}

const (
	defaultStopGracePeriod      = 5 * time.Second
	defaultStopKillPeriod       = 2 * time.Second
	defaultSaveStateTimeout     = 5 * time.Second
	defaultShutdownWaitPerEntry = 10 * time.Second
)

type entry struct {
	state *TunnelState
	cmd   *exec.Cmd
	// ctx is the context driving this entry's reconnect/adopted loop.
	// Storing it alongside cancel lets stopEntry's waitForEntryCommand
	// observe cancellation directly instead of polling until done closes,
	// which matters when the loop is blocked on a subprocess (ssh -G) that
	// doesn't respect ctx.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	// persistWg counts in-flight detached persist goroutines spawned by
	// this entry's loops (via saveEntryState -> persistStateForDiskTracked).
	// The loop goroutine closes done after updateState returns, but
	// persistStateForDiskTracked's detached writer may still be queued on
	// persistMu. Up() waits on the outgoing entry's persistWg after
	// stopEntry returns so a late cancel-path "stopped" persist cannot
	// win the mutex race and overwrite the replacement entry's
	// "connecting" placeholder on disk.
	persistWg sync.WaitGroup
}

// NewManager creates a new tunnel manager.
func NewManager(stateDir string) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		entries:              make(map[string]*entry),
		stateDir:             stateDir,
		ctx:                  ctx,
		cancel:               cancel,
		persistState:         saveStateForDisk,
		cleanupStaleProcess:  cleanupStaleTunnelProcess,
		findRunningProcess:   FindRunningTunnelProcess,
		processMatchesPID:    tunnelProcessMatchesPID,
		stopProcess:          stopTunnelProcess,
		stopGracePeriod:      defaultStopGracePeriod,
		stopKillPeriod:       defaultStopKillPeriod,
		saveStateTimeout:     defaultSaveStateTimeout,
		shutdownWaitPerEntry: defaultShutdownWaitPerEntry,
	}
}

func (m *Manager) stopGraceDuration() time.Duration    { return m.stopGracePeriod }
func (m *Manager) stopKillDuration() time.Duration     { return m.stopKillPeriod }
func (m *Manager) saveTimeoutDuration() time.Duration  { return m.saveStateTimeout }
func (m *Manager) shutdownWaitDuration() time.Duration { return m.shutdownWaitPerEntry }

// persistStateForDisk writes state to disk with a bounded wait so a wedged
// filesystem (NFS, frozen volume) cannot block a reconnect goroutine past
// shutdown. If the write outlives the timeout, the goroutine running it
// leaks until the IO unblocks, but the caller is free to proceed.
func (m *Manager) persistStateForDisk(st *TunnelState) error {
	return m.persistStateForDiskTracked(st, nil)
}

// persistStateForDiskTracked is persistStateForDisk plus optional entry
// tracking: when wg is non-nil the detached writer Add/Done's on it, so a
// subsequent waitPersistDrain can flush queued writes for a specific
// entry before a new value is persisted.
func (m *Manager) persistStateForDiskTracked(st *TunnelState, wg *sync.WaitGroup) error {
	st = cloneStateForDisk(st)
	timeout := m.saveTimeoutDuration()
	if timeout <= 0 {
		m.persistMu.Lock()
		defer m.persistMu.Unlock()
		return m.persistState(m.stateDir, st)
	}
	if wg != nil {
		wg.Add(1)
	}
	errCh := make(chan error, 1)
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		m.persistMu.Lock()
		defer m.persistMu.Unlock()
		errCh <- m.persistState(m.stateDir, st)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-errCh:
		return err
	case <-timer.C:
		return fmt.Errorf("persist tunnel state timed out after %v", timeout)
	}
}

// waitPersistDrain waits for the outgoing entry's detached persist
// goroutines to finish before the caller issues a replacement write.
// Returns true if the wg drained within timeout, false on timeout; the
// caller proceeds either way — a timeout means a wedged filesystem is
// already holding persistMu, and the subsequent persist would block on
// the same mutex anyway. The spawn of the waiter goroutine and its
// eventual wg.Wait() return is the only cost on the hot path when the
// wg is already empty.
func waitPersistDrain(wg *sync.WaitGroup, timeout time.Duration) bool {
	if wg == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	if timeout <= 0 {
		<-done
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// SetLocalPort records the daemon port that owns this manager's persisted
// tunnel state.
func (m *Manager) SetLocalPort(localPort int) {
	m.mu.Lock()
	m.localPort = localPort
	m.mu.Unlock()
}

// Cancel marks the manager as shutting down so callers can stop long-running
// startup/control loops before waiting for live processes to drain.
func (m *Manager) Cancel() {
	m.cancel()
}

// startupInspect caches the result of resolveRunningTunnelProcess so the
// LoadAndStartAll loop body can read it without re-issuing the slow ps/CIM
// call on the critical path. `done` is false for states the parallel
// pre-pass skipped (disabled, or not owned by this daemon's local port);
// resolve falls back to a synchronous inspect for those.
type startupInspect struct {
	pid   int
	found bool
	err   error
	done  bool
}

func (in startupInspect) resolve(m *Manager, s *TunnelState) (int, bool, error) {
	if in.done {
		return in.pid, in.found, in.err
	}
	return m.resolveRunningTunnelProcess(s)
}

// runStartupInspects concurrently resolves the running-process state for
// every enabled tunnel that this daemon owns. The mutation of s.PID
// inside resolveRunningTunnelProcess is preserved because each goroutine
// receives a unique state pointer (states slice elements are not aliased
// across iterations). Concurrency is capped so a machine with hundreds of
// saved tunnels does not fork hundreds of ps/CIM subprocesses at once —
// each inspect is a fork, and unbounded fan-out can exhaust fds or pids on
// constrained systems.
const startupInspectConcurrency = 8

// shutdownStopConcurrency caps the fan-out of stopEntry during Shutdown so a
// daemon with hundreds of saved tunnels does not spawn hundreds of taskkill
// / ps / CIM subprocesses simultaneously. Symmetric with
// startupInspectConcurrency.
const shutdownStopConcurrency = 8

func (m *Manager) runStartupInspects(states []*TunnelState, localPort int) []startupInspect {
	inspects := make([]startupInspect, len(states))
	if len(states) == 0 {
		return inspects
	}
	sem := make(chan struct{}, startupInspectConcurrency)
	var wg sync.WaitGroup
	for i, s := range states {
		if s == nil {
			continue
		}
		if s.Config.LocalPort != 0 && s.Config.LocalPort != localPort {
			continue
		}
		if !s.Config.Enabled {
			continue
		}
		i, s := i, s
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Honour a manager cancel while waiting for a concurrency slot —
			// without this, Shutdown() called during startup has to wait for
			// every pending inspect to complete before it can acquire opMu
			// and start draining live entries, which can exceed launchd's
			// grace window on a host with many saved tunnels.
			select {
			case sem <- struct{}{}:
			case <-m.ctx.Done():
				inspects[i] = startupInspect{err: m.ctx.Err(), done: true}
				return
			}
			defer func() { <-sem }()
			if err := m.ctx.Err(); err != nil {
				inspects[i] = startupInspect{err: err, done: true}
				return
			}
			pid, found, err := m.resolveRunningTunnelProcess(s)
			inspects[i] = startupInspect{pid: pid, found: found, err: err, done: true}
		}()
	}
	wg.Wait()
	return inspects
}

// LoadAndStartAll loads saved tunnel configs for the active daemon port
// and starts enabled ones. Called on daemon startup.
//
// opMu is held across the whole loop so concurrent HTTP Up/Down/Remove
// cannot race the inspect→adopt transition. To keep the locked window
// short, per-state ps/CIM inspects are pre-computed in parallel before
// the main loop runs — startup wall-clock is then O(slowest inspect)
// instead of O(N × inspect).
func (m *Manager) LoadAndStartAll(localPort int) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.SetLocalPort(localPort)

	states, err := LoadAllStates(m.stateDir)
	if err != nil {
		log.Printf("tunnel-manager: failed to load states: %v", err)
		return
	}
	inspects := m.runStartupInspects(states, localPort)
	for i, s := range states {
		if err := m.ctx.Err(); err != nil {
			return
		}
		if s.Config.LocalPort != 0 && s.Config.LocalPort != localPort {
			log.Printf("tunnel-manager: skipping %s from local port %d (daemon on %d)", s.Config.Host, s.Config.LocalPort, localPort)
			continue
		}
		if s.Config.LocalPort == 0 {
			s.Config.LocalPort = localPort
		}
		if m.hasLiveEntry(s.Config.Host, s.Config.LocalPort) {
			continue
		}
		if s.Config.Enabled {
			pid, found, err := inspects[i].resolve(m, s)
			if err != nil {
				log.Printf("tunnel-manager: failed to inspect existing tunnel for %s: %v", s.Config.Host, err)
				s.Status = StatusDisconnected
				s.LastError = fmt.Sprintf("inspect running tunnel: %v", err)
				s.PID = 0
				if saveErr := m.persistStateForDisk(s); saveErr != nil {
					log.Printf("tunnel-manager: failed to persist inspect error for %s: %v", s.Config.Host, saveErr)
				}
				continue
			}
			if found && m.processMatchesPID != nil {
				// Re-validate immediately before the adoptTunnel handoff.
				// Between resolveRunningTunnelProcess and now the ssh
				// process may have exited and the PID may have been
				// recycled; persisting "connecting" with a stale PID
				// would only self-correct on the next daemon restart
				// while List()/SwiftBar showed a phantom in-flight tunnel
				// in the meantime.
				if matches, perr := m.processMatchesPID(pid, s.Config); perr != nil {
					log.Printf("tunnel-manager: adoptive re-inspect failed for %s pid %d: %v", s.Config.Host, pid, perr)
					// The initial startup inspect already proved this PID
					// matched the recorded tunnel. A second inspect can still
					// fail transiently (ps/CIM hiccup, /proc race) while the
					// tunnel remains healthy; dropping `found` here would fall
					// through into stale-PID cleanup and could kill the live
					// ssh we just verified. Keep the adoption and let
					// adoptedLoop retry on its normal cadence.
				} else if !matches {
					log.Printf("tunnel-manager: pid %d no longer owns the tunnel for %s; not adopting", pid, s.Config.Host)
					found = false
					s.PID = 0
				}
			}
			if found {
				s.PID = pid
				// Leave the adopted tunnel in "connecting" until adoptedLoop's
				// first successful inspect confirms the PID still matches.
				// Persisting "connected" before the first poll would write a
				// lie to disk if the PID had already been recycled between
				// findRunningProcess and adoptTunnel.
				s.Status = StatusConnecting
				s.LastError = ""
				if s.StartedAt == nil {
					now := time.Now()
					s.StartedAt = &now
				}
				if saveErr := m.persistStateForDisk(s); saveErr != nil {
					s.PersistenceError = saveErr.Error()
					log.Printf("tunnel-manager: failed to persist adopted state for %s: %v", s.Config.Host, saveErr)
				} else {
					s.PersistenceError = ""
				}
				m.adoptTunnel(s)
				continue
			}
			// Enabled tunnel with a recorded PID whose process is no longer
			// running. cleanupRecordedTunnelProcess will also zero s.PID so
			// we do not retry the stale PID on the next startup.
		}
		if s.PID > 0 {
			// Re-check cancellation before each stale-PID cleanup.
			// cleanupRecordedTunnelProcess fans out to bounded ps/CIM probes
			// and SIGTERM/SIGKILL escalation that each run for seconds; on a
			// host with many saved tunnels this loop can otherwise block
			// Shutdown() past launchd's grace window even though m.ctx has
			// already been cancelled.
			if err := m.ctx.Err(); err != nil {
				return
			}
			if err := m.cleanupRecordedTunnelProcess(s); err != nil {
				log.Printf("tunnel-manager: stale process cleanup failed for %s: %v", s.Config.Host, err)
				if s.Config.Enabled {
					s.Status = StatusDisconnected
				} else {
					s.Status = StatusStopped
				}
				s.LastError = staleProcessErrorMessage(s, err)
				// Zero the PID on persist so the next startup does not retry
				// the same stale PID — if the cleanup really failed due to a
				// permission issue the PID is likely someone else's now.
				s.PID = 0
				if saveErr := m.persistStateForDisk(s); saveErr != nil {
					log.Printf("tunnel-manager: failed to persist cleanup error for %s: %v", s.Config.Host, saveErr)
				}
				continue
			}
			if !s.Config.Enabled {
				s.Status = StatusStopped
				s.LastError = ""
				if saveErr := m.persistStateForDisk(s); saveErr != nil {
					log.Printf("tunnel-manager: failed to persist cleanup for %s: %v", s.Config.Host, saveErr)
				}
			}
		}
		if !s.Config.Enabled {
			continue
		}
		if !s.Config.SSHConfigResolved {
			s.Status = StatusDisconnected
			s.LastError = fmt.Sprintf("%s for %s; rerun `cc-clip tunnel up %s`", ErrTunnelSSHOptionsUnresolved, s.Config.Host, s.Config.Host)
			s.PID = 0
			if saveErr := m.persistStateForDisk(s); saveErr != nil {
				log.Printf("tunnel-manager: failed to persist unresolved ssh options for %s: %v", s.Config.Host, saveErr)
			}
			continue
		}
		if err := m.startTunnel(s.Config); err != nil {
			log.Printf("tunnel-manager: failed to start %s: %v", s.Config.Host, err)
		}
	}
}

// Up starts (or restarts) a persistent tunnel.
func (m *Manager) Up(cfg TunnelConfig) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.ensureActiveLocked(); err != nil {
		return err
	}
	if err := m.validateOwnedLocalPort(cfg.LocalPort); err != nil {
		return err
	}
	cfg, err := resolveSSHTunnelConfig(m.ctx, cfg)
	if err != nil {
		return fmt.Errorf("resolve ssh config for %s: %w", cfg.Host, err)
	}

	cfg.Enabled = true
	st := connectingTunnelState(cfg)

	// Persist the replacement state before disturbing the live tunnel so a
	// persistence failure cannot take down an otherwise healthy connection.
	if err := m.persistStateForDisk(st); err != nil {
		if errors.Is(err, ErrInvalidTunnelState) {
			return fmt.Errorf("invalid tunnel config for %s: %w", cfg.Host, err)
		}
		return fmt.Errorf("persist tunnel state for %s: %w", cfg.Host, err)
	}

	key := stateKey(cfg.Host, cfg.LocalPort)
	m.mu.Lock()
	old := m.entries[key]
	m.mu.Unlock()

	if old != nil {
		if err := m.stopEntry(old); err != nil {
			// The old entry's reconnect goroutine may still be alive and
			// mutating old.state. Snapshot under m.mu so the restore write
			// reads a consistent copy rather than racing field-by-field.
			m.mu.Lock()
			snapshot := cloneState(old.state)
			m.mu.Unlock()
			if snapshot != nil {
				if restoreErr := m.persistStateForDisk(snapshot); restoreErr != nil {
					log.Printf("tunnel-manager: failed to restore state for %s after stop failure: %v", cfg.Host, restoreErr)
				}
			}
			return fmt.Errorf("stop existing tunnel for %s: %w", cfg.Host, err)
		}
		// The old reconnect loop's cancel-path updateState(StatusStopped)
		// spawns a detached persist goroutine that may still be queued on
		// persistMu after stopEntry returns (done-channel close happens
		// before the detached writer drains). persistMu does not guarantee
		// FIFO, so a late-arriving "stopped" write can win the mutex race
		// against the "connecting" placeholder below and leave disk state
		// inconsistent with the live entry. Drain the old entry's wg with
		// a bounded wait to flush those queued writes first.
		if !waitPersistDrain(&old.persistWg, m.saveTimeoutDuration()) {
			log.Printf("tunnel-manager: drain of outgoing entry persists for %s timed out; proceeding (disk may momentarily reflect stale state)", cfg.Host)
		}
		// The old reconnect loop may have persisted its stopped state during
		// shutdown; restore the connecting placeholder before launching the
		// replacement tunnel.
		if err := m.persistStateForDisk(st); err != nil {
			return fmt.Errorf("persist tunnel state for %s: %w", cfg.Host, err)
		}
	}

	m.mu.Lock()
	if current, ok := m.entries[key]; ok && current == old {
		delete(m.entries, key)
	}
	m.mu.Unlock()

	m.launchTunnel(st)
	return nil
}

// Down stops a tunnel and marks it disabled.
func (m *Manager) Down(host string, localPort int) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.ensureActiveLocked(); err != nil {
		return err
	}

	return m.downLocked(host, localPort)
}

// Remove stops a tunnel and deletes its state file.
func (m *Manager) Remove(host string, localPort int) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.ensureActiveLocked(); err != nil {
		return err
	}

	if localPort == 0 {
		localPort = m.localPort
	}
	if err := m.downLocked(host, localPort); err != nil && !errors.Is(err, ErrTunnelNotFound) {
		return err
	}
	return RemoveState(m.stateDir, host, localPort)
}

// List returns all known tunnel states, merging live entries with on-disk state.
//
// The disk read runs under m.mu so a concurrent Up() that persists a new
// state file cannot win the race between "LoadAllStates scan" and
// "m.entries snapshot" and thereby drop the fresh tunnel from the merge.
func (m *Manager) List() ([]*TunnelState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	diskStates, err := LoadAllStates(m.stateDir)
	if err != nil {
		return nil, err
	}

	merged := make(map[string]*TunnelState, len(diskStates))
	for _, s := range diskStates {
		if s == nil {
			continue
		}
		merged[stateKey(s.Config.Host, s.Config.LocalPort)] = s
	}
	// Override with live entry states (more current).
	for key, e := range m.entries {
		if e == nil || e.state == nil {
			continue
		}
		merged[key] = cloneState(e.state)
	}

	result := make([]*TunnelState, 0, len(merged))
	for _, s := range merged {
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Config.Host == result[j].Config.Host {
			return result[i].Config.LocalPort < result[j].Config.LocalPort
		}
		return result[i].Config.Host < result[j].Config.Host
	})
	return result, nil
}

func (m *Manager) hasLiveEntry(host string, localPort int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.entries[stateKey(host, localPort)]
	return ok
}

func (m *Manager) validateOwnedLocalPort(localPort int) error {
	// Callers must have resolved localPort before reaching here. Accepting
	// 0 used to "mean unspecified, skip the check" and silently let an
	// invalid port slip through to SaveState, which then rejected it with
	// a persistence error instead of a clean validation error. Fail fast.
	if localPort <= 0 || localPort > 65535 {
		return fmt.Errorf("%w: local_port %d out of range [1, 65535]", ErrTunnelLocalPortMismatch, localPort)
	}

	m.mu.Lock()
	managerPort := m.localPort
	m.mu.Unlock()

	if managerPort == 0 || localPort == managerPort {
		return nil
	}
	return fmt.Errorf("%w: daemon on local port %d cannot manage tunnel for local port %d", ErrTunnelLocalPortMismatch, managerPort, localPort)
}

func cloneState(s *TunnelState) *TunnelState {
	if s == nil {
		return nil
	}
	cp := *s
	if s.StartedAt != nil {
		startedAt := *s.StartedAt
		cp.StartedAt = &startedAt
	}
	if s.StoppedAt != nil {
		stoppedAt := *s.StoppedAt
		cp.StoppedAt = &stoppedAt
	}
	// Match cloneStateForDisk: deep-copy SSHOptions so List() consumers
	// cannot share the backing array with the live entry. Prevents a
	// future element-wise mutation on the returned snapshot from racing
	// with the reconnect loop reading the live slice.
	if len(s.Config.SSHOptions) > 0 {
		cp.Config.SSHOptions = append([]string(nil), s.Config.SSHOptions...)
	}
	return &cp
}

func (m *Manager) loadOwnedState(host string, localPort int) (*TunnelState, error) {
	if localPort > 0 {
		return LoadState(m.stateDir, host, localPort)
	}
	return LoadStateByHost(m.stateDir, host)
}

func (m *Manager) cleanupRecordedTunnelProcess(s *TunnelState) error {
	if s == nil {
		return nil
	}
	pid := s.PID
	if pid <= 0 && s.Config.Enabled && m.findRunningProcess != nil {
		foundPID, found, err := m.findRunningProcess(s.Config)
		if err != nil {
			// Ambiguity with no recorded PID is unsafe to paper over:
			// silently returning nil would leave both orphan ssh
			// processes alive while marking state stopped, and the
			// next startTunnel's ExitOnForwardFailure would then fail
			// to bind forever. Surface to the operator.
			return err
		}
		if found {
			pid = foundPID
			s.PID = foundPID
		}
	}
	if pid <= 0 {
		return nil
	}
	if err := m.cleanupStaleProcess(pid, s.Config); err != nil {
		return err
	}
	s.PID = 0
	return nil
}

func (m *Manager) resolveRunningTunnelProcess(s *TunnelState) (int, bool, error) {
	if s == nil {
		return 0, false, nil
	}
	if s.PID > 0 {
		if m.processMatchesPID == nil {
			return s.PID, true, nil
		}
		matches, err := m.processMatchesPID(s.PID, s.Config)
		if err != nil {
			return 0, false, err
		}
		if matches {
			return s.PID, true, nil
		}
		s.PID = 0
	}
	if !s.Config.Enabled || m.findRunningProcess == nil {
		return 0, false, nil
	}
	return m.findRunningProcess(s.Config)
}

func staleProcessErrorMessage(s *TunnelState, err error) string {
	if s != nil && s.PID > 0 {
		return fmt.Sprintf("stale tunnel pid %d: %v", s.PID, err)
	}
	return fmt.Sprintf("stale tunnel process: %v", err)
}

func (m *Manager) ensureActiveLocked() error {
	if err := m.ctx.Err(); err != nil {
		return ErrManagerShuttingDown
	}
	return nil
}

// Shutdown stops all tunnels gracefully.
func (m *Manager) Shutdown() {
	// Signal cancellation *before* queueing on opMu. If LoadAndStartAll is
	// still holding opMu when Shutdown is called (e.g. daemon was killed
	// mid-startup), its per-state ctx check and the inspect goroutines
	// need to observe the cancel so they return promptly — otherwise
	// Shutdown blocks on opMu for the entire startup budget before it can
	// start draining live entries.
	m.cancel()

	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	entries := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	// Fan out stopEntry across entries. Serial iteration could take
	// len(entries) * (stopGracePeriod + stopKillPeriod) = N*7s in the worst
	// case, which exceeds launchd's default ~20s grace for 3+ tunnels and
	// results in SIGKILL'ed tunnel children getting no chance to clean up.
	// Each stopEntry already has its own bounded timeouts, so the outer
	// waitgroup completes within the longest individual stop, not their sum.
	//
	// Cap concurrency to shutdownStopConcurrency so a daemon with hundreds of
	// saved tunnels does not spawn hundreds of `taskkill` / `ps` subprocesses
	// at once. Symmetric with runStartupInspects' startupInspectConcurrency.
	sem := make(chan struct{}, shutdownStopConcurrency)
	var wg sync.WaitGroup
	for _, e := range entries {
		e := e
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := m.stopEntry(e); err != nil && e != nil && e.state != nil {
				log.Printf("tunnel-manager: failed to stop %s on local port %d during shutdown: %v", e.state.Config.Host, e.state.Config.LocalPort, err)
			}
		}()
	}
	wg.Wait()

	// Defense in depth: stopEntry's internal timeouts should already have
	// elapsed, but if a reconnect goroutine is still running (e.g. wedged in
	// disk IO that outlived saveStateTimeout), wait a bounded additional
	// window for e.done before giving up. Goroutines that outlive this leak,
	// but we refuse to let Shutdown hang launchd past its grace period.
	//
	// Use a single shared deadline across all entries rather than a per-entry
	// timer: N leaking goroutines would otherwise stack to N*budget, which
	// blows past launchd's grace window even though each individual timeout
	// is bounded.
	shutdownBudget := m.shutdownWaitDuration()
	deadline := time.Now().Add(shutdownBudget)
	for _, e := range entries {
		if e == nil {
			continue
		}
		m.mu.Lock()
		done := e.done
		m.mu.Unlock()
		if done == nil {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if e.state != nil {
				log.Printf("tunnel-manager: reconnect goroutine for %s on local port %d did not exit within shutdown budget %v; leaking", e.state.Config.Host, e.state.Config.LocalPort, shutdownBudget)
			} else {
				log.Printf("tunnel-manager: reconnect goroutine did not exit within shutdown budget %v; leaking", shutdownBudget)
			}
			continue
		}
		timer := time.NewTimer(remaining)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
			if e.state != nil {
				log.Printf("tunnel-manager: reconnect goroutine for %s on local port %d did not exit within shutdown budget %v; leaking", e.state.Config.Host, e.state.Config.LocalPort, shutdownBudget)
			} else {
				log.Printf("tunnel-manager: reconnect goroutine did not exit within shutdown budget %v; leaking", shutdownBudget)
			}
		}
	}
}

func (m *Manager) downLocked(host string, localPort int) error {
	if localPort == 0 {
		localPort = m.localPort
	}
	if err := m.validateOwnedLocalPort(localPort); err != nil {
		return err
	}
	key := stateKey(host, localPort)

	m.mu.Lock()
	e, ok := m.entries[key]
	if !ok {
		m.mu.Unlock()
		// No live entry — update state file if it exists.
		s, err := m.loadOwnedState(host, localPort)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: %s", ErrTunnelNotFound, host)
			}
			return err
		}
		if err := m.cleanupRecordedTunnelProcess(s); err != nil {
			return fmt.Errorf("stop stale tunnel process for %s: %w", host, err)
		}
		s.Config.Enabled = false
		s.Status = StatusStopped
		now := time.Now()
		s.StoppedAt = &now
		s.PID = 0
		return m.persistStateForDisk(s)
	}
	m.mu.Unlock()

	if err := m.stopEntry(e); err != nil {
		return fmt.Errorf("stop tunnel for %s: %w", host, err)
	}

	m.mu.Lock()
	delete(m.entries, key)
	var snapshot *TunnelState
	if e.state != nil {
		e.state.Config.Enabled = false
		e.state.Status = StatusStopped
		now := time.Now()
		e.state.StoppedAt = &now
		e.state.PID = 0
		snapshot = cloneState(e.state)
	}
	m.mu.Unlock()

	if snapshot == nil {
		return nil
	}
	snapshot.PersistenceError = ""
	return m.persistStateForDisk(snapshot)
}

// startTunnel creates an entry and launches the reconnect loop.
// Acquires m.mu internally; caller must NOT hold it.
func (m *Manager) startTunnel(cfg TunnelConfig) error {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	st := connectingTunnelState(cfg)
	if err := m.persistStateForDisk(st); err != nil {
		return fmt.Errorf("persist tunnel state for %s: %w", cfg.Host, err)
	}
	m.launchTunnel(st)
	return nil
}

func connectingTunnelState(cfg TunnelConfig) *TunnelState {
	return &TunnelState{
		Config: cfg,
		Status: StatusConnecting,
	}
}

func (m *Manager) launchTunnel(st *TunnelState) {
	ctx, cancel := context.WithCancel(m.ctx)
	m.mu.Lock()
	e := &entry{state: st, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	m.entries[stateKey(st.Config.Host, st.Config.LocalPort)] = e
	// Snapshot the config under lock so the goroutine never races with
	// a concurrent updateState that might rewrite e.state fields.
	cfgSnapshot := st.Config
	// If the manager has already begun shutting down between Up()
	// persisting "connecting" and this point, roll the state back to
	// disconnected/stopped so we do not leak a stale "connecting" entry
	// on disk.
	if err := m.ctx.Err(); err != nil {
		// Pull the entry back out of the map — no goroutine will
		// service it and leaving it stranded would make Shutdown()
		// block on an e.done we close inline.
		delete(m.entries, stateKey(st.Config.Host, st.Config.LocalPort))
		rolledBack := rollbackToTerminalStatus(st)
		m.mu.Unlock()
		if rolledBack {
			_ = m.persistStateForDisk(st)
		}
		close(e.done)
		cancel()
		return
	}
	m.mu.Unlock()
	go m.reconnectLoop(ctx, e, cfgSnapshot)
}

func (m *Manager) adoptTunnel(st *TunnelState) {
	ctx, cancel := context.WithCancel(m.ctx)
	m.mu.Lock()
	e := &entry{state: st, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	m.entries[stateKey(st.Config.Host, st.Config.LocalPort)] = e
	cfgSnapshot := st.Config
	pidSnapshot := st.PID
	if err := m.ctx.Err(); err != nil {
		delete(m.entries, stateKey(st.Config.Host, st.Config.LocalPort))
		rolledBack := rollbackToTerminalStatus(st)
		m.mu.Unlock()
		if rolledBack {
			_ = m.persistStateForDisk(st)
		}
		close(e.done)
		cancel()
		return
	}
	m.mu.Unlock()
	go m.adoptedLoop(ctx, e, cfgSnapshot, pidSnapshot)
}

// rollbackToTerminalStatus converts a still-connecting tunnel state to
// its correct terminal status when the manager has shut down before the
// reconnect goroutine had a chance to run. Returns true if the state
// was actually changed.
func rollbackToTerminalStatus(st *TunnelState) bool {
	if st == nil {
		return false
	}
	target := StatusDisconnected
	if !st.Config.Enabled {
		target = StatusStopped
	}
	if st.Status == target {
		return false
	}
	st.Status = target
	st.PID = 0
	if target == StatusStopped {
		now := time.Now()
		st.StoppedAt = &now
	}
	return true
}

// reconnectLoop runs SSH, waits for exit, and reconnects with backoff.
// cfg is a snapshot of the tunnel configuration taken under m.mu at the
// point the loop was launched. Ports/host are persistent identity fields
// that never change for a given entry, so using the snapshot throughout
// avoids racy lock-free reads from e.state while updateState writes to it.
func (m *Manager) reconnectLoop(ctx context.Context, e *entry, cfg TunnelConfig) {
	defer close(e.done)

	const (
		initialBackoff = 2 * time.Second
		maxBackoff     = 60 * time.Second
		// stabilityWindow is how long a tunnel must stay "connected" before
		// we treat the current backoff as having been earned back. A remote
		// that logs ssh's "remote forward success" phrase then drops the
		// socket seconds later is a flapping remote, not a recovered one —
		// resetting backoff on the bare ready signal would hammer it at the
		// base interval forever.
		stabilityWindow = 30 * time.Second
	)
	backoff := initialBackoff
	var connectedAt time.Time

	// escalate decides whether the next reconnect waits at `backoff` or
	// starts back at `initialBackoff`. Reset is only granted when the
	// previous connection was both confirmed-ready AND held together for
	// at least stabilityWindow.
	escalate := func() {
		if !connectedAt.IsZero() && time.Since(connectedAt) >= stabilityWindow {
			backoff = initialBackoff
		} else {
			backoff = min(backoff*2, maxBackoff)
		}
		connectedAt = time.Time{}
	}

	// If the manager cancelled before the goroutine was scheduled,
	// roll the persisted "connecting" state back to its terminal
	// status so we do not leave stale files behind.
	if err := ctx.Err(); err != nil {
		m.mu.Lock()
		rolledBack := rollbackToTerminalStatus(e.state)
		st := cloneState(e.state)
		m.mu.Unlock()
		if rolledBack && st != nil {
			_ = m.persistStateForDisk(st)
		}
		return
	}

	for {
		if err := ctx.Err(); err != nil {
			return
		}

		args, cleanupArgs, err := sshTunnelArgs(ctx, cfg)
		if err != nil {
			m.updateState(e, StatusDisconnected, 0, err.Error(), false)
			if errors.Is(err, ErrTunnelSSHOptionsUnresolved) {
				return
			}
			if !m.sleepCtx(ctx, backoff) {
				return
			}
			escalate()
			m.bumpReconnectCount(e)
			continue
		}

		sshPath, err := resolveSSHBinary()
		if err != nil {
			cleanupArgs()
			m.updateState(e, StatusDisconnected, 0, err.Error(), false)
			if !m.sleepCtx(ctx, backoff) {
				return
			}
			escalate()
			m.bumpReconnectCount(e)
			continue
		}

		// exec.CommandContext binds ssh lifetime to ctx so a lingering
		// cancelled goroutine cannot orphan an ssh child. By default ctx
		// cancel delivers SIGKILL immediately, which would defeat
		// stopEntry's SIGTERM-then-SIGKILL escalation: a Down/Shutdown
		// that cancels ctx before stopEntry runs stopProcess races Go's
		// own killer, and ssh often loses before it can close remote
		// forwards cleanly. Override Cancel to a no-op; stopEntry is the
		// single owner of "stop this ssh" and applies the documented
		// grace period. WaitDelay caps how long we wait after stop before
		// Go force-closes the child's I/O pipes, so a wedged ssh that
		// ignores both signals cannot leak the stderr-reader goroutine.
		cmd := exec.CommandContext(ctx, sshPath, args...)
		cmd.Cancel = func() error { return nil }
		cmd.WaitDelay = m.stopGraceDuration() + m.stopKillDuration() + 2*time.Second
		cmd.SysProcAttr = tunnelSysProcAttr()
		// Force C locale so watchTunnelReady's English phrase match is
		// not defeated by a user shell env that sets a translated ssh
		// output locale.
		cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
		stderr, err := cmd.StderrPipe()
		if err != nil {
			cleanupArgs()
			m.updateState(e, StatusDisconnected, 0, err.Error(), false)
			if !m.sleepCtx(ctx, backoff) {
				return
			}
			escalate()
			m.bumpReconnectCount(e)
			continue
		}

		if err := cmd.Start(); err != nil {
			cleanupArgs()
			m.updateState(e, StatusDisconnected, 0, err.Error(), false)
			if !m.sleepCtx(ctx, backoff) {
				return
			}
			escalate()
			m.bumpReconnectCount(e)
			continue
		}

		readyCh := watchTunnelReady(stderr, cfg)

		// SSH process started — record PID, but keep the tunnel in connecting
		// state until ssh reports that the reverse forward is ready.
		now := time.Now()
		m.mu.Lock()
		e.cmd = cmd
		e.state.Status = StatusConnecting
		e.state.PID = cmd.Process.Pid
		e.state.StartedAt = &now
		e.state.LastError = ""
		m.mu.Unlock()
		_ = m.saveEntryState(e)

		waitErrCh := make(chan error, 1)
		go func() {
			waitErrCh <- waitAndCleanup(cmd.Wait, cleanupArgs)
		}()

		connected := false
		shouldReconnect := false
		for !connected {
			select {
			case ready := <-readyCh:
				if ready {
					m.updateState(e, StatusConnected, cmd.Process.Pid, "", false)
					// Mark when ssh confirmed the reverse forward is bound, but
					// defer resetting the backoff until the connection proves
					// stable — a tunnel that Start()s cleanly, logs "ready",
					// then dies seconds later would otherwise see backoff
					// reset and hammer the remote at the base interval.
					connectedAt = time.Now()
					log.Printf("tunnel-manager: %s connected (pid %d)", cfg.Host, cmd.Process.Pid)
				} else {
					// stderr closed before the reverse forward succeeded; ssh
					// is exiting but cmd.Wait() has not yet returned. Downgrade
					// the visible status so List() does not show "connecting",
					// but leave e.cmd intact so stopEntry can still reach the
					// live *exec.Cmd if the user cancels during this window.
					// updateState would nil e.cmd as a side effect of any
					// non-Connected transition; using the keep-cmd variant
					// closes the tiny orphan window where stopEntry would see
					// cmd==nil + pid>0 and fall into the stale-cleanup path
					// while Go still holds a pending Wait() for the same pid.
					m.setStateDisconnectedKeepCmd(e, cmd.Process.Pid, "ssh stderr closed before ready")
				}
				connected = true
			case waitErr := <-waitErrCh:
				if ctx.Err() != nil {
					m.updateState(e, StatusStopped, 0, "", false)
					return
				}
				errMsg := "ssh exited unexpectedly"
				if waitErr != nil {
					errMsg = waitErr.Error()
				}
				m.updateState(e, StatusDisconnected, 0, errMsg, true)
				log.Printf("tunnel-manager: %s disconnected (%s), reconnecting in %v", cfg.Host, errMsg, backoff)

				if !m.sleepCtx(ctx, backoff) {
					return
				}
				escalate()
				shouldReconnect = true
				connected = true
			}
		}
		if shouldReconnect {
			continue
		}

		// Wait for SSH to exit after the tunnel has become ready.
		waitErr := <-waitErrCh

		// If context cancelled, this was an intentional stop.
		if ctx.Err() != nil {
			m.updateState(e, StatusStopped, 0, "", false)
			return
		}

		// Unexpected exit — schedule reconnect.
		errMsg := "ssh exited unexpectedly"
		if waitErr != nil {
			errMsg = waitErr.Error()
		}
		m.updateState(e, StatusDisconnected, 0, errMsg, true)
		log.Printf("tunnel-manager: %s disconnected (%s), reconnecting in %v", cfg.Host, errMsg, backoff)

		if !m.sleepCtx(ctx, backoff) {
			return
		}
		escalate()
	}
}

// adoptedLoop watches an adopted tunnel's external ssh process. cfg and
// initialPID are snapshotted under m.mu before the goroutine starts;
// subsequent mutable reads from e.state take the lock. Once the first
// inspect confirms the pid still matches, subsequent polls back off
// from 1s to a slower cadence so we do not run ps/CIM once per second
// per tunnel indefinitely.
func (m *Manager) adoptedLoop(ctx context.Context, e *entry, cfg TunnelConfig, initialPID int) {
	handedOff := false
	defer func() {
		if !handedOff {
			close(e.done)
		}
	}()

	const (
		preConfirmInterval  = time.Second
		postConfirmInterval = 10 * time.Second
	)
	confirmed := false
	pid := initialPID

	for {
		if err := ctx.Err(); err != nil {
			return
		}

		// Refresh pid under lock in case updateState zeroed/changed it.
		m.mu.Lock()
		if e.state != nil {
			pid = e.state.PID
		}
		m.mu.Unlock()
		if pid <= 0 {
			// Lost the adopted PID (caller zeroed it, or external
			// reset). Hand off to reconnectLoop so the tunnel keeps
			// being supervised instead of being silently abandoned.
			handedOff = true
			m.reconnectLoop(ctx, e, cfg)
			return
		}

		matches, err := m.processMatchesPID(pid, cfg)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("tunnel-manager: failed to inspect adopted tunnel %s (pid %d): %v", cfg.Host, pid, err)
			if !m.sleepCtx(ctx, preConfirmInterval) {
				return
			}
			continue
		}
		if !matches {
			if ctx.Err() != nil {
				return
			}
			m.updateState(e, StatusDisconnected, 0, "ssh exited unexpectedly", true)
			handedOff = true
			m.reconnectLoop(ctx, e, cfg)
			return
		}

		interval := preConfirmInterval
		if confirmed {
			interval = postConfirmInterval
		} else {
			// First successful inspect — promote the adopted tunnel from
			// "connecting" (the placeholder LoadAndStartAll wrote) to
			// "connected" so List() reflects reality.
			m.updateState(e, StatusConnected, pid, "", false)
			confirmed = true
		}
		// Add ±20% jitter so hosts with many adopted tunnels don't all poll
		// on the same tick, which would produce a periodic spike of
		// ps/CIM calls proportional to tunnel count.
		if !m.sleepCtx(ctx, jitteredInterval(interval)) {
			return
		}
	}
}

// jitteredInterval returns d ± up-to-20%, rounded away from negative. Used to
// desynchronize periodic polls across many tunnels so a host running 50
// adopted tunnels does not issue 50 simultaneous ps/CIM subprocesses every
// postConfirmInterval tick.
func jitteredInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	// math/rand is fine: the purpose is stochastic dispersion, not security.
	spread := int64(d) / 5 // 20%
	if spread <= 0 {
		return d
	}
	tunnelJitterRandMu.Lock()
	delta := tunnelJitterRand.Int63n(2*spread+1) - spread
	tunnelJitterRandMu.Unlock()
	result := int64(d) + delta
	if result < 1 {
		result = 1
	}
	return time.Duration(result)
}

func watchTunnelReady(stderr io.Reader, cfg TunnelConfig) <-chan bool {
	readyCh := make(chan bool, 1)
	go func() {
		defer close(readyCh)
		scanner := bufio.NewScanner(stderr)
		// Raise the scanner's line cap from the default 64 KiB to 1 MiB.
		// A chatty `ssh -v` emits verbose diagnostic output that can
		// accumulate past 64 KiB if the user has heavy key/agent/KEX
		// logging; once Scan() errors out with bufio.ErrTooLong the
		// goroutine returns, stderr stops being drained, and the ssh
		// child eventually blocks on a full stderr pipe — which looks
		// externally like a hung tunnel. Even after the readiness
		// signal fires we keep draining so the pipe never fills.
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		readySent := false
		forwardSucceeded := false
		for scanner.Scan() {
			if readySent {
				continue
			}
			line := scanner.Text()
			if tunnelForwardSuccessLine(line, cfg) {
				forwardSucceeded = true
				readyCh <- true
				readySent = true
				continue
			}
			// The "entering interactive session" line is only a
			// meaningful readiness signal after ssh has already
			// reported success for *this* tunnel's remote forward.
			// Firing on it without a prior success would let an
			// unrelated tunnel session flip our state to connected.
			if forwardSucceeded && tunnelInteractiveFallbackLine(line) {
				readyCh <- true
				readySent = true
			}
		}
		// If Scan() exited because a single line exceeded the 1 MiB cap
		// (scanner.Err() == bufio.ErrTooLong), stop parsing but keep the
		// stderr pipe drained so the ssh child doesn't block on a full
		// pipe. Cap the total drained output at 16 MiB; past that we
		// simply close the pipe by letting this goroutine return, which
		// causes ssh's next write to fail with EPIPE and the cmd.Wait
		// path handles the exit. The previous code followed the bounded
		// io.Copy with an unbounded one, defeating the cap and letting a
		// pathologically chatty ssh keep this goroutine alive past
		// Shutdown()'s budget.
		if err := scanner.Err(); err != nil && err == bufio.ErrTooLong {
			_, _ = io.Copy(io.Discard, io.LimitReader(stderr, 16<<20))
		}
		if !readySent {
			readyCh <- false
		}
	}()
	return readyCh
}

// tunnelForwardSuccessLine matches ssh's "remote forward success" log
// line specifically for cfg.RemotePort. It requires a trailing
// delimiter after the port number so `listen 19001` does not spuriously
// match `listen 190019999`.
func tunnelForwardSuccessLine(line string, cfg TunnelConfig) bool {
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return false
	}
	if !strings.Contains(line, "remote forward success for:") {
		return false
	}
	needle := fmt.Sprintf("listen %d", cfg.RemotePort)
	idx := strings.Index(line, needle)
	if idx < 0 {
		return false
	}
	end := idx + len(needle)
	if end == len(line) {
		return true
	}
	next := line[end]
	switch next {
	case ' ', '\t', ',', '\n', '\r', ';', ')', ']':
		// Some OpenSSH builds emit the bracketed form
		// "[listen 19001, connect 127.0.0.1:18339]"; accept ']' so
		// readiness detection works against those builds too.
		return true
	}
	return false
}

func tunnelInteractiveFallbackLine(line string) bool {
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return false
	}
	return strings.Contains(line, "entering interactive session")
}

func waitAndCleanup(waitFn func() error, cleanup func()) error {
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()
	return waitFn()
}

// updateState updates the entry state under lock and persists.
func (m *Manager) updateState(e *entry, status Status, pid int, lastErr string, incrementReconnect bool) {
	m.mu.Lock()
	if incrementReconnect {
		e.state.ReconnectCount++
	}
	e.state.Status = status
	e.state.PID = pid
	e.state.LastError = lastErr
	if status != StatusConnected {
		e.cmd = nil
	}
	m.mu.Unlock()
	_ = m.saveEntryState(e)
}

// setStateDisconnectedKeepCmd marks the entry as disconnected but preserves
// e.cmd so a concurrent stopEntry can still signal the live *exec.Cmd via
// stopProcess instead of falling into the stale-pid cleanup path. Used for
// the narrow window between ssh's stderr closing and cmd.Wait() returning —
// the process is exiting but Go still holds a wait slot on its pid, so
// sending a signal to that pid is still safe and correctly targeted.
func (m *Manager) setStateDisconnectedKeepCmd(e *entry, pid int, lastErr string) {
	m.mu.Lock()
	e.state.Status = StatusDisconnected
	e.state.PID = pid
	e.state.LastError = lastErr
	m.mu.Unlock()
	_ = m.saveEntryState(e)
}

func (m *Manager) bumpReconnectCount(e *entry) {
	m.mu.Lock()
	e.state.ReconnectCount++
	m.mu.Unlock()
	_ = m.saveEntryState(e)
}

func (m *Manager) saveEntryState(e *entry) error {
	m.mu.Lock()
	st := cloneState(e.state)
	m.mu.Unlock()
	err := m.persistStateForDiskTracked(st, &e.persistWg)

	m.mu.Lock()
	if e.state != nil {
		if err != nil {
			e.state.PersistenceError = err.Error()
		} else {
			e.state.PersistenceError = ""
		}
	}
	m.mu.Unlock()

	if err != nil && st != nil {
		log.Printf("tunnel-manager: failed to persist state for %s on local port %d: %v", st.Config.Host, st.Config.LocalPort, err)
	}
	return err
}

func saveStateForDisk(stateDir string, st *TunnelState) error {
	cp := cloneState(st)
	if cp != nil {
		cp.PersistenceError = ""
	}
	return SaveState(stateDir, cp)
}

// stopEntry cancels the entry context, kills the SSH process, and waits
// for the supervising goroutine to close its done channel so the caller
// can safely delete the map entry and mutate persisted state without
// racing a still-live reconnect loop.
func (m *Manager) stopEntry(e *entry) error {
	cmd, pid, cfg, done := m.entryTargetSnapshot(e)
	e.cancel()

	gracePeriod := m.stopGraceDuration()
	killPeriod := m.stopKillDuration()

	waitDone := func(timeout time.Duration) error {
		if done == nil {
			return nil
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-done:
			return nil
		case <-timer.C:
			return fmt.Errorf("timed out waiting for tunnel %s goroutine to exit after %v", cfg.Host, timeout)
		}
	}

	if cmd == nil && pid > 0 {
		if err := m.runBoundedCleanup(pid, cfg); err != nil {
			return fmt.Errorf("stop pid %d for %s: %w", pid, cfg.Host, err)
		}
		return waitDone(gracePeriod)
	}

	cmd, done = m.waitForEntryCommand(e)
	if cmd == nil {
		// cmd was never published — the supervising goroutine saw the
		// cancel before it could Start() ssh. Still wait for done so
		// callers (Down/Up/Shutdown) observe goroutine exit before
		// mutating the map entry.
		return waitDone(gracePeriod)
	}

	if err := m.runBoundedStopProcess(cmd); err != nil {
		if cmd.Process == nil {
			return fmt.Errorf("stop tunnel %s: %w", cfg.Host, err)
		}
		pid := m.snapshotCmdPid(cmd)
		if cleanupErr := m.runBoundedCleanup(pid, cfg); cleanupErr != nil {
			return fmt.Errorf("stop tunnel %s: %v; cleanup pid %d: %w", cfg.Host, err, pid, cleanupErr)
		}
	}

	if err := waitDone(gracePeriod); err == nil {
		return nil
	}
	if cmd.Process != nil {
		pid := m.snapshotCmdPid(cmd)
		if err := forceKillTunnelCommand(cmd); err != nil {
			if !errors.Is(err, os.ErrProcessDone) {
				return fmt.Errorf("force kill tunnel %s (pid %d): %w", cfg.Host, pid, err)
			}
		}
	}
	if err := waitDone(killPeriod); err != nil {
		pid := 0
		if cmd.Process != nil {
			pid = m.snapshotCmdPid(cmd)
		}
		return fmt.Errorf("timed out waiting for tunnel %s (pid %d) to stop after kill", cfg.Host, pid)
	}
	return nil
}

// runBoundedStopProcess wraps m.stopProcess with a bounded wait. On Windows
// `stopTunnelProcess` shells out to `taskkill` synchronously; a hung taskkill
// (rare but real, e.g. on a suspended container) would pin opMu — which
// serializes every Up/Down/Remove — until it returned. On Unix the same
// family of wedges can affect the `ps` probe inside pidStillMatchesTunnelCmd.
// The goroutine spawned here leaks until the underlying subprocess returns,
// but we prefer a leaked goroutine to a locked-up manager; the daemon will
// restart on the next shutdown anyway.
func (m *Manager) runBoundedStopProcess(cmd *exec.Cmd) error {
	stopBudget := m.stopGraceDuration() + m.stopKillDuration()
	if stopBudget <= 0 {
		stopBudget = defaultStopGracePeriod + defaultStopKillPeriod
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- m.stopProcess(cmd)
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(stopBudget):
		return fmt.Errorf("stopProcess timed out after %v", stopBudget)
	}
}

// runBoundedCleanup wraps cleanupStaleProcess the same way as
// runBoundedStopProcess. cleanupStaleProcess already has internal SIGTERM
// (5s) and SIGKILL (2s) grace periods, but those bounds are only honored if
// the child ps call itself returns; a hung ps would block the whole
// cleanup. Cap the total wait so stopEntry can continue regardless.
func (m *Manager) runBoundedCleanup(pid int, cfg TunnelConfig) error {
	budget := m.stopGraceDuration() + m.stopKillDuration() + 2*time.Second
	if budget <= 0 {
		budget = defaultStopGracePeriod + defaultStopKillPeriod + 2*time.Second
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- m.cleanupStaleProcess(pid, cfg)
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(budget):
		return fmt.Errorf("cleanupStaleProcess for pid %d timed out after %v", pid, budget)
	}
}

// snapshotCmdPid reads cmd.Process.Pid under m.mu so that concurrent
// writes to the owning entry (which publishes e.cmd under the same lock)
// cannot surface a torn value. The Pid itself is immutable after
// cmd.Start(), but protecting the read removes a future footgun if the
// cmd handle is ever swapped while stopEntry is draining.
func (m *Manager) snapshotCmdPid(cmd *exec.Cmd) int {
	if cmd == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

func (m *Manager) entryTargetSnapshot(e *entry) (*exec.Cmd, int, TunnelConfig, chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()

	done := e.done
	if done == nil {
		done = make(chan struct{})
	}

	var (
		pid int
		cfg TunnelConfig
	)
	if e.state != nil {
		pid = e.state.PID
		cfg = e.state.Config
	}
	return e.cmd, pid, cfg, done
}

func (m *Manager) waitForEntryCommand(e *entry) (*exec.Cmd, chan struct{}) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		m.mu.Lock()
		cmd := e.cmd
		done := e.done
		m.mu.Unlock()

		if cmd != nil {
			return cmd, done
		}

		select {
		case <-done:
			return nil, done
		case <-ticker.C:
		}
	}
}

func tunnelProcessMatchesPID(pid int, cfg TunnelConfig) (bool, error) {
	exitedOrChanged, err := tunnelProcessExitedOrChangedWith(tunnelProcessCommandLine, pid, cfg)
	if err != nil {
		return false, err
	}
	return !exitedOrChanged, nil
}

// sleepCtx sleeps for d or until ctx is cancelled. Returns false if cancelled.
func (m *Manager) sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
