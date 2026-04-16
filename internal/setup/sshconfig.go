package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SSHConfigChange describes a modification made to ~/.ssh/config.
type SSHConfigChange struct {
	Action string // "created", "added", "ok"
	Detail string
}

type ManagedHostSpec struct {
	Host       string
	RemotePort int
	LocalPort  int
}

// EnsureSSHConfig ensures ~/.ssh/config has required directives for cc-clip:
//   - RemoteForward <port> 127.0.0.1:<port>
//   - ControlMaster no
//   - ControlPath none
//
// If the host block doesn't exist, it is created before "Host *".
// A backup is written to ~/.ssh/config.cc-clip-backup before any modification.
func EnsureSSHConfig(host string, port int) ([]SSHConfigChange, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return nil, fmt.Errorf("cannot create ~/.ssh: %w", err)
	}
	return ensureSSHConfigAt(filepath.Join(sshDir, "config"), host, port)
}

func EnsureManagedHostConfig(spec ManagedHostSpec) ([]SSHConfigChange, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return nil, fmt.Errorf("cannot create ~/.ssh: %w", err)
	}
	return ensureManagedHostConfigAt(filepath.Join(sshDir, "config"), spec)
}

func RemoveManagedHostConfig(host string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}
	return removeManagedHostConfigAt(filepath.Join(home, ".ssh", "config"), host)
}

func ensureSSHConfigAt(configPath string, host string, port int) ([]SSHConfigChange, error) {
	content, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot read %s: %w", configPath, err)
	}

	lines := strings.Split(string(content), "\n")
	block := findHostBlock(lines, host)
	rfValue := fmt.Sprintf("%d 127.0.0.1:%d", port, port)
	var changes []SSHConfigChange
	modified := false

	if block == nil {
		newBlock := []string{
			fmt.Sprintf("Host %s", host),
			fmt.Sprintf("    RemoteForward %s", rfValue),
			"    ControlMaster no",
			"    ControlPath none",
			"",
		}
		starLine := findHostStarLine(lines)
		if starLine >= 0 {
			result := make([]string, 0, len(lines)+len(newBlock))
			result = append(result, lines[:starLine]...)
			result = append(result, newBlock...)
			result = append(result, lines[starLine:]...)
			lines = result
		} else {
			if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
				lines = append(lines, "")
			}
			lines = append(lines, newBlock...)
		}
		changes = append(changes, SSHConfigChange{"created", fmt.Sprintf("Host %s (RemoteForward, ControlMaster no, ControlPath none)", host)})
		modified = true
	} else {
		type required struct {
			key      string
			value    string
			contains string
		}
		directives := []required{
			{"RemoteForward", rfValue, fmt.Sprintf("%d", port)},
			{"ControlMaster", "no", "no"},
			{"ControlPath", "none", "none"},
		}
		for _, d := range directives {
			if block.hasDirective(strings.ToLower(d.key), d.contains) {
				changes = append(changes, SSHConfigChange{"ok", fmt.Sprintf("%s %s", d.key, d.value)})
			} else {
				line := fmt.Sprintf("    %s %s", d.key, d.value)
				lines = insertDirectiveInBlock(lines, block, line)
				block.endLine++
				changes = append(changes, SSHConfigChange{"added", fmt.Sprintf("%s %s", d.key, d.value)})
				modified = true
			}
		}
	}

	if modified {
		if len(content) > 0 {
			backupPath := configPath + ".cc-clip-backup"
			_ = os.WriteFile(backupPath, content, 0644)
		}
		newContent := strings.Join(lines, "\n")
		if err := os.WriteFile(configPath, []byte(newContent), 0644); err != nil {
			return nil, fmt.Errorf("cannot write %s: %w", configPath, err)
		}
	}

	return changes, nil
}

