package setup

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shunmei/cc-clip/internal/fileutil"
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

var ErrManagedRemotePortInvalid = errors.New("managed RemoteForward is invalid")
var ErrSSHHostBlockNotFound = errors.New("ssh host block not found")

// ErrInvalidSSHHost is returned when a host alias supplied to a setup function
// cannot be safely used as an exact Host pattern in ssh_config — e.g. it
// contains glob meta characters, a negation prefix, whitespace, or starts
// with `-`. cc-clip manages only dedicated exact aliases; wildcards like
// `Host *` must never be treated as cc-clip-owned blocks.
var ErrInvalidSSHHost = errors.New("invalid ssh host alias")

// ErrSharedHostStanza is returned when the only block matching a given host
// alias groups multiple positive patterns together (e.g. `Host prod staging`).
// cc-clip manages only dedicated exact `Host <alias>` blocks; shared stanzas
// must be split into separate aliases by the user before any operation that
// writes to or reads from the managed block can proceed. This mirrors the
// documented product boundary in CLAUDE.md / README.md.
var ErrSharedHostStanza = errors.New("ssh host alias lives in a shared stanza; split into a dedicated alias")

type ManagedTunnelPorts struct {
	RemotePort int
	LocalPort  int
}

const maxForwardPort = 65535

// maxHostAliasLength matches DNS label cap; ssh host aliases are typically
// much shorter but we give headroom while still rejecting oversized values.
const maxHostAliasLength = 253

// validateHostAlias rejects host values that must not be treated as a
// dedicated exact `Host <alias>` block. cc-clip manages only exact aliases,
// so glob meta characters (*, ?), negation prefixes (!), whitespace, control
// chars, and leading `-` are all refused at entry. Keeping this check close
// to every setup.* entry point prevents a future caller from accidentally
// writing a managed block under `Host *`.
func validateHostAlias(host string) error {
	if host == "" {
		return fmt.Errorf("%w: host must not be empty", ErrInvalidSSHHost)
	}
	if len(host) > maxHostAliasLength {
		return fmt.Errorf("%w: host length %d exceeds max %d", ErrInvalidSSHHost, len(host), maxHostAliasLength)
	}
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("%w: host must not start with '-': %q", ErrInvalidSSHHost, host)
	}
	if strings.HasPrefix(host, "!") {
		return fmt.Errorf("%w: host must not be a negation pattern: %q", ErrInvalidSSHHost, host)
	}
	// Reject path-traversal-looking characters even though today's only
	// downstream use is as ssh_config / marker text — a future caller that
	// embeds the host in a filename (e.g., state file or log path) would
	// silently inherit the vulnerability without this gate.
	if strings.ContainsAny(host, "/\\") {
		return fmt.Errorf("%w: host must not contain path separators: %q", ErrInvalidSSHHost, host)
	}
	if host == ".." || strings.Contains(host, "../") || strings.Contains(host, "..\\") {
		return fmt.Errorf("%w: host must not be or contain a parent-directory reference: %q", ErrInvalidSSHHost, host)
	}
	for _, r := range host {
		switch r {
		case '*', '?':
			return fmt.Errorf("%w: host must not contain glob meta characters: %q", ErrInvalidSSHHost, host)
		case ' ', '\t', '\n', '\r', 0:
			return fmt.Errorf("%w: host must not contain whitespace or NUL: %q", ErrInvalidSSHHost, host)
		}
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: host must not contain control characters: %q", ErrInvalidSSHHost, host)
		}
	}
	return nil
}

// EnsureSSHConfig ensures ~/.ssh/config has required directives for cc-clip:
//   - RemoteForward <port> 127.0.0.1:<port>
//   - ControlMaster no
//   - ControlPath none
//
// If the host block doesn't exist, it is created before "Host *".
// A backup is written to ~/.ssh/config.cc-clip-backup before any modification.
func EnsureSSHConfig(host string, port int) ([]SSHConfigChange, error) {
	if err := validateHostAlias(host); err != nil {
		return nil, err
	}
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
	if err := validateHostAlias(spec.Host); err != nil {
		return nil, err
	}
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
	if err := validateHostAlias(host); err != nil {
		return err
	}
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

	lines, lineEnding := splitSSHConfigLines(content)
	block := findHostBlock(lines, host)
	if block != nil && !block.hasExactHostPattern(host) && block.positivePatternCount() > 1 {
		return nil, fmt.Errorf("%w: %s", ErrSharedHostStanza, host)
	}
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
			// strings.Split("", "\n") returns [""] — a single empty element
			// representing "no lines". Dropping that artifact before append
			// keeps a freshly-created config from starting with a spurious
			// leading newline.
			if len(lines) == 1 && lines[0] == "" {
				lines = lines[:0]
			}
			if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
				lines = append(lines, "")
			}
			lines = append(lines, newBlock...)
		}
		changes = append(changes, SSHConfigChange{"created", fmt.Sprintf("Host %s (RemoteForward, ControlMaster no, ControlPath none)", host)})
		modified = true
	} else {
		type required struct {
			key   string
			value string
		}
		directives := []required{
			{"RemoteForward", rfValue},
			{"ControlMaster", "no"},
			{"ControlPath", "none"},
		}
		for _, d := range directives {
			if block.hasDirective(strings.ToLower(d.key), d.value) {
				changes = append(changes, SSHConfigChange{"ok", fmt.Sprintf("%s %s", d.key, d.value)})
				continue
			}
			if strings.EqualFold(d.key, "RemoteForward") {
				if line := block.directiveLine("remoteforward", func(value string) bool {
					return remoteForwardUsesListenPort(value, port)
				}); line >= 0 {
					lines[line] = fmt.Sprintf("    %s %s", d.key, d.value)
					changes = append(changes, SSHConfigChange{"updated", fmt.Sprintf("%s %s", d.key, d.value)})
					modified = true
					continue
				}
			}
			line := fmt.Sprintf("    %s %s", d.key, d.value)
			lines = insertDirectiveInBlock(lines, block, line)
			block.endLine++
			changes = append(changes, SSHConfigChange{"added", fmt.Sprintf("%s %s", d.key, d.value)})
			modified = true
		}
	}

	if modified {
		if _, err := writeSSHConfigBackup(configPath, content); err != nil {
			return nil, err
		}
		newContent := joinSSHConfigLines(lines, lineEnding)
		if writeErr := writeSSHConfigFile(configPath, []byte(newContent)); writeErr != nil {
			return nil, fmt.Errorf("cannot write %s: %w", configPath, writeErr)
		}
	} else if err := ensureSSHConfigMode(configPath); err != nil {
		return nil, fmt.Errorf("cannot secure %s: %w", configPath, err)
	}

	return changes, nil
}

