package tunnel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shunmei/cc-clip/internal/fileutil"
)

// Status represents the current state of a tunnel.
type Status string

const (
	StatusConnected    Status = "connected"
	StatusConnecting   Status = "connecting"
	StatusDisconnected Status = "disconnected"
	StatusStopped      Status = "stopped"
)

// TunnelConfig is the persistent configuration for a tunnel.
type TunnelConfig struct {
	Host              string   `json:"host"`
	LocalPort         int      `json:"local_port"`
	RemotePort        int      `json:"remote_port"`
	Enabled           bool     `json:"enabled"`
	SSHOptions        []string `json:"ssh_options,omitempty"`
	SSHConfigResolved bool     `json:"ssh_config_resolved,omitempty"`
}

// TunnelState is the full runtime state of a tunnel (persisted to disk).
type TunnelState struct {
	Config           TunnelConfig `json:"config"`
	Status           Status       `json:"status"`
	PID              int          `json:"pid,omitempty"`
	StartedAt        *time.Time   `json:"started_at,omitempty"`
	StoppedAt        *time.Time   `json:"stopped_at,omitempty"`
	LastError        string       `json:"last_error,omitempty"`
	PersistenceError string       `json:"persistence_error,omitempty"`
	ReconnectCount   int          `json:"reconnect_count"`
}

const maxTunnelStatePort = 65535

// ErrAmbiguousTunnelState reports that multiple saved tunnel states exist for
// the same host and a host-only lookup cannot determine which one to use.
var ErrAmbiguousTunnelState = errors.New("ambiguous tunnel state")

// ErrInvalidTunnelState flags structural validation failures from
// validateTunnelState (missing host, bad port). Wrapping with this sentinel
// lets callers use errors.Is instead of brittle prefix matching like
// strings.HasPrefix(err.Error(), "invalid ") — which would silently
// mis-classify any future error message that happened to start with
// "invalid " (e.g., "invalid state file path").
var ErrInvalidTunnelState = errors.New("invalid tunnel state")

var hostSanitizer = regexp.MustCompile(`[^a-zA-Z0-9-]`)
var currentStateFileNamePattern = regexp.MustCompile(`^.+-[0-9]+-[0-9a-f]{16}\.json$`)

// SanitizeHost converts a hostname to a safe filename component.
func SanitizeHost(host string) string {
	s := hostSanitizer.ReplaceAllString(normalizeHost(host), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "unknown"
	}
	return s
}

// normalizeHost applies the same case/whitespace rules used by SanitizeHost
// so hash and map-key derivations stay consistent with the on-disk filename.
// Without this, "Example" and "example" would share a filename prefix but
// hash to different suffixes and produce two state files for one host.
func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSpace(host))
}

// StateFilePath returns the path to the state file for a host/local-port pair.
func StateFilePath(stateDir, host string, localPort int) string {
	stem := fmt.Sprintf("%s-%d-%s", SanitizeHost(host), localPort, shortStateHash(host, localPort))
	return filepath.Join(stateDir, stem+".json")
}

func shortStateHash(host string, localPort int) string {
	sum := sha256.Sum256([]byte(normalizeHost(host) + "\x00" + strconv.Itoa(localPort)))
	return hex.EncodeToString(sum[:8])
}

// DefaultStateDir returns the default directory for tunnel state files.
func DefaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "cc-clip", "tunnels")
	}
	return filepath.Join(home, ".cache", "cc-clip", "tunnels")
}

// LoadState reads tunnel state for a specific host/local-port pair.
func LoadState(stateDir, host string, localPort int) (*TunnelState, error) {
	path := StateFilePath(stateDir, host, localPort)
	s, err := loadStateFile(path)
	if err != nil {
		return nil, err
	}
	if normalizeHost(s.Config.Host) != normalizeHost(host) || s.Config.LocalPort != localPort {
		// A hash collision is cryptographically implausible, so a mismatch
		// almost certainly indicates a hand-edited or truncated file.
		// Compare hosts normalized (lowercase + trim) because the filename
		// derives from the normalized form.
		return nil, fmt.Errorf("state file %s contents (host=%q local_port=%d) do not match lookup (host=%q local_port=%d)", path, s.Config.Host, s.Config.LocalPort, host, localPort)
	}
	return s, nil
}

