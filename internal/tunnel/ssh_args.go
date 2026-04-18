package tunnel

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/shunmei/cc-clip/internal/win32"
)

var (
	sshBinaryMu       sync.Mutex
	sshBinaryPath     string
	sshBinaryErr      error
	sshBinaryOverride string // the CC_CLIP_SSH value the cache was populated against
	sshBinaryCached   bool
)

// resolveSSHBinary returns the ssh path for the persistent tunnel process.
// Invalidates the cache when CC_CLIP_SSH changes between calls so a test or
// operator that flips the env var at runtime doesn't keep getting the stale
// value forever, and drops the cache when the cached path has disappeared
// from disk (e.g., package upgrade replaced the binary).
func resolveSSHBinary() (string, error) {
	override := strings.TrimSpace(os.Getenv("CC_CLIP_SSH"))
	sshBinaryMu.Lock()
	defer sshBinaryMu.Unlock()
	if sshBinaryCached && sshBinaryOverride == override {
		if sshBinaryErr != nil {
			return "", sshBinaryErr
		}
		if _, err := os.Stat(sshBinaryPath); err == nil {
			return sshBinaryPath, nil
		}
		// cached path is gone; fall through to a fresh resolve
	}
	sshBinaryPath, sshBinaryErr = "", nil
	sshBinaryOverride = override
	sshBinaryCached = true
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			sshBinaryErr = fmt.Errorf("resolve CC_CLIP_SSH=%q: %w", override, err)
			return "", sshBinaryErr
		}
		if err := requireTrustedSSHBinaryPrefix(abs); err != nil {
			sshBinaryErr = err
			return "", sshBinaryErr
		}
		sshBinaryPath = abs
		return sshBinaryPath, nil
	}
	path, err := exec.LookPath("ssh")
	if err != nil {
		sshBinaryErr = fmt.Errorf("locate ssh binary: %w", err)
		return "", sshBinaryErr
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		sshBinaryErr = fmt.Errorf("resolve ssh binary path: %w", err)
		return "", sshBinaryErr
	}
	sshBinaryPath = abs
	return sshBinaryPath, nil
}

// requireTrustedSSHBinaryPrefix refuses CC_CLIP_SSH overrides that resolve
// outside a small allowlist of system/user-local bin directories. Without
// this, a writable dotfile setting CC_CLIP_SSH=/tmp/evil-ssh would redirect
// every reconnect through an attacker-controlled binary for the lifetime of
// the daemon.
func requireTrustedSSHBinaryPrefix(abs string) error {
	allowed := []string{"/usr/bin/", "/bin/", "/usr/local/bin/", "/opt/homebrew/bin/", "/usr/sbin/", "/sbin/"}
	if home, err := os.UserHomeDir(); err == nil {
		allowed = append(allowed, filepath.Join(home, ".local", "bin")+string(filepath.Separator), filepath.Join(home, "bin")+string(filepath.Separator))
	}
	for _, prefix := range allowed {
		if strings.HasPrefix(abs, prefix) {
			return nil
		}
	}
	return fmt.Errorf("CC_CLIP_SSH=%q must live under a trusted prefix (e.g. /usr/bin, ~/.local/bin)", abs)
}

// maxSSHHostLength limits SSH host alias length. DNS labels cap at 253
// characters; SSH aliases are typically shorter but this gives headroom
// while still rejecting wildly oversized values that are almost certainly
// malicious or buggy input.
const maxSSHHostLength = 253

// ValidateSSHHost returns an error if host is unsafe to pass as an argv
// parameter to `ssh`. It rejects empty strings, leading `-` (which ssh
// treats as a flag), excessive length, whitespace, NULs, control
// characters, and any character outside `[A-Za-z0-9._:@-]`. Callers that
// accept hostnames from untrusted input (HTTP handlers, file-driven
// configs) should invoke this before persisting or spawning ssh.
func ValidateSSHHost(host string) error {
	if host == "" {
		return fmt.Errorf("ssh host must not be empty")
	}
	if len(host) > maxSSHHostLength {
		return fmt.Errorf("ssh host length %d exceeds max %d", len(host), maxSSHHostLength)
	}
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("ssh host must not start with '-' (ambiguous with ssh flag): %q", host)
	}
	for i, r := range host {
		if r == 0 {
			return fmt.Errorf("ssh host must not contain NUL byte at index %d", i)
		}
		if unicode.IsSpace(r) {
			return fmt.Errorf("ssh host must not contain whitespace: %q", host)
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("ssh host must not contain control character at index %d: %q", i, host)
		}
		if !isAllowedSSHHostRune(r) {
			return fmt.Errorf("ssh host contains disallowed character %q at index %d: %q", r, i, host)
		}
	}
	return nil
}