// writeSSHConfigFile writes the ssh config atomically at mode 0600.
// Directives can reference sensitive values (IdentityFile, ProxyJump,
// RemoteForward targets), so we never honor a looser mode even if the
// original file had one.
func writeSSHConfigFile(path string, data []byte) error {
	const perm = os.FileMode(0600)
	writePath, err := resolveSSHConfigWritePath(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(writePath)
	// Use the resolved target's basename (not the original `path`) for the
	// tmp filename. When `path` is a symlink pointing at a file in a
	// different directory, the tmp file lives next to the target; naming it
	// after the target avoids confusing paths like `.../real-config-dir/config.tmp-XYZ`
	// (where `config` is the name of the link, not the file actually being
	// rewritten).
	tmp, err := os.CreateTemp(dir, filepath.Base(writePath)+".tmp-*")
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
	if err := fileutil.RenameReplace(tmpPath, writePath); err != nil {
		cleanup()
		return err
	}
	// Double-check the mode after rename. The tmp file was already chmod'd
	// to 0600 before rename, so the content is safe on disk either way;
	// this post-rename chmod is defense-in-depth for platforms where rename
	// might swap the mode (e.g. Windows reparse points). Treat a failure
	// here as a non-fatal warning: returning an error would signal "write
	// failed" to the caller when the write actually succeeded, potentially
	// triggering retry loops against a file that is already correct.
	if chmodErr := os.Chmod(writePath, perm); chmodErr != nil {
		log.Printf("cc-clip: warning: post-rename chmod 0600 %s failed (tmp file was already 0600; config is on disk): %v", writePath, chmodErr)
	}
	// fsync the parent directory so the rename is durable across crashes.
	// On platforms that cannot open a directory (e.g. Windows), skip
	// silently. On platforms that can open a directory, a sync failure is
	// surfaced as a non-fatal diagnostic via both log and the test-observable
	// hook so operators notice; the rename itself has already succeeded and
	// the new content is visible to readers, so the write is reported OK.
	if dirFile, dirErr := os.Open(dir); dirErr == nil {
		if syncErr := dirFile.Sync(); syncErr != nil {
			msg := fmt.Sprintf("cc-clip: warning: fsync %s failed after renaming config: %v", dir, syncErr)
			log.Print(msg)
			if sshConfigDirFsyncFailureHook != nil {
				sshConfigDirFsyncFailureHook(dir, syncErr)
			}
		}
		_ = dirFile.Close()
	}
	return nil
}

// sshConfigDirFsyncFailureHook is an optional observer invoked with the
// directory path and the sync error when the parent-directory fsync fails
// after a successful rename. Production callers leave this nil; tests use
// it to assert the non-fatal path surfaces the error instead of silently
// swallowing it.
var sshConfigDirFsyncFailureHook func(dir string, err error)

// resolveSSHConfigWritePath returns the real file path that `path` resolves
// to, following symlink chains of any length. When `path` is not a symlink,
// the input is returned unchanged. When `path` does not exist, the input is
// returned unchanged so the caller can create it.
//
// EvalSymlinks handles arbitrary-length chains (chezmoi / stow / yadm commonly
// produce two-hop chains: ~/.ssh/config → ~/dotfiles/ssh/config → …). A single
// os.Readlink call only unwrapped one level, silently rewriting the
// intermediate symlink as a regular file on the first save and detaching the
// managed config from the dotfiles repo.
func resolveSSHConfigWritePath(path string) (string, error) {
	info, lstatErr := os.Lstat(path)
	switch {
	case os.IsNotExist(lstatErr):
		return path, nil
	case lstatErr != nil:
		return "", fmt.Errorf("lstat %s: %w", path, lstatErr)
	case info.Mode()&os.ModeSymlink == 0:
		return path, nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve symlink %s: %w", path, err)
	}
	return filepath.Clean(resolved), nil
}

func ensureSSHConfigMode(path string) error {
	writePath, err := resolveSSHConfigWritePath(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(writePath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.Chmod(writePath, 0600)
}

// splitSSHConfigLines splits raw config content into lines, preserving the
// detected line-ending convention so it can be restored on write. The returned
// lineEnding is either "\r\n" (majority CRLF) or "\n". Individual trailing
// "\r" characters are stripped from each line so downstream parsers don't see
// them. A leading UTF-8 BOM (EF BB BF), which some Windows editors insert
// automatically, is stripped so the first line's `Host` keyword is not
// prefixed with invisible bytes that break pattern matching.
func splitSSHConfigLines(content []byte) ([]string, string) {
	s := string(content)
	const utf8BOM = "\ufeff"
	s = strings.TrimPrefix(s, utf8BOM)
	crlfCount := strings.Count(s, "\r\n")
	// Approximate LF-only line count by counting LF occurrences that are not
	// part of a CRLF. This lets us pick the dominant style even in mixed
	// files without a second pass.
	totalLF := strings.Count(s, "\n")
	lfOnly := totalLF - crlfCount
	lineEnding := "\n"
	// Prefer the dominant convention; on an exact tie, prefer LF so a single
	// pasted CRLF snippet does not force the whole file to CRLF on rewrite.
	if crlfCount > 0 && crlfCount > lfOnly {
		lineEnding = "\r\n"
	}
	// strings.Split on "\n" leaves a trailing "\r" on CRLF lines; strip it so
	// parsers see clean content. When we rejoin with CRLF, the "\r" is
	// re-added by joinSSHConfigLines.
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.HasSuffix(line, "\r") {
			lines[i] = strings.TrimSuffix(line, "\r")
		}
	}
	return lines, lineEnding
}

// joinSSHConfigLines rejoins lines using the provided line-ending, mirroring
// what splitSSHConfigLines produced. An empty lineEnding defaults to "\n".
func joinSSHConfigLines(lines []string, lineEnding string) string {
	if lineEnding == "" {
		lineEnding = "\n"
	}
	return strings.Join(lines, lineEnding)
}

// writeSSHConfigBackup writes the pristine pre-cc-clip copy of the config
// to <path>.cc-clip-backup the first time it is called for that path; on
// subsequent runs it leaves the pristine backup alone. Always returns the
// empty string — callers used to treat the return value as a rollback path,
// but cc-clip intentionally keeps a successfully-written backup on disk even
// when the subsequent config write fails, so there is nothing to roll back.
// For a missing or zero-length config, the pristine backup is an empty file:
// that still gives operators a concrete sidecar proving no pre-existing SSH
// config content was overwritten on the first cc-clip rewrite.
//
// Unlike writeSSHConfigFile, the backup path MUST NOT follow symlinks:
// otherwise a pre-existing symlink at <path>.cc-clip-backup could redirect
// writeSSHConfigFile's atomic rename at an attacker-chosen path, dumping the
// full SSH config (IdentityFile / ProxyCommand / etc.) somewhere else.
// lstat + O_EXCL|O_NOFOLLOW on create closes that window.
func writeSSHConfigBackup(configPath string, original []byte) (string, error) {
	pristinePath := configPath + ".cc-clip-backup"
	if info, statErr := os.Lstat(pristinePath); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to write backup at %s: path is a symlink", pristinePath)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("refusing to use backup at %s: path is not a regular file", pristinePath)
		}
		if err := os.Chmod(pristinePath, 0600); err != nil {
			return "", fmt.Errorf("chmod 0600 %s: %w", pristinePath, err)
		}
		return "", nil
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("cannot stat backup %s: %w", pristinePath, statErr)
	}
	if err := writeBackupFileNoFollow(pristinePath, original); err != nil {
		return "", fmt.Errorf("cannot write backup %s: %w", pristinePath, err)
	}
	return "", nil
}