// LoadStateByHost reads the saved tunnel state for a host when there is only
// one saved local-port entry for that host.
func LoadStateByHost(stateDir, host string) (*TunnelState, error) {
	states, err := LoadStatesForHost(stateDir, host)
	if err != nil {
		return nil, err
	}
	switch len(states) {
	case 0:
		return nil, os.ErrNotExist
	case 1:
		return cloneStateForDisk(states[0]), nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrAmbiguousTunnelState, host)
	}
}

// LoadStatesForHost reads all saved tunnel states for a host across local ports.
func LoadStatesForHost(stateDir, host string) ([]*TunnelState, error) {
	states, err := LoadAllStates(stateDir)
	if err != nil {
		return nil, err
	}
	matches := make([]*TunnelState, 0, len(states))
	needle := normalizeHost(host)
	for _, s := range states {
		// Compare against the normalized form: the on-disk filename derives
		// from normalizeHost (lowercase + trim) while s.Config.Host preserves
		// whatever case the user originally passed. Lookups of "MyHost" would
		// otherwise silently miss a state file that was saved as "myhost".
		if s != nil && normalizeHost(s.Config.Host) == needle {
			matches = append(matches, cloneStateForDisk(s))
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Config.LocalPort < matches[j].Config.LocalPort
	})
	return matches, nil
}

// SaveState writes tunnel state to disk.
func SaveState(stateDir string, s *TunnelState) error {
	if err := validateTunnelStateForSave(s); err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := StateFilePath(stateDir, s.Config.Host, s.Config.LocalPort)
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return err
	}
	return nil
}

// LoadAllStates reads all tunnel states from the state directory.
// Files whose names do not match the deterministic <host>-<port>-<hash>.json
// form are skipped: they are either stale legacy names that pre-date the
// feature (none exist in practice; the feature is new) or someone copied
// a state file from a different host.
func LoadAllStates(stateDir string) ([]*TunnelState, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	statesByKey := make(map[string]*TunnelState)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if !isCurrentStateFileName(e.Name()) {
			continue
		}
		s, err := loadStateFile(filepath.Join(stateDir, e.Name()))
		if err != nil {
			log.Printf("tunnel-state: skipping corrupt state file %s: %v", e.Name(), err)
			continue
		}
		if !stateFilenameMatchesContents(e.Name(), s) {
			log.Printf("tunnel-state: skipping state file %s: filename hash does not match contents (host=%q local_port=%d)", e.Name(), s.Config.Host, s.Config.LocalPort)
			continue
		}
		statesByKey[stateKey(s.Config.Host, s.Config.LocalPort)] = cloneStateForDisk(s)
	}
	states := make([]*TunnelState, 0, len(statesByKey))
	for _, s := range statesByKey {
		states = append(states, s)
	}
	sort.Slice(states, func(i, j int) bool {
		if states[i].Config.Host == states[j].Config.Host {
			return states[i].Config.LocalPort < states[j].Config.LocalPort
		}
		return states[i].Config.Host < states[j].Config.Host
	})
	return states, nil
}

func isCurrentStateFileName(name string) bool {
	return currentStateFileNamePattern.MatchString(name)
}

// stateFilenameMatchesContents returns true when `name` is the expected
// current-format filename for s.Config.(Host, LocalPort). A mismatch means
// an attacker (or a careless operator) may have moved or tampered with the
// file; the caller should refuse to load it rather than trust the contents.
//
// Important: this only validates the {host, local_port} identity pair —
// the mutable body (PID, Status, LastError, ReconnectCount, RemotePort) is
// NOT covered. A hand-edited file that keeps the original (host, local_port)
// can change every other field undetectably. validateTunnelState catches
// gross corruption (invalid host, out-of-range ports), but not subtle
// mutations like a stale PID or a fabricated remote port. Treat the on-disk
// state as best-effort recovery context, not as authoritative configuration.
func stateFilenameMatchesContents(name string, s *TunnelState) bool {
	if s == nil {
		return false
	}
	expected := filepath.Base(StateFilePath("", s.Config.Host, s.Config.LocalPort))
	return name == expected
}

