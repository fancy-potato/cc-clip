package peer

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

func PeerStateDir(baseDir, peerID string) string {
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