// writeBackupFileNoFollow creates path with O_EXCL|O_NOFOLLOW so a symlink
// (or race-planted regular file) cannot redirect the write. The caller has
// already verified that path does not exist; O_EXCL turns a race into a
// hard error rather than a silent clobber.
//
// fsyncs both the file and the parent directory so a power-loss window
// cannot leave the directory entry un-persisted. The backup exists
// specifically so a user whose edit got clobbered by a racing cc-clip write
// can recover — defeating that recovery via a non-durable directory entry
// would invert the purpose of keeping the backup.
func writeBackupFileNoFollow(path string, data []byte) error {
	const perm = os.FileMode(0600)
	flags := os.O_CREATE | os.O_EXCL | os.O_WRONLY | sysNoFollow
	f, err := os.OpenFile(path, flags, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	if dirFile, dirErr := os.Open(filepath.Dir(path)); dirErr == nil {
		if syncErr := dirFile.Sync(); syncErr != nil {
			log.Printf("cc-clip: warning: fsync %s failed after writing backup: %v", filepath.Dir(path), syncErr)
		}
		_ = dirFile.Close()
	}
	return nil
}

func ensureManagedHostConfigAt(configPath string, spec ManagedHostSpec) ([]SSHConfigChange, error) {
	if err := validateHostAlias(spec.Host); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot read %s: %w", configPath, err)
	}

	lines, lineEnding := splitSSHConfigLines(content)
	block := findHostBlock(lines, spec.Host)
	if block == nil {
		return nil, fmt.Errorf("ssh config missing exact Host %s block; define an explicit alias in ~/.ssh/config first", spec.Host)
	}
	if !block.hasExactHostPattern(spec.Host) {
		return nil, fmt.Errorf("%w: %s (define an explicit alias in ~/.ssh/config first)", ErrSharedHostStanza, spec.Host)
	}

	startMarker, endMarker := managedHostMarkers(spec.Host)
	managed := findManagedRangeInBlock(lines, block, startMarker, endMarker)
	if managed.invalid() {
		return nil, fmt.Errorf("managed Host %s block is malformed", spec.Host)
	}
	if managed.partial() {
		return nil, fmt.Errorf("managed Host %s block is incomplete", spec.Host)
	}
	if err := validateManagedHostConflicts(lines, block, spec, managed.start, managed.end, managed.complete()); err != nil {
		return nil, err
	}

	fragment := managedHostFragment(spec)
	var updated []string
	var change SSHConfigChange
	switch {
	case managed.complete():
		updated = replaceManagedRange(lines, managed.start, managed.end, fragment)
		change = SSHConfigChange{Action: "updated", Detail: fmt.Sprintf("managed Host %s", spec.Host)}
	default:
		insertAt := managedHostInsertLine(lines, block)
		updated = insertLinesAt(lines, insertAt, fragment)
		change = SSHConfigChange{Action: "created", Detail: fmt.Sprintf("managed Host %s", spec.Host)}
	}

	joined := joinSSHConfigLines(updated, lineEnding)
	if joined == string(content) {
		if err := ensureSSHConfigMode(configPath); err != nil {
			return nil, fmt.Errorf("cannot secure %s: %w", configPath, err)
		}
		return []SSHConfigChange{{Action: "ok", Detail: fmt.Sprintf("managed Host %s", spec.Host)}}, nil
	}

	if _, err := writeSSHConfigBackup(configPath, content); err != nil {
		return nil, err
	}
	if writeErr := writeSSHConfigFile(configPath, []byte(joined)); writeErr != nil {
		return nil, fmt.Errorf("cannot write %s: %w", configPath, writeErr)
	}

	return []SSHConfigChange{change}, nil
}

