package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shunmei/cc-clip/internal/peer"
)

// SSHConfigChange describes a modification made to ~/.ssh/config.
type SSHConfigChange struct {
	Action string // "created", "added", "ok"
	Detail string
}

type ManagedAliasSpec struct {
	BaseHost   string
	Alias      string
	RemotePort int
	LocalPort  int
	PeerID     string
	PeerLabel  string
}

const (
	managedMarkerPrefix = "# >>> cc-clip managed:"
	managedMarkerSuffix = ">>>"
)

var resolveManagedAliasDirectives = resolveManagedAliasDirectivesWithSSH

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

func EnsureManagedAliasConfig(spec ManagedAliasSpec) ([]SSHConfigChange, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return nil, fmt.Errorf("cannot create ~/.ssh: %w", err)
	}
	return ensureManagedAliasConfigAt(filepath.Join(sshDir, "config"), spec)
}

func RemoveManagedAliasConfig(alias string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}
	return removeManagedAliasConfigAt(filepath.Join(home, ".ssh", "config"), alias)
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

func ensureManagedAliasConfigAt(configPath string, spec ManagedAliasSpec) ([]SSHConfigChange, error) {
	content, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot read %s: %w", configPath, err)
	}

	lines := strings.Split(string(content), "\n")
	aliasBlock := managedAliasBlock(lines, configPath, spec)
	startMarker, endMarker := managedAliasMarkers(spec.Alias)

	var (
		baseLines []string
		changes   []SSHConfigChange
	)

	if start, end, found := findManagedAliasRange(lines, startMarker, endMarker); found {
		if hostLine, ok := findHostLineOutsideRange(lines, spec.Alias, start, end); ok {
			return nil, fmt.Errorf("refusing to manage alias %s: existing unmanaged Host entry at line %d", spec.Alias, hostLine+1)
		}
		baseLines = append([]string{}, lines[:start]...)
		baseLines = append(baseLines, lines[end:]...)
		changes = append(changes, SSHConfigChange{Action: "updated", Detail: fmt.Sprintf("managed alias %s", spec.Alias)})
	} else {
		if hostLine, ok := findHostLineOutsideRange(lines, spec.Alias, -1, -1); ok {
			return nil, fmt.Errorf("refusing to manage alias %s: existing unmanaged Host entry at line %d", spec.Alias, hostLine+1)
		}
		baseLines = append([]string{}, lines...)
		changes = append(changes, SSHConfigChange{Action: "created", Detail: fmt.Sprintf("managed alias %s", spec.Alias)})
	}

	lines = insertManagedAliasBlock(baseLines, aliasBlock, findManagedAliasInsertLine(baseLines, spec.Alias))
	if strings.Join(lines, "\n") == string(content) {
		return []SSHConfigChange{{Action: "ok", Detail: fmt.Sprintf("managed alias %s", spec.Alias)}}, nil
	}

	if len(content) > 0 {
		backupPath := configPath + ".cc-clip-backup"
		_ = os.WriteFile(backupPath, content, 0600)
	}
	if err := os.WriteFile(configPath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return nil, fmt.Errorf("cannot write %s: %w", configPath, err)
	}

	return changes, nil
}

func removeManagedAliasConfigAt(configPath, alias string) error {
	content, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot read %s: %w", configPath, err)
	}
	lines := strings.Split(string(content), "\n")
	startMarker, endMarker := managedAliasMarkers(alias)
	start, end, found := findManagedAliasRange(lines, startMarker, endMarker)
	if !found {
		return nil
	}
	result := append([]string{}, lines[:start]...)
	result = append(result, lines[end:]...)
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