// RemoveState deletes the state file for a host/local-port pair.
func RemoveState(stateDir, host string, localPort int) error {
	path := StateFilePath(stateDir, host, localPort)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func loadStateFile(path string) (*TunnelState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s TunnelState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse tunnel state: %w", err)
	}
	if err := validateTunnelStateForLoad(&s); err != nil {
		return nil, fmt.Errorf("parse tunnel state: %w", err)
	}
	return &s, nil
}

func validateTunnelStateForLoad(s *TunnelState) error {
	return validateTunnelState(s, false)
}

func validateTunnelStateForSave(s *TunnelState) error {
	return validateTunnelState(s, true)
}

func validateTunnelState(s *TunnelState, requireLocalPort bool) error {
	if s == nil {
		return fmt.Errorf("%w: missing tunnel state", ErrInvalidTunnelState)
	}
	if strings.TrimSpace(s.Config.Host) == "" {
		return fmt.Errorf("%w: missing host", ErrInvalidTunnelState)
	}
	// Re-validate the host against the same rules applied before spawning
	// ssh. Without this, a hand-edited or legacy state file could surface
	// a bogus host value (e.g. starts with `-`, contains whitespace) in
	// List() output and SwiftBar status even though sshTunnelArgs would
	// refuse to honor it.
	if err := ValidateSSHHost(s.Config.Host); err != nil {
		return fmt.Errorf("%w: invalid ssh host: %w", ErrInvalidTunnelState, err)
	}
	if s.Config.LocalPort < 0 || s.Config.LocalPort > maxTunnelStatePort {
		return fmt.Errorf("%w: invalid local port %d", ErrInvalidTunnelState, s.Config.LocalPort)
	}
	if requireLocalPort && s.Config.LocalPort == 0 {
		return fmt.Errorf("%w: invalid local port %d", ErrInvalidTunnelState, s.Config.LocalPort)
	}
	if s.Config.RemotePort < 1 || s.Config.RemotePort > maxTunnelStatePort {
		return fmt.Errorf("%w: invalid remote port %d", ErrInvalidTunnelState, s.Config.RemotePort)
	}
	if len(s.Config.SSHOptions) > 0 && !s.Config.SSHConfigResolved {
		s.Config.SSHConfigResolved = true
	}
	if s.Config.SSHConfigResolved {
		if err := validateResolvedTunnelOptions(s.Config.SSHOptions); err != nil {
			return fmt.Errorf("%w: invalid ssh options: %v", ErrInvalidTunnelState, err)
		}
	}
	return nil
}

func validateResolvedTunnelOptions(opts []string) error {
	for _, opt := range opts {
		key, value, ok := strings.Cut(opt, "=")
		if !ok || key == "" || value == "" {
			return fmt.Errorf("ssh option %q must be key=value", opt)
		}
		if !isSafeSSHOptionKey(key) {
			return fmt.Errorf("ssh option key %q is invalid", key)
		}
		if _, excluded := excludedTunnelSSHOptions[strings.ToLower(key)]; excluded {
			return fmt.Errorf("ssh option %q is not allowed in persistent tunnels", key)
		}
		if strings.EqualFold(key, "host") {
			return fmt.Errorf("ssh option %q is not allowed in persistent tunnels", key)
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("ssh option %q contains control characters", key)
		}
	}
	return nil
}

func stateKey(host string, localPort int) string {
	return normalizeHost(host) + "\x00" + strconv.Itoa(localPort)
}

func cloneStateForDisk(s *TunnelState) *TunnelState {
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
	// Deep-copy SSHOptions so a caller that mutates the returned slice
	// (future element-wise edit, append that stays below capacity, or a
	// test that patches a single option) cannot race with the live entry
	// still holding the original backing array. Current callers replace
	// SSHOptions wholesale, so this is defensive against future edits.
	if len(s.Config.SSHOptions) > 0 {
		cp.Config.SSHOptions = append([]string(nil), s.Config.SSHOptions...)
	}
	return &cp
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = os.Remove(tmpPath)
	}

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := fileutil.RenameReplace(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	// fsync the parent directory so the rename is durable across crashes.
	// Best-effort on platforms where opening a directory is not supported.
	if dirFile, dirErr := os.Open(dir); dirErr == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}