func removeManagedHostConfigAt(configPath, host string) error {
	// Internal helpers are called from tests with unvalidated inputs, and a
	// future internal caller could easily forget the public wrapper's
	// validation. A control-char-laced host would slip into marker strings
	// and break round-trip parsing; an unbounded pattern like "*" could
	// match blocks we never intended to touch.
	if err := validateHostAlias(host); err != nil {
		return err
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot read %s: %w", configPath, err)
	}
	lines, lineEnding := splitSSHConfigLines(content)
	block := findPreferredExactHostBlock(lines, host)
	if block == nil {
		return nil
	}
	startMarker, endMarker := managedHostMarkers(host)
	managed := findManagedRangeInBlock(lines, block, startMarker, endMarker)
	if managed.invalid() {
		return fmt.Errorf("managed Host %s block is malformed", host)
	}
	if managed.partial() {
		return fmt.Errorf("managed Host %s block is incomplete", host)
	}
	if !managed.complete() {
		return nil
	}
	result := replaceManagedRange(lines, managed.start, managed.end, nil)
	joined := joinSSHConfigLines(result, lineEnding)
	if joined == string(content) {
		return nil
	}
	if _, err := writeSSHConfigBackup(configPath, content); err != nil {
		return err
	}
	if writeErr := writeSSHConfigFile(configPath, []byte(joined)); writeErr != nil {
		return writeErr
	}
	return nil
}

type sshBlock struct {
	startLine  int
	endLine    int
	hostLine   string
	patterns   []string
	directives []sshDirective
}

type sshDirective struct {
	key   string // lowercase
	value string
	line  int
}

func (b *sshBlock) hasDirective(key, expectedValue string) bool {
	for _, d := range b.directives {
		if d.key == key && directiveValuesEquivalent(key, d.value, expectedValue) {
			return true
		}
	}
	return false
}

func (b *sshBlock) directiveLine(key string, match func(string) bool) int {
	for _, d := range b.directives {
		if d.key != key {
			continue
		}
		if match != nil && !match(d.value) {
			continue
		}
		return d.line
	}
	return -1
}

func directiveValuesEquivalent(key, actualValue, expectedValue string) bool {
	if strings.EqualFold(key, "remoteforward") {
		return remoteForwardValuesEquivalent(actualValue, expectedValue)
	}
	return normalizeDirectiveValue(actualValue) == normalizeDirectiveValue(expectedValue)
}