func managedAliasBlock(lines []string, configPath string, spec ManagedAliasSpec) []string {
	startMarker, endMarker := managedAliasMarkers(spec.Alias)
	block := []string{startMarker}
	block = append(block, fmt.Sprintf("Host %s", spec.Alias))

	directives := copyableDirectives(resolveAliasDirectives(configPath, lines, spec.BaseHost), spec)
	if !hasDirectiveKey(directives, "hostname") {
		_, host := splitSSHDestination(spec.BaseHost)
		block = append(block, fmt.Sprintf("    HostName %s", host))
	}
	if !hasDirectiveKey(directives, "user") {
		if user, _ := splitSSHDestination(spec.BaseHost); user != "" {
			block = append(block, fmt.Sprintf("    User %s", user))
		}
	}
	for _, d := range directives {
		block = append(block, fmt.Sprintf("    %s %s", displayDirectiveKey(d.key), d.value))
	}

	block = append(block,
		fmt.Sprintf("    RemoteForward %d 127.0.0.1:%d", spec.RemotePort, spec.LocalPort),
		"    ControlMaster no",
		"    ControlPath none",
		"    RequestTTY yes",
		fmt.Sprintf("    RemoteCommand test -x ~/.local/bin/cc-clip-shell-enter && exec ~/.local/bin/cc-clip-shell-enter %s %d %s || exec \"${SHELL:-/bin/bash}\" -i", spec.PeerID, spec.RemotePort, spec.PeerLabel),
		endMarker,
		"",
	)
	return block
}

func resolveAliasDirectives(configPath string, lines []string, host string) []sshDirective {
	if directives, err := resolveManagedAliasDirectives(configPath, host); err == nil && len(directives) > 0 {
		return directives
	}
	return effectiveDirectives(lines, host)
}

func resolveManagedAliasDirectivesWithSSH(configPath, host string) ([]sshDirective, error) {
	cmd := exec.Command("ssh", "-G", "-F", configPath, host)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	allow := map[string]bool{
		"batchmode":                    true,
		"certificatefile":              true,
		"forwardagent":                 true,
		"globalknownhostsfile":         true,
		"hostkeyalias":                 true,
		"hostname":                     true,
		"identitiesonly":               true,
		"identityagent":                true,
		"identityfile":                 true,
		"kbdinteractiveauthentication": true,
		"localforward":                 true,
		"passwordauthentication":       true,
		"port":                         true,
		"preferredauthentications":     true,
		"proxycommand":                 true,
		"proxyjump":                    true,
		"pubkeyauthentication":         true,
		"remoteforward":                true,
		"serveraliveinterval":          true,
		"stricthostkeychecking":        true,
		"user":                         true,
		"userknownhostsfile":           true,
	}
	var directives []sshDirective
	for _, line := range strings.Split(string(out), "\n") {
		key, value := parseSSHDirective(line)
		if key == "" {
			continue
		}
		key = strings.ToLower(key)
		if !allow[key] {
			continue
		}
		directives = append(directives, sshDirective{key: key, value: value})
	}
	return directives, nil
}

func effectiveDirectives(lines []string, host string) []sshDirective {
	var (
		matched bool
		seen    = map[string]bool{}
		eff     []sshDirective
	)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isAnyHostLine(trimmed) {
			matched = hostMatchesPattern(trimmed, host)
			continue
		}
		if !matched {
			continue
		}
		key, val := parseSSHDirective(trimmed)
		if key == "" {
			continue
		}
		key = strings.ToLower(key)
		if key == "identityfile" || key == "localforward" {
			eff = append(eff, sshDirective{key: key, value: val})
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		eff = append(eff, sshDirective{key: key, value: val})
	}
	return eff
}

func copyableDirectives(dirs []sshDirective, spec ManagedAliasSpec) []sshDirective {
	seen := map[string]bool{}
	var kept []sshDirective
	for _, d := range dirs {
		if isManagedAliasOverrideDirective(d, spec) {
			continue
		}
		// Keep repeated directives such as IdentityFile, LocalForward, and
		// unrelated RemoteForward entries from the base host.
		if d.key != "identityfile" && d.key != "localforward" && d.key != "remoteforward" && seen[d.key] {
			continue
		}
		seen[d.key] = true
		kept = append(kept, d)
	}
	return kept
}

func isManagedAliasOverrideDirective(d sshDirective, spec ManagedAliasSpec) bool {
	switch d.key {
	case "controlmaster", "controlpath", "requesttty", "remotecommand":
		return true
	case "remoteforward":
		return remoteForwardUsesListenPort(d.value, spec.RemotePort) ||
			isLegacyCCClipRemoteForward(d.value)
	default:
		return false
	}
}