func ensureManagedHostConfigAt(configPath string, spec ManagedHostSpec) ([]SSHConfigChange, error) {
	content, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot read %s: %w", configPath, err)
	}

	lines := strings.Split(string(content), "\n")
	block := findHostBlock(lines, spec.Host)
	if block == nil {
		return nil, fmt.Errorf("ssh config missing exact Host %s block; define an explicit alias in ~/.ssh/config first", spec.Host)
	}

	startMarker, endMarker := managedHostMarkers(spec.Host)
	start, end, found := findManagedRangeInBlock(lines, block, startMarker, endMarker)
	if err := validateManagedHostConflicts(lines, block, spec, start, end, found); err != nil {
		return nil, err
	}

	fragment := managedHostFragment(spec)
	var updated []string
	var change SSHConfigChange
	switch {
	case found:
		updated = replaceManagedRange(lines, start, end, fragment)
		change = SSHConfigChange{Action: "updated", Detail: fmt.Sprintf("managed Host %s", spec.Host)}
	default:
		insertAt := managedHostInsertLine(lines, block)
		updated = insertLinesAt(lines, insertAt, fragment)
		change = SSHConfigChange{Action: "created", Detail: fmt.Sprintf("managed Host %s", spec.Host)}
	}

	if strings.Join(updated, "\n") == string(content) {
		return []SSHConfigChange{{Action: "ok", Detail: fmt.Sprintf("managed Host %s", spec.Host)}}, nil
	}

	if len(content) > 0 {
		backupPath := configPath + ".cc-clip-backup"
		_ = os.WriteFile(backupPath, content, 0600)
	}
	if err := os.WriteFile(configPath, []byte(strings.Join(updated, "\n")), 0644); err != nil {
		return nil, fmt.Errorf("cannot write %s: %w", configPath, err)
	}

	return []SSHConfigChange{change}, nil
}

func removeManagedHostConfigAt(configPath, host string) error {
	content, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot read %s: %w", configPath, err)
	}
	lines := strings.Split(string(content), "\n")
	block := findHostBlock(lines, host)
	if block == nil {
		return nil
	}
	startMarker, endMarker := managedHostMarkers(host)
	start, end, found := findManagedRangeInBlock(lines, block, startMarker, endMarker)
	if !found {
		return nil
	}
	result := replaceManagedRange(lines, start, end, nil)
	return os.WriteFile(configPath, []byte(strings.Join(result, "\n")), 0644)
}

type sshBlock struct {
	startLine  int
	endLine    int
	directives []sshDirective
}

type sshDirective struct {
	key   string // lowercase
	value string
}

func (b *sshBlock) hasDirective(key, valueSubstr string) bool {
	for _, d := range b.directives {
		if d.key == key && strings.Contains(strings.ToLower(d.value), strings.ToLower(valueSubstr)) {
			return true
		}
	}
	return false
}

func findHostBlock(lines []string, host string) *sshBlock {
	var block *sshBlock
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if matchesHost(trimmed, host) {
			block = &sshBlock{startLine: i}
			continue
		}
		if block != nil && isAnyHostLine(trimmed) {
			block.endLine = i
			return block
		}
		if block != nil {
			key, val := parseSSHDirective(trimmed)
			if key != "" {
				block.directives = append(block.directives, sshDirective{
					key:   strings.ToLower(key),
					value: val,
				})
			}
		}
	}
	if block != nil {
		block.endLine = len(lines)
	}
	return block
}

func managedHostFragment(spec ManagedHostSpec) []string {
	startMarker, endMarker := managedHostMarkers(spec.Host)
	return []string{
		"    " + startMarker,
		fmt.Sprintf("    RemoteForward %d 127.0.0.1:%d", spec.RemotePort, spec.LocalPort),
		"    ControlMaster no",
		"    ControlPath none",
		"    " + endMarker,
	}
}

func remoteForwardUsesListenPort(value string, listenPort int) bool {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return false
	}
	got, ok := parseRemoteForwardListenPort(fields[0])
	return ok && got == listenPort
}

func parseRemoteForwardListenPort(token string) (int, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(token); err == nil {
		return n, true
	}
	if i := strings.LastIndex(token, ":"); i >= 0 && i < len(token)-1 {
		if n, err := strconv.Atoi(token[i+1:]); err == nil {
			return n, true
		}
	}
	return 0, false
}

func displayDirectiveKey(key string) string {
	switch key {
	case "batchmode":
		return "BatchMode"
	case "certificatefile":
		return "CertificateFile"
	case "globalknownhostsfile":
		return "GlobalKnownHostsFile"
	case "hostkeyalias":
		return "HostKeyAlias"
	case "user":
		return "User"
	case "port":
		return "Port"
	case "hostname":
		return "HostName"
	case "identityagent":
		return "IdentityAgent"
	case "identitiesonly":
		return "IdentitiesOnly"
	case "identityfile":
		return "IdentityFile"
	case "kbdinteractiveauthentication":
		return "KbdInteractiveAuthentication"
	case "passwordauthentication":
		return "PasswordAuthentication"
	case "preferredauthentications":
		return "PreferredAuthentications"
	case "proxycommand":
		return "ProxyCommand"
	case "proxyjump":
		return "ProxyJump"
	case "pubkeyauthentication":
		return "PubkeyAuthentication"
	case "forwardagent":
		return "ForwardAgent"
	case "serveraliveinterval":
		return "ServerAliveInterval"
	case "stricthostkeychecking":
		return "StrictHostKeyChecking"
	case "localforward":
		return "LocalForward"
	case "remoteforward":
		return "RemoteForward"
	case "remotecommand":
		return "RemoteCommand"
	case "requesttty":
		return "RequestTTY"
	case "controlmaster":
		return "ControlMaster"
	case "controlpath":
		return "ControlPath"
	case "userknownhostsfile":
		return "UserKnownHostsFile"
	default:
		if key == "" {
			return ""
		}
		return strings.ToUpper(key[:1]) + key[1:]
	}
}