func normalizeDirectiveValue(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func remoteForwardValuesEquivalent(actualValue, expectedValue string) bool {
	if normalizeDirectiveValue(actualValue) == normalizeDirectiveValue(expectedValue) {
		return true
	}

	actualListen, actualTarget, ok := parseRemoteForwardSpec(actualValue)
	if !ok {
		return false
	}
	expectedListen, expectedTarget, ok := parseRemoteForwardSpec(expectedValue)
	if !ok {
		return false
	}
	if actualListen.port != expectedListen.port || actualTarget.port != expectedTarget.port {
		return false
	}
	if !remoteForwardListenHostsEquivalent(actualListen, expectedListen) {
		return false
	}
	return forwardHostsEquivalent(actualTarget.host, expectedTarget.host)
}

type forwardListen struct {
	host    string
	port    int
	hasHost bool
}

type forwardTarget struct {
	host string
	port int
}

func parseRemoteForwardSpec(value string) (forwardListen, forwardTarget, bool) {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) != 2 {
		return forwardListen{}, forwardTarget{}, false
	}
	listen, ok := parseRemoteForwardListenSpec(stripSSHQuotes(fields[0]))
	if !ok {
		return forwardListen{}, forwardTarget{}, false
	}
	target, ok := parseForwardTarget(stripSSHQuotes(fields[1]))
	if !ok {
		return forwardListen{}, forwardTarget{}, false
	}
	return listen, target, true
}

// stripSSHQuotes removes one matching pair of surrounding ASCII quotes from a
// token. ssh_config allows values like `RemoteForward "18339" "127.0.0.1:18339"`,
// so individual tokens may arrive wrapped in " or '.
func stripSSHQuotes(token string) string {
	token = strings.TrimSpace(token)
	if len(token) >= 2 {
		first, last := token[0], token[len(token)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return token[1 : len(token)-1]
		}
	}
	return token
}

func parseRemoteForwardListenSpec(token string) (forwardListen, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return forwardListen{}, false
	}
	if n, ok := parseForwardPortNumber(token); ok {
		return forwardListen{port: n}, true
	}

	target, ok := parseForwardTarget(token)
	if !ok {
		return forwardListen{}, false
	}
	return forwardListen{
		host:    target.host,
		port:    target.port,
		hasHost: true,
	}, true
}

func parseForwardTarget(token string) (forwardTarget, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return forwardTarget{}, false
	}

	var (
		host    string
		portStr string
	)
	if strings.HasPrefix(token, "[") {
		end := strings.LastIndex(token, "]")
		if end < 0 || end+1 >= len(token) || token[end+1] != ':' {
			return forwardTarget{}, false
		}
		host = token[1:end]
		portStr = token[end+2:]
	} else {
		lastColon := strings.LastIndex(token, ":")
		if lastColon <= 0 || lastColon == len(token)-1 {
			return forwardTarget{}, false
		}
		host = token[:lastColon]
		portStr = token[lastColon+1:]
	}

	port, ok := parseForwardPortNumber(portStr)
	if !ok {
		return forwardTarget{}, false
	}
	return forwardTarget{
		host: normalizeForwardHost(host),
		port: port,
	}, true
}

func normalizeForwardHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return strings.ToLower(host)
}

func forwardHostsEquivalent(actualHost, expectedHost string) bool {
	return normalizeForwardHost(actualHost) == normalizeForwardHost(expectedHost)
}

func isLoopbackListenHost(host string) bool {
	switch normalizeForwardHost(host) {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

// findHostBlock returns the first Host stanza in file order that matches host.
// OpenSSH applies the first matching stanza, so setup/read paths must use that
// same precedence instead of skipping ahead to a later exact alias.
func findHostBlock(lines []string, host string) *sshBlock {
	return findMatchingHostBlock(lines, host, false)
}

// findPreferredExactHostBlock is for cleanup paths that should still remove a
// managed fragment from the exact alias block even if an earlier shared stanza
// also matches. This is intentionally not used by setup/read flows.
func findPreferredExactHostBlock(lines []string, host string) *sshBlock {
	return findMatchingHostBlock(lines, host, true)
}

func findMatchingHostBlock(lines []string, host string, preferExact bool) *sshBlock {
	var sharedMatch *sshBlock
	// C-style loop so `i = endLine - 1` actually advances past already-scanned
	// blocks; a `for i, line := range lines` loop does not honor the mutation.
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !isAnyHostLine(trimmed) {
			continue
		}
		endLine := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if isSSHBlockBoundaryLine(strings.TrimSpace(lines[j])) {
				endLine = j
				break
			}
		}
		if !matchesHost(trimmed, host) {
			i = endLine - 1
			continue
		}
		block := &sshBlock{
			startLine: i,
			endLine:   endLine,
			hostLine:  trimmed,
			patterns:  hostPatterns(trimmed),
		}
		for j := i + 1; j < endLine; j++ {
			key, val := parseSSHDirective(strings.TrimSpace(lines[j]))
			if key != "" {
				block.directives = append(block.directives, sshDirective{
					key:   strings.ToLower(key),
					value: val,
					line:  j,
				})
			}
		}
		if block.hasExactHostPattern(host) {
			return block
		}
		if !preferExact {
			return block
		}
		if sharedMatch == nil {
			sharedMatch = block
		}
		i = endLine - 1
	}
	return sharedMatch
}

