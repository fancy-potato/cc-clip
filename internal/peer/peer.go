package peer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/shunmei/cc-clip/internal/userhome"
)

const (
	DefaultRangeStart = 18339
	DefaultRangeEnd   = 18439
)

var labelSanitizer = regexp.MustCompile(`[^a-z0-9]+`)
var peerIDValidator = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// loadLocalIdentityLabelLogOnce gates the stderr warning emitted when
// LoadLocalIdentity hits a hard I/O error reading local-peer-label.
// Callers that invoke LoadLocalIdentity in a loop (status refresh, tunnel
// inspector polls, etc.) would otherwise spam a well-known misconfig
// once per call — once per process is the right frequency for operators.
var loadLocalIdentityLabelLogOnce sync.Once

// ErrLocalIdentityNotFound reports that the local peer identity files are
// missing or incomplete. Bare `cc-clip uninstall --peer` uses this sentinel
// to fail closed instead of minting a fresh peer ID and orphaning the real
// remote reservation.
var ErrLocalIdentityNotFound = errors.New("local peer identity not found")

type Identity struct {
	ID    string
	Label string
}

type Registration struct {
	PeerID       string `json:"peer_id"`
	Label        string `json:"label"`
	ReservedPort int    `json:"reserved_port"`
	StateDir     string `json:"state_dir"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	LastConnect  string `json:"last_connect_at"`
}

type State struct {
	PeerID       string `json:"peer_id"`
	Label        string `json:"label"`
	ReservedPort int    `json:"reserved_port"`
	StateDir     string `json:"state_dir"`
	UpdatedAt    string `json:"updated_at"`
}

func BaseDir() (string, error) {
	dir, err := baseDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// BaseDirPath returns the cc-clip cache path without creating it. Read-only
// callers should prefer this over BaseDir so status/list probes do not
// materialize ~/.cache/cc-clip as a side effect.
func BaseDirPath() (string, error) {
	return baseDirPath()
}

// baseDirPath returns the cc-clip cache path without creating the directory.
// LoadLocalIdentity (and any other strictly read-only caller) goes through
// this helper so asking "what is the local peer id?" does not have the side
// effect of materializing ~/.cache/cc-clip — which matters for the bare
// `cc-clip uninstall --peer` fail-closed contract: if the cache dir is
// already gone, probing for the identity must fail with
// ErrLocalIdentityNotFound rather than silently recreate the directory.
func baseDirPath() (string, error) {
	home, err := userhome.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "cc-clip"), nil
}

// PeerStateDir returns the per-peer state directory under baseDir. It refuses
// to compose a path when peerID fails ValidateID so a bad id cannot climb out
// of ~/.cache/cc-clip/peers via string concatenation on paths that bypass
// filepath.Clean. Callers are expected to validate the id beforehand (the
// registry entry points do); the explicit error here surfaces the misuse so a
// future caller that forgets the validation cannot silently compose
// ~/.cache/cc-clip/peers/ (which filepath.Join would happily turn into the
// shared peers root) and clobber another peer's state.
func PeerStateDir(baseDir, peerID string) (string, error) {
	if err := ValidateID(peerID); err != nil {
		return "", fmt.Errorf("PeerStateDir: %w", err)
	}
	return filepath.Join(baseDir, "peers", peerID), nil
}

// AliasForHost returns the deterministic local alias cc-clip uses for a
// (host, label) pair. Distinct `host` inputs that sanitize to the same
// label base (`user@box` vs `user-box`) would collide on the sanitized
// form alone, so we fold a short SHA-256 fingerprint of the ORIGINAL
// host into the alias. The fingerprint is 16 hex chars (64 bits) —
// previously 8 hex chars (32 bits) but widened because birthday-
// collision odds at 32 bits become non-negligible once a user accrues
// hundreds of remote hosts across their career (p ~ 10^-4 at 10k hosts).
// The alias is per-laptop local state, not a security boundary — the
// goal is avoiding accidental cross-routing, not defending against
// deliberate preimage attacks. An empty host gets an empty fingerprint,
// preserving the legacy "peer" fallback for tests.
func AliasForHost(host, label string) string {
	trimmed := strings.TrimSpace(host)
	base := aliasBaseHost(trimmed)
	fingerprint := hostFingerprint(trimmed)
	if fingerprint != "" {
		base = base + "-" + fingerprint
	}
	return fmt.Sprintf("%s-cc-clip-%s", base, sanitizeLabel(label))
}

func hostFingerprint(host string) string {
	if host == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(host))
	return hex.EncodeToString(sum[:8])
}

func aliasBaseHost(host string) string {
	return sanitizeLabel(strings.TrimSpace(host))
}

func sanitizeLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = labelSanitizer.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "peer"
	}
	return s
}

func LoadOrCreateLocalIdentity() (Identity, error) {
	baseDir, err := BaseDir()
	if err != nil {
		return Identity{}, err
	}

	idPath := filepath.Join(baseDir, "local-peer-id")
	labelPath := filepath.Join(baseDir, "local-peer-label")

	id, idErr := readTrimmedFile(idPath)
	if idErr != nil || id == "" {
		id, err = randomHex(16)
		if err != nil {
			return Identity{}, err
		}
		if err := os.WriteFile(idPath, []byte(id+"\n"), 0600); err != nil {
			return Identity{}, err
		}
	}

	label, labelErr := readTrimmedFile(labelPath)
	if labelErr != nil || label == "" {
		host, hostErr := os.Hostname()
		if hostErr != nil || strings.TrimSpace(host) == "" {
			host = "peer"
		}
		label = sanitizeLabel(host)
		if err := os.WriteFile(labelPath, []byte(label+"\n"), 0600); err != nil {
			return Identity{}, err
		}
	}

	return Identity{
		ID:    strings.TrimSpace(id),
		Label: sanitizeLabel(label),
	}, nil
}

// LoadLocalIdentity returns the existing local peer identity without creating
// or rewriting any files. It requires the peer ID to be present; the label is
// best-effort because only the ID is needed for self-targeted uninstall.
func LoadLocalIdentity() (Identity, error) {
	baseDir, err := baseDirPath()
	if err != nil {
		return Identity{}, err
	}

	idPath := filepath.Join(baseDir, "local-peer-id")
	id, err := readTrimmedFile(idPath)
	switch {
	case err == nil && id != "":
		// proceed
	case errors.Is(err, os.ErrNotExist):
		return Identity{}, fmt.Errorf("%w: %s", ErrLocalIdentityNotFound, idPath)
	case err == nil && id == "":
		return Identity{}, fmt.Errorf("%w: %s", ErrLocalIdentityNotFound, idPath)
	default:
		return Identity{}, err
	}

	labelPath := filepath.Join(baseDir, "local-peer-label")
	label, err := readTrimmedFile(labelPath)
	switch {
	case err == nil && label != "":
		// keep the saved label
	case errors.Is(err, os.ErrNotExist):
		label = ""
	case err == nil && label == "":
		label = ""
	default:
		// TODO: consider surfacing this via a package-level logger once
		// one exists. A hard I/O error on local-peer-label (e.g. the
		// file has been replaced by a directory) is intentionally NOT
		// fatal here — the self-targeted uninstall path only needs the
		// ID — but a silent swallow hides the misconfiguration from
		// operators. Pinned by TestLoadLocalIdentitySwallowsLabelReadErrors.
		// Wrapped in sync.Once: callers that invoke LoadLocalIdentity
		// in a loop (e.g. status refresh) would otherwise spam this line
		// on every poll. Once per process is enough to surface the
		// misconfig without drowning the operator's terminal.
		loadLocalIdentityLabelLogOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "cc-clip: local-peer-label read error (non-fatal, logged once): %v\n", err)
		})
		label = ""
	}

	return Identity{
		ID:    strings.TrimSpace(id),
		Label: sanitizeLabel(label),
	}, nil
}

// ValidateID rejects peer IDs that are unsafe to embed in paths or shell
// commands. Generated local IDs are hex; explicit operator-supplied IDs only
// need a conservative ASCII token grammar.
//
// A `..` substring is also rejected so that an ID concatenated into
// ~/.cache/cc-clip/peers/<id> cannot climb out of the peers directory even
// though filepath.Clean would normally defeat that — belt-and-braces given
// the id can also flow through string concatenation (not only filepath.Join)
// on some code paths.
func ValidateID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("peer id is empty")
	}
	if !peerIDValidator.MatchString(id) {
		return fmt.Errorf("peer id %q contains unsupported characters", id)
	}
	if strings.Contains(id, "..") {
		return fmt.Errorf("peer id %q contains `..` sequence", id)
	}
	return nil
}

func WritePeerState(stateDir string, reg Registration) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	state := State{
		PeerID:       reg.PeerID,
		Label:        reg.Label,
		ReservedPort: reg.ReservedPort,
		StateDir:     reg.StateDir,
		UpdatedAt:    reg.UpdatedAt,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, "state.json"), append(data, '\n'), 0600)
}

// RFC3339Now returns the current UTC time formatted with nanosecond
// precision. Unified on time.RFC3339Nano so the registry entries
// (CreatedAt/UpdatedAt/LastConnect) and the lock-holder claim-time all
// share a single format — simplifies diffing state files and avoids
// accidental precision mismatches between equal-looking timestamps that
// round differently.
func RFC3339Now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func readTrimmedFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