// isSafeSSHOptionKey returns true for keys that look like real ssh_config
// option names. Keys are conventionally alphanumeric with no internal
// punctuation; anything outside [A-Za-z][A-Za-z0-9]* is rejected so a
// pathological `ssh -G` output cannot smuggle `=`, whitespace, or flag-like
// prefixes into the `-o key=value` argv entry.
func isSafeSSHOptionKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if i == 0 {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				return false
			}
			continue
		}
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func isAllowedSSHHostRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	}
	switch r {
	case '.', '_', ':', '@', '-':
		return true
	}
	return false
}

// defaultSSHConfigQuery invokes `ssh -G <host>` to resolve the effective
// ssh configuration. ctx bounds the subprocess so a wedged ssh (stuck
// Match exec, frozen DNS resolver) cannot pin the reconnect goroutine.
func defaultSSHConfigQuery(ctx context.Context, host string) (string, error) {
	sshPath, err := resolveSSHBinary()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, sshPath, "-G", host)
	win32.HideConsoleWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return "", fmt.Errorf("ssh -G %s failed: %w (%s)", host, err, msg)
		}
		return "", fmt.Errorf("ssh -G %s failed: %w", host, err)
	}
	return string(out), nil
}

// sshConfigQueryFunc is the package-level hook used by sshTunnelArgs.
// Tests swap it (with t.Cleanup) to avoid shelling out to real ssh.
//
// NOT safe for t.Parallel: writes to this variable from test setup race
// with any concurrent reader. Callers that want parallel tests must not
// mutate this var. A test helper (e.g. newManagerForTest with a per-call
// hook) would be required first.
var sshConfigQueryFunc = defaultSSHConfigQuery

const sshConfigResolveTimeout = 10 * time.Second

// excludedTunnelSSHOptions names ssh_config directives that must not be
// forwarded into the persistent-tunnel argv. Command-valued directives
// (ProxyCommand, KnownHostsCommand, LocalCommand, etc.) would cause the
// unattended reconnect loop to exec arbitrary user-config-supplied shell
// commands — very different from the interactive-session posture where
// the user sees and approves the same directive at least once. Agent /
// X11 forwarding is stripped because inheriting it would expose the
// local ssh-agent socket or X display to the remote for as long as the
// tunnel is alive. Identity-position and session-type directives are
// stripped so the tunnel is always `-N -R …`.
var excludedTunnelSSHOptions = map[string]struct{}{
	"batchmode":               {},
	"canonicalizecommand":     {},
	"clearallforwardings":     {},
	"controlmaster":           {},
	"controlpath":             {},
	"controlpersist":          {},
	"dynamicforward":          {},
	"exitonforwardfailure":    {},
	"forkafterauthentication": {},
	"forwardagent":            {},
	"forwardx11":              {},
	"forwardx11trusted":       {},
	"host":                    {},
	"knownhostscommand":       {},
	"localcommand":            {},
	"localforward":            {},
	"match":                   {},
	"originalhost":            {},
	"permitlocalcommand":      {},
	"proxycommand":            {},
	"remotecommand":           {},
	"remoteforward":           {},
	"requesttty":              {},
	"serveralivecountmax":     {},
	"serveraliveinterval":     {},
	"sessiontype":             {},
	"sshaskpass":              {},
}

func resolveSSHTunnelConfig(ctx context.Context, cfg TunnelConfig) (TunnelConfig, error) {
	if err := ValidateSSHHost(cfg.Host); err != nil {
		return TunnelConfig{}, fmt.Errorf("invalid ssh host: %w", err)
	}
	if cfg.SSHConfigResolved {
		if err := validateResolvedTunnelOptions(cfg.SSHOptions); err != nil {
			return TunnelConfig{}, fmt.Errorf("invalid cached ssh options: %w", err)
		}
		return cfg, nil
	}
	resolveCtx, cancel := context.WithTimeout(ctx, sshConfigResolveTimeout)
	defer cancel()
	resolved, err := sshConfigQueryFunc(resolveCtx, cfg.Host)
	if err != nil {
		return TunnelConfig{}, err
	}
	cfg.SSHOptions = resolvedTunnelOptionsFromSSHConfig(resolved)
	cfg.SSHConfigResolved = true
	if err := validateResolvedTunnelOptions(cfg.SSHOptions); err != nil {
		return TunnelConfig{}, fmt.Errorf("invalid resolved ssh options: %w", err)
	}
	return cfg, nil
}