func isLegacyCCClipRemoteForward(value string) bool {
	listenPort, targetHost, _, ok := parseRemoteForward(value)
	if !ok {
		return false
	}
	if listenPort < peer.DefaultRangeStart || listenPort > peer.DefaultRangeEnd {
		return false
	}
	switch targetHost {
	case "127.0.0.1", "localhost":
		return true
	default:
		return false
	}
}

func parseRemoteForward(value string) (int, string, int, bool) {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) < 2 {
		return 0, "", 0, false
	}
	listenPort, ok := parseRemoteForwardListenPort(fields[0])
	if !ok {
		return 0, "", 0, false
	}
	targetHost, targetPort, ok := parseHostPort(fields[len(fields)-1])
	if !ok {
		return 0, "", 0, false
	}
	return listenPort, targetHost, targetPort, true
}

func parseHostPort(token string) (string, int, bool) {
	token = strings.TrimSpace(token)
	i := strings.LastIndex(token, ":")
	if i <= 0 || i >= len(token)-1 {
		return "", 0, false
	}
	port, err := strconv.Atoi(token[i+1:])
	if err != nil {
		return "", 0, false
	}
	host := strings.Trim(token[:i], "[]")
	if host == "" {
		return "", 0, false
	}
	return host, port, true
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

func hasDirectiveKey(dirs []sshDirective, key string) bool {
	for _, d := range dirs {
		if d.key == key {
			return true
		}
	}
	return false
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
	case "userknownhostsfile":
		return "UserKnownHostsFile"
	default:
		if key == "" {
			return ""
		}
		return strings.ToUpper(key[:1]) + key[1:]
	}
}

func managedAliasMarkers(alias string) (string, string) {
	start := fmt.Sprintf("%s %s %s", managedMarkerPrefix, alias, managedMarkerSuffix)
	end := fmt.Sprintf("# <<< cc-clip managed: %s <<<", alias)
	return start, end
}

func findManagedAliasRange(lines []string, startMarker, endMarker string) (int, int, bool) {
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == startMarker {
			start = i
			continue
		}
		if start >= 0 && strings.TrimSpace(line) == endMarker {
			end := i + 1
			if end < len(lines) && strings.TrimSpace(lines[end]) == "" {
				end++
			}
			return start, end, true
		}
	}
	return 0, 0, false
}

func findHostLineOutsideRange(lines []string, host string, skipStart, skipEnd int) (int, bool) {
	for i, line := range lines {
		if skipStart >= 0 && i >= skipStart && i < skipEnd {
			continue
		}
		if matchesHost(strings.TrimSpace(line), host) {
			return i, true
		}
	}
	return 0, false
}

func findManagedAliasInsertLine(lines []string, alias string) int {
	for i, line := range lines {
		if hostMatchesPattern(strings.TrimSpace(line), alias) {
			return i
		}
	}
	return -1
}

func insertManagedAliasBlock(lines, aliasBlock []string, insertAt int) []string {
	if insertAt >= 0 {
		result := make([]string, 0, len(lines)+len(aliasBlock))
		result = append(result, lines[:insertAt]...)
		if len(result) > 0 && strings.TrimSpace(result[len(result)-1]) != "" {
			result = append(result, "")
		}
		result = append(result, aliasBlock...)
		result = append(result, lines[insertAt:]...)
		return result
	}

	result := append([]string{}, lines...)
	if len(result) > 0 && strings.TrimSpace(result[len(result)-1]) != "" {
		result = append(result, "")
	}
	result = append(result, aliasBlock...)
	return result
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

func hostMatchesPattern(trimmed, host string) bool {
	if !isAnyHostLine(trimmed) {
		return false
	}
	matched := false
	for _, pattern := range strings.Fields(trimmed)[1:] {
		negated := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimPrefix(pattern, "!")
		ok, err := path.Match(pattern, host)
		if err != nil {
			ok = pattern == host
		}
		if !ok {
			continue
		}
		if negated {
			return false
		}
		matched = true
	}
	return matched
}

func splitSSHDestination(target string) (string, string) {
	if i := strings.LastIndex(target, "@"); i > 0 && i < len(target)-1 {
		return target[:i], target[i+1:]
	}
	return "", target
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