func (b *sshBlock) hasExactHostPattern(host string) bool {
	if b == nil {
		return false
	}
	positivePatterns := 0
	for _, pattern := range b.patterns {
		if strings.HasPrefix(pattern, "!") {
			continue
		}
		if pattern != host {
			return false
		}
		positivePatterns++
	}
	return positivePatterns == 1
}

// positivePatternCount returns how many non-negated patterns the block carries.
// A shared stanza such as `Host prod staging` yields 2. Callers use this to
// reject any operation on a shared stanza up-front.
func (b *sshBlock) positivePatternCount() int {
	if b == nil {
		return 0
	}
	count := 0
	for _, pattern := range b.patterns {
		if strings.HasPrefix(pattern, "!") {
			continue
		}
		count++
	}
	return count
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
	got, ok := parseRemoteForwardListenPort(stripSSHQuotes(fields[0]))
	return ok && got == listenPort
}

func parseRemoteForwardListenPort(token string) (int, bool) {
	listen, ok := parseRemoteForwardListenSpec(token)
	if !ok {
		return 0, false
	}
	return listen.port, true
}

func parseManagedTunnelPortsSpec(value string) (ManagedTunnelPorts, bool) {
	listen, target, ok := parseRemoteForwardSpec(value)
	if !ok {
		return ManagedTunnelPorts{}, false
	}
	// Accept an explicit loopback listen host (e.g. "127.0.0.1:19001
	// 127.0.0.1:18339") as equivalent to the host-less form we emit
	// ourselves, so a user who hand-edits the managed block in this
	// semantically-identical way is not forced into an
	// ErrManagedRemotePortInvalid retry loop.
	if listen.hasHost && !isLoopbackListenHost(listen.host) {
		return ManagedTunnelPorts{}, false
	}
	// Accept `127.0.0.1` and `localhost` (both resolve to the IPv4
	// daemon listener) as targets. `::1` is not accepted because the
	// daemon only binds IPv4 loopback — the forward would land but the
	// TCP connect from ssh would fail.
	switch normalizeForwardHost(target.host) {
	case "127.0.0.1", "localhost":
	default:
		return ManagedTunnelPorts{}, false
	}
	return ManagedTunnelPorts{
		RemotePort: listen.port,
		LocalPort:  target.port,
	}, true
}

func remoteForwardListenHostsEquivalent(actual, expected forwardListen) bool {
	if actual.port != expected.port {
		return false
	}
	switch {
	case !actual.hasHost && !expected.hasHost:
		return true
	case !actual.hasHost:
		return isLoopbackListenHost(expected.host)
	case !expected.hasHost:
		return isLoopbackListenHost(actual.host)
	default:
		return forwardHostsEquivalent(actual.host, expected.host)
	}
}

func parseForwardPortNumber(token string) (int, bool) {
	n, err := strconv.Atoi(token)
	if err != nil || n < 1 || n > maxForwardPort {
		return 0, false
	}
	return n, true
}

// displayDirectiveKey returns the canonical mixed-case name for a
// lowercase directive key. Only keys used by validateManagedHostConflicts
// need a canonical rendering; anything else title-cases.
func displayDirectiveKey(key string) string {
	switch key {
	case "controlmaster":
		return "ControlMaster"
	case "controlpath":
		return "ControlPath"
	}
	if key == "" {
		return ""
	}
	return strings.ToUpper(key[:1]) + key[1:]
}

func managedHostMarkers(host string) (string, string) {
	start := fmt.Sprintf("# >>> cc-clip managed host: %s >>>", host)
	end := fmt.Sprintf("# <<< cc-clip managed host: %s <<<", host)
	return start, end
}

type managedRange struct {
	start      int
	end        int
	startFound bool
	endFound   bool
	malformed  bool
}

func (r managedRange) complete() bool {
	return !r.malformed && r.startFound && r.endFound
}

func (r managedRange) partial() bool {
	return !r.malformed && r.startFound != r.endFound
}

func (r managedRange) invalid() bool {
	return r.malformed
}

func findManagedRangeInBlock(lines []string, block *sshBlock, startMarker, endMarker string) managedRange {
	rng := managedRange{start: -1, end: -1}
	for i := block.startLine + 1; i < block.endLine; i++ {
		switch strings.TrimSpace(lines[i]) {
		case startMarker:
			if rng.startFound {
				rng.malformed = true
				return rng
			}
			rng.start = i
			rng.startFound = true
		case endMarker:
			if !rng.startFound {
				// Stray end marker with no prior start. Leaving the
				// caller to interpret this as "no managed block" would
				// cause a fresh fragment to be inserted above the stray
				// marker, producing an unmatched end line in the output.
				rng.malformed = true
				return rng
			}
			if rng.endFound {
				rng.malformed = true
				return rng
			}
			rng.end = i + 1
			rng.endFound = true
		}
	}
	return rng
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
			expectedValue := "no"
			if lowerKey == "controlpath" {
				expectedValue = "none"
			}
			if directiveValuesEquivalent(lowerKey, value, expectedValue) {
				continue
			}
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
	return hostLineHasPattern(trimmed, host)
}

