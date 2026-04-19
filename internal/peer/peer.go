package peer

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultRangeStart = 18339
	DefaultRangeEnd   = 18439
)

var labelSanitizer = regexp.MustCompile(`[^a-z0-9]+`)
var peerIDValidator = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".cache", "cc-clip")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// PeerStateDir returns the per-peer state directory under baseDir. It refuses
// to compose a path when peerID fails ValidateID so a bad id cannot climb out
// of ~/.cache/cc-clip/peers via string concatenation on paths that bypass
// filepath.Clean. Callers are expected to validate the id beforehand (the
// registry entry points do); the empty-string return here is a last-resort
// guard so a future caller that forgets the validation can't silently escape.
func PeerStateDir(baseDir, peerID string) string {
	if ValidateID(peerID) != nil {
		return ""
	}
	return filepath.Join(baseDir, "peers", peerID)
}

func AliasForHost(host, label string) string {
	return fmt.Sprintf("%s-cc-clip-%s", aliasBaseHost(host), sanitizeLabel(label))
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
	baseDir, err := BaseDir()
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

func RFC3339Now() string {
	return time.Now().UTC().Format(time.RFC3339)
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