func managedHostMarkers(host string) (string, string) {
	start := fmt.Sprintf("# >>> cc-clip managed host: %s >>>", host)
	end := fmt.Sprintf("# <<< cc-clip managed host: %s <<<", host)
	return start, end
}

func findManagedRangeInBlock(lines []string, block *sshBlock, startMarker, endMarker string) (int, int, bool) {
	start := -1
	for i := block.startLine + 1; i < block.endLine; i++ {
		if strings.TrimSpace(lines[i]) == startMarker {
			start = i
			continue
		}
		if start >= 0 && strings.TrimSpace(lines[i]) == endMarker {
			return start, i + 1, true
		}
	}
	return 0, 0, false
}

func insertLinesAt(lines []string, insertAt int, extra []string) []string {
	result := make([]string, 0, len(lines)+len(extra))
	result = append(result, lines[:insertAt]...)
	result = append(result, extra...)
	result = append(result, lines[insertAt:]...)
	return result
}

func replaceManagedRange(lines []string, start, end int, replacement []string) []string {
	result := make([]string, 0, len(lines)-end+start+len(replacement))
	result = append(result, lines[:start]...)
	result = append(result, replacement...)
	result = append(result, lines[end:]...)
	return result
}

func managedHostInsertLine(lines []string, block *sshBlock) int {
	insertAt := block.endLine
	for insertAt > block.startLine+1 && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}
	return insertAt
}

func validateManagedHostConflicts(lines []string, block *sshBlock, spec ManagedHostSpec, managedStart, managedEnd int, hasManaged bool) error {
	for i := block.startLine + 1; i < block.endLine; i++ {
		if hasManaged && i >= managedStart && i < managedEnd {
			continue
		}
		key, value := parseSSHDirective(lines[i])
		if key == "" {
			continue
		}
		lowerKey := strings.ToLower(key)
		switch lowerKey {
		case "controlmaster", "controlpath":
			return fmt.Errorf("refusing to manage Host %s: existing %s directive at line %d conflicts with cc-clip-managed SSH behavior", spec.Host, displayDirectiveKey(lowerKey), i+1)
		case "remoteforward":
			if remoteForwardUsesListenPort(value, spec.RemotePort) {
				return fmt.Errorf("refusing to manage Host %s: existing RemoteForward on port %d at line %d conflicts with cc-clip-managed SSH behavior", spec.Host, spec.RemotePort, i+1)
			}
		}
	}
	return nil
}

func matchesHost(trimmed, host string) bool {
	if !isAnyHostLine(trimmed) {
		return false
	}
	for _, f := range strings.Fields(trimmed)[1:] {
		if f == host {
			return true
		}
	}
	return false
}

func isAnyHostLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "Host ") || strings.HasPrefix(trimmed, "Host\t")
}

func findHostStarLine(lines []string) int {
	for i, line := range lines {
		if matchesHost(strings.TrimSpace(line), "*") {
			return i
		}
	}
	return -1
}

func parseSSHDirective(line string) (string, string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", ""
	}
	// Handle both "Key Value" and "Key=Value"
	trimmed = strings.Replace(trimmed, "=", " ", 1)
	parts := strings.SplitN(trimmed, " ", 2)
	if len(parts) < 2 {
		return parts[0], ""
	}
	return parts[0], strings.TrimSpace(parts[1])
}

func insertDirectiveInBlock(lines []string, block *sshBlock, directive string) []string {
	insertAt := block.endLine
	for insertAt > block.startLine+1 && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}
	result := make([]string, 0, len(lines)+1)
	result = append(result, lines[:insertAt]...)
	result = append(result, directive)
	result = append(result, lines[insertAt:]...)
	return result
}