func hostPatterns(trimmed string) []string {
	if !isAnyHostLine(trimmed) {
		return nil
	}
	// Skip past the leading "Host" keyword (whitespace/tab separator). The
	// keyword is matched case-insensitively so `host foo` / `HOST foo` are
	// parsed the same as `Host foo` — ssh_config(5) keywords are officially
	// case-insensitive and users hand-edit configs in mixed case.
	rest := strings.TrimSpace(trimmed)
	if len(rest) < len("Host") || !strings.EqualFold(rest[:len("Host")], "Host") {
		return nil
	}
	rest = rest[len("Host"):]
	if len(rest) == 0 {
		return nil
	}
	// ssh_config(5) allows `=` as the keyword/value separator with optional
	// whitespace on either side. Accept `Host foo`, `Host=foo`, `Host =foo`,
	// `Host= foo`, and `Host = foo` so users who write the equals form are
	// not silently treated as not having a Host block.
	if rest[0] != ' ' && rest[0] != '\t' && rest[0] != '=' {
		return nil
	}
	rest = strings.TrimLeft(rest, " \t")
	if len(rest) > 0 && rest[0] == '=' {
		rest = strings.TrimLeft(rest[1:], " \t")
	}
	rest = stripSSHInlineComment(rest)
	tokens := splitSSHHostTokens(rest)
	if len(tokens) == 0 {
		return nil
	}
	return tokens
}

// splitSSHHostTokens performs quote-aware whitespace tokenization of a Host
// pattern list. ssh_config(5) allows `Host "pattern with spaces"` to treat
// the quoted value as a single pattern. strings.Fields would split it into
// multiple patterns and mis-classify the line as a shared stanza.
func splitSSHHostTokens(line string) []string {
	var (
		tokens   []string
		current  strings.Builder
		inSingle bool
		inDouble bool
	)
	flush := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, current.String())
		current.Reset()
	}
	for _, r := range line {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case (r == ' ' || r == '\t') && !inSingle && !inDouble:
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return tokens
}

func hostLineHasPattern(trimmed, host string) bool {
	for _, f := range hostPatterns(trimmed) {
		// Skip negation patterns: `Host foo !bar` should not be treated as a
		// positive match for `bar`, even though the literal token matches.
		if strings.HasPrefix(f, "!") {
			continue
		}
		if f == host {
			return true
		}
	}
	return false
}

// firstFieldEqualsFold reports whether the first token of trimmed equals
// keyword (case-insensitively). ssh_config keywords are case-insensitive per
// ssh_config(5), and keywords may be separated from their arguments by either
// whitespace *or* a single `=` (optionally surrounded by whitespace). Both
// `Host foo`/`HOST foo` and `Host=foo`/`host =foo` must be recognized — any
// form that slips past the detector becomes a parser-bypass footgun
// (duplicate managed blocks, shared-stanza mutation, silent Include of an
// attacker-controlled path).
func firstFieldEqualsFold(trimmed, keyword string) bool {
	if len(trimmed) < len(keyword) {
		return false
	}
	if !strings.EqualFold(trimmed[:len(keyword)], keyword) {
		return false
	}
	// Must be followed by whitespace, `=`, or end-of-line.
	if len(trimmed) == len(keyword) {
		return true
	}
	next := trimmed[len(keyword)]
	return next == ' ' || next == '\t' || next == '='
}

func isAnyHostLine(trimmed string) bool {
	return firstFieldEqualsFold(trimmed, "Host")
}

func isMatchLine(trimmed string) bool {
	return firstFieldEqualsFold(trimmed, "Match")
}

func isSSHBlockBoundaryLine(trimmed string) bool {
	return isAnyHostLine(trimmed) || isMatchLine(trimmed)
}