func sshTunnelArgs(ctx context.Context, cfg TunnelConfig) ([]string, func(), error) {
	if err := ValidateSSHHost(cfg.Host); err != nil {
		return nil, nil, fmt.Errorf("invalid ssh host: %w", err)
	}
	if !cfg.SSHConfigResolved {
		return nil, nil, fmt.Errorf("%w for %s; rerun `cc-clip tunnel up %s`", ErrTunnelSSHOptionsUnresolved, cfg.Host, cfg.Host)
	}
	if err := validateResolvedTunnelOptions(cfg.SSHOptions); err != nil {
		return nil, nil, fmt.Errorf("invalid cached ssh options: %w", err)
	}
	// Keep the scratch config under a private temp dir so the path remains
	// short and rediscovery-friendly on Unix while still keeping the file
	// unreadable to other local users. The file's basename must match the
	// `cc-clip-ssh-config-*.conf` shape that matchesManagedTunnelProcess
	// anchors on — otherwise stale-PID cleanup and adoption silently fail to
	// identify processes we launched.
	scratchDir, err := os.MkdirTemp(sshTunnelScratchRoot(), "cc-clip-ssh-config-")
	if err != nil {
		return nil, nil, fmt.Errorf("create ssh scratch dir: %w", err)
	}
	configFile, err := os.CreateTemp(scratchDir, "cc-clip-ssh-config-*.conf")
	if err != nil {
		_ = os.RemoveAll(scratchDir)
		return nil, nil, fmt.Errorf("create empty ssh config: %w", err)
	}
	configPath := configFile.Name()
	if err := configFile.Chmod(0600); err != nil {
		_ = configFile.Close()
		_ = os.RemoveAll(scratchDir)
		return nil, nil, fmt.Errorf("secure ssh config perms: %w", err)
	}
	if err := configFile.Close(); err != nil {
		_ = os.RemoveAll(scratchDir)
		return nil, nil, fmt.Errorf("close ssh config: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(scratchDir)
	}
	return buildSSHTunnelArgs(cfg, configPath), cleanup, nil
}

func sshTunnelScratchRoot() string {
	root := os.TempDir()
	if runtime.GOOS == "windows" {
		return root
	}
	if strings.ContainsAny(root, " \t\n\r'\"") {
		if _, err := os.Stat("/tmp"); err == nil {
			return "/tmp"
		}
	}
	return root
}

func buildSSHTunnelArgs(cfg TunnelConfig, configPath string) []string {
	args := []string{"-F", configPath}

	for _, opt := range cfg.SSHOptions {
		args = append(args, "-o", opt)
	}

	args = append(args,
		"-N",
		"-v",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-R", fmt.Sprintf("%d:127.0.0.1:%d", cfg.RemotePort, cfg.LocalPort),
		cfg.Host,
	)
	return args
}

func resolvedTunnelOptionsFromSSHConfig(resolved string) []string {
	var opts []string
	for _, line := range strings.Split(resolved, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.ToLower(fields[0])
		if _, skip := excludedTunnelSSHOptions[key]; skip {
			continue
		}
		// Require the option key to be a well-formed identifier so we never
		// pass anything ssh could interpret as a flag or as a second option.
		// ssh option names are conventionally alphanumeric; rejecting anything
		// outside that set means a future ssh release that emits an odd token
		// as a key cannot smuggle it through into the argv.
		if !isSafeSSHOptionKey(fields[0]) {
			continue
		}
		value := strings.TrimSpace(line[len(fields[0]):])
		if value == "" {
			continue
		}
		// Refuse values with embedded control characters; `ssh -G` emits one
		// directive per line, so a value containing \r or \n would either be a
		// malformed line we should skip or an attempt to smuggle additional
		// directives through the `-o key=value` arg boundary.
		if strings.ContainsAny(value, "\x00\r\n") {
			continue
		}
		opts = append(opts, fmt.Sprintf("%s=%s", fields[0], value))
	}
	return opts
}