func findHostStarLine(lines []string) int {
	for i, line := range lines {
		if hostLineHasPattern(strings.TrimSpace(line), "*") {
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
	trimmed = stripSSHInlineComment(trimmed)
	if trimmed == "" {
		return "", ""
	}
	// Handle "Key Value", "Key=Value", and "Key = Value". The `=` form is
	// only valid when the key-side consists of a run of key-chars followed
	// by optional whitespace; `=` characters embedded later in the value
	// — e.g. `ProxyCommand ssh -o Foo=bar host` or `RemoteForward
	// 127.0.0.1=...` — must be left untouched so the value round-trips.
	firstEq := strings.IndexByte(trimmed, '=')
	if firstEq >= 0 {
		keyCandidate := strings.TrimRight(trimmed[:firstEq], " \t")
		if keyCandidate != "" && !strings.ContainsAny(keyCandidate, " \t") {
			// `=` sits directly after the key (with optional whitespace).
			// Everything after it is the value; preserve it verbatim so
			// `Key = foo = bar` yields value `foo = bar`.
			value := strings.TrimSpace(trimmed[firstEq+1:])
			return keyCandidate, value
		}
	}
	parts := strings.Fields(trimmed)
	if len(parts) == 0 {
		return "", ""
	}
	if len(parts) == 1 {
		return parts[0], ""
	}
	// Rejoin the remainder; stripping the leading key+space off `trimmed`
	// preserves embedded whitespace / `=` characters in the value.
	key := parts[0]
	// Find the start of the value by skipping the key and one separator.
	rest := strings.TrimLeft(strings.TrimPrefix(trimmed, key), " \t")
	return key, strings.TrimSpace(rest)
}

// stripSSHInlineComment drops the first whitespace-preceded `#` and
// everything after it. ssh_config itself does not implement
// backslash-escaping on comment markers, so we don't either.
func stripSSHInlineComment(line string) string {
	var inSingle, inDouble bool
	for i, r := range line {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if inSingle || inDouble {
				continue
			}
			// Require preceding whitespace (or start of line) so a `#`
			// embedded in a value like `User admin#1` is preserved.
			if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
				return strings.TrimSpace(line[:i])
			}
		}
	}
	return strings.TrimSpace(line)
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

// ReadManagedRemotePort reads the RemoteForward listen port from the
// cc-clip managed block for the given host in ~/.ssh/config.
// Returns 0, nil if the host exists but has no managed block.
// Returns ErrManagedRemotePortInvalid when the managed block exists but does
// not contain a valid RemoteForward directive.
func ReadManagedRemotePort(host string) (int, error) {
	ports, err := ReadManagedTunnelPorts(host)
	if err != nil {
		return 0, err
	}
	return ports.RemotePort, nil
}

// ReadManagedTunnelPorts reads the RemoteForward listen port and target local
// port from the cc-clip managed block for the given host in ~/.ssh/config.
// Returns zero-value ports, nil if the host exists but has no managed block.
// Returns ErrManagedRemotePortInvalid when the managed block exists but does
// not contain a valid managed RemoteForward directive.
func ReadManagedTunnelPorts(host string) (ManagedTunnelPorts, error) {
	if err := validateHostAlias(host); err != nil {
		return ManagedTunnelPorts{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ManagedTunnelPorts{}, fmt.Errorf("cannot determine home directory: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		// Surface "no ssh config at all" as the same sentinel callers use
		// for "no Host block". A raw *PathError/ENOENT forces every caller
		// to branch on os.ErrNotExist in addition to ErrSSHHostBlockNotFound
		// to give actionable guidance; collapsing them here keeps
		// `errors.Is(err, ErrSSHHostBlockNotFound)` the single check.
		if errors.Is(err, os.ErrNotExist) {
			return ManagedTunnelPorts{}, fmt.Errorf("%w: %s (no ~/.ssh/config)", ErrSSHHostBlockNotFound, host)
		}
		return ManagedTunnelPorts{}, err
	}
	lines, _ := splitSSHConfigLines(data)
	block := findHostBlock(lines, host)
	if block == nil {
		return ManagedTunnelPorts{}, fmt.Errorf("%w: %s", ErrSSHHostBlockNotFound, host)
	}
	// Reject shared multi-pattern stanzas up front: cc-clip manages only
	// dedicated exact `Host <alias>` blocks. The returned error wraps both
	// ErrSharedHostStanza (for callers that want to branch on the specific
	// condition) and ErrManagedRemotePortInvalid (for backward compatibility
	// with existing callers that only look at the port-invalid sentinel).
	if !block.hasExactHostPattern(host) && block.positivePatternCount() > 1 {
		return ManagedTunnelPorts{}, fmt.Errorf("%w (%w): %s", ErrSharedHostStanza, ErrManagedRemotePortInvalid, host)
	}
	startMarker, endMarker := managedHostMarkers(host)
	if !block.hasExactHostPattern(host) {
		managed := findManagedRangeInBlock(lines, block, startMarker, endMarker)
		if managed.complete() || managed.partial() || managed.invalid() {
			// Wrap both sentinels so callers branching on either see the right
			// classification: the underlying issue is a shared host stanza,
			// and the managed block inside it is structurally invalid for our
			// purposes.
			return ManagedTunnelPorts{}, fmt.Errorf("%w (%w) for %s: shared Host stanzas are not supported", ErrSharedHostStanza, ErrManagedRemotePortInvalid, host)
		}
		return ManagedTunnelPorts{}, nil
	}
	managed := findManagedRangeInBlock(lines, block, startMarker, endMarker)
	if managed.invalid() {
		return ManagedTunnelPorts{}, fmt.Errorf("%w for %s: malformed managed block", ErrManagedRemotePortInvalid, host)
	}
	if managed.partial() {
		return ManagedTunnelPorts{}, fmt.Errorf("%w for %s: incomplete managed block", ErrManagedRemotePortInvalid, host)
	}
	if !managed.complete() {
		return ManagedTunnelPorts{}, nil
	}
	var ports []ManagedTunnelPorts
	for i := managed.start; i < managed.end; i++ {
		key, val := parseSSHDirective(lines[i])
		if strings.EqualFold(key, "RemoteForward") {
			parsed, ok := parseManagedTunnelPortsSpec(val)
			if !ok {
				return ManagedTunnelPorts{}, fmt.Errorf("%w for %s: %q", ErrManagedRemotePortInvalid, host, strings.TrimSpace(val))
			}
			ports = append(ports, parsed)
		}
	}
	switch len(ports) {
	case 0:
		return ManagedTunnelPorts{}, fmt.Errorf("%w for %s: missing RemoteForward", ErrManagedRemotePortInvalid, host)
	case 1:
		return ports[0], nil
	default:
		return ManagedTunnelPorts{}, fmt.Errorf("%w for %s: multiple RemoteForward directives", ErrManagedRemotePortInvalid, host)
	}
}
