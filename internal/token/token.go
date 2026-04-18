package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shunmei/cc-clip/internal/fileutil"
)

var (
	ErrTokenExpired       = errors.New("token expired")
	ErrTokenInvalid       = errors.New("token invalid")
	errOpaqueTokenInvalid = errors.New("opaque token invalid")

	// errTokenFileRegeneratable is the typed sentinel for "this token file is
	// missing, empty, or uses the legacy single-line format; generate a new
	// one instead of surfacing the read error". Using a sentinel (rather than
	// matching on the error string) keeps the "should we regenerate?" decision
	// robust against caller-side error wrapping and future message changes.
	errTokenFileRegeneratable = errors.New("token file regeneratable")
)

type Session struct {
	Token     string
	ExpiresAt time.Time
}

const tunnelControlTokenFileName = "tunnel-control.token"

type Manager struct {
	mu      sync.RWMutex
	session *Session
	ttl     time.Duration
}

func NewManager(ttl time.Duration) *Manager {
	return &Manager{ttl: ttl}
}

func (m *Manager) Generate() (Session, error) {
	tok, err := generateOpaqueToken()
	if err != nil {
		return Session{}, err
	}

	s := Session{
		Token:     tok,
		ExpiresAt: time.Now().Add(m.ttl),
	}

	m.mu.Lock()
	m.session = &s
	m.mu.Unlock()

	return s, nil
}

// LoadOrGenerate reads the existing token file and reuses it if still valid.
// If the file is missing, its contents are in the legacy single-line format,
// or the token has expired, a new token is generated. Real I/O failures
// (permission denied, EIO) are surfaced so a broken filesystem does not
// silently rotate the token out from under every synced peer.
func (m *Manager) LoadOrGenerate(ttl time.Duration) (Session, bool, error) {
	tok, expiresAt, err := ReadTokenFileWithExpiry()
	switch {
	case err == nil && time.Now().Before(expiresAt):
		s := Session{Token: tok, ExpiresAt: expiresAt}
		m.mu.Lock()
		m.session = &s
		m.ttl = ttl
		m.mu.Unlock()
		return s, true, nil
	case err == nil, errors.Is(err, os.ErrNotExist), isRegeneratableTokenFileError(err):
		// Expired, missing, or legacy/empty — generate fresh.
	default:
		return Session{}, false, fmt.Errorf("read session token: %w", err)
	}

	// Generate the raw token *outside* the lock so crypto/rand failures
	// surface fast, then install the new ttl and session atomically under
	// one write lock. Splitting "set ttl" and "set session" into two
	// separate lock acquisitions let a concurrent Validate observe the
	// new ttl against the stale session and extend the old token's expiry
	// using the new TTL — violating the sliding-window invariant.
	raw, genErr := generateOpaqueToken()
	if genErr != nil {
		return Session{}, false, genErr
	}
	s := Session{
		Token:     raw,
		ExpiresAt: time.Now().Add(ttl),
	}
	m.mu.Lock()
	m.ttl = ttl
	m.session = &s
	m.mu.Unlock()
	return s, false, nil
}

// isRegeneratableTokenFileError reports whether err from ReadTokenFileWithExpiry
// indicates a missing/malformed token file rather than a real I/O failure.
// Uses errors.Is against a typed sentinel so future callers that wrap the
// error (or switch the phrasing of the underlying message) cannot silently
// bypass the "regenerate vs propagate" decision.
func isRegeneratableTokenFileError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errTokenFileRegeneratable)
}

// IsOpaqueTokenInvalid reports whether err indicates a malformed local-only
// opaque token file (for example, empty, multi-line, wrong length, or
// non-hex). Callers can use this to decide whether to regenerate the token or
// treat the file as effectively missing.
func IsOpaqueTokenInvalid(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errOpaqueTokenInvalid)
}

// ConstantTimeEqual compares arbitrary strings by hashing both inputs to
// fixed-size digests before ConstantTimeCompare. This avoids the compare
// function's length-mismatch fast path while keeping the final equality check
// on a constant-size buffer. The hashing work still scales with input length,
// so this is a narrow token/nonce helper, not a blanket constant-time claim
// for unbounded attacker-controlled input.
func ConstantTimeEqual(got, want string) bool {
	gotDigest := sha256.Sum256([]byte(got))
	wantDigest := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotDigest[:], wantDigest[:]) == 1
}

func (m *Manager) Validate(tok string) error {
	m.mu.Lock()
	if m.session == nil {
		m.mu.Unlock()
		return ErrTokenInvalid
	}
	if time.Now().After(m.session.ExpiresAt) {
		m.mu.Unlock()
		return ErrTokenExpired
	}
	if !ConstantTimeEqual(strings.TrimSpace(tok), m.session.Token) {
		m.mu.Unlock()
		return ErrTokenInvalid
	}
	var (
		persistToken string
		persistExp   time.Time
	)
	if m.ttl > 0 && time.Until(m.session.ExpiresAt) < m.ttl/2 {
		m.session.ExpiresAt = time.Now().Add(m.ttl)
		persistToken = m.session.Token
		persistExp = m.session.ExpiresAt
	}
	m.mu.Unlock()

	if persistToken != "" {
		_, _ = WriteTokenFile(persistToken, persistExp)
	}
	return nil
}

func (m *Manager) Current() *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.session == nil {
		return nil
	}
	cp := *m.session
	return &cp
}

// TokenDirOverride allows overriding the token directory (for testing).
// When empty, the default ~/.cache/cc-clip is used.
var TokenDirOverride string

func TokenDir() (string, error) {
	if TokenDirOverride != "" {
		return TokenDirOverride, ensureTokenDirMode(TokenDirOverride)
	}
	// CC_CLIP_STATE_DIR is set by the shell entry script on the remote so that
	// peer-scoped processes (x11-bridge, cc-clip notify) resolve tokens from
	// their own state directory. It is never set on the local machine.
	if env := os.Getenv("CC_CLIP_STATE_DIR"); env != "" {
		return env, ensureTokenDirMode(env)
	}
	if env := os.Getenv("CC_CLIP_TOKEN_DIR"); env != "" {
		return env, ensureTokenDirMode(env)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".cache", "cc-clip")
	return dir, ensureTokenDirMode(dir)
}

// ensureTokenDirMode ensures dir exists *and* that its mode is 0700 when
// it was wider than 0700. MkdirAll alone does not tighten permissions on a
// pre-existing directory — if `~/.cache` (or the token dir itself) was
// created at 0755 by another tool the token files' *names* would be
// world-listable even though the files themselves are 0600. Chmoding after
// MkdirAll closes that gap.
//
// A pre-existing directory whose mode is already ≤ 0700 (i.e. no group or
// world bits set) is left alone. This matters for users who point
// CC_CLIP_TOKEN_DIR / CC_CLIP_STATE_DIR at a directory they share with
// other tools: the earlier behavior unconditionally re-applied 0700 on
// every call, which could surprise users who intentionally set a tighter
// mode (e.g. 0500 for read-only run) or who expected cc-clip not to touch
// dir metadata they had configured themselves. When the mode is wider, we
// log a warning before tightening so the state change is observable.
//
// Before chmoding, Lstat the path and refuse if it is a symlink. os.Chmod
// follows symlinks, so a pre-planted `~/.cache/cc-clip -> /tmp/evil` would
// otherwise cause cc-clip to restrict `/tmp/evil` to 0700 on startup — a
// small but real footgun if the user has another tool relying on that
// target's mode. The documented product boundary is that the token dir is
// a regular directory owned by the current user; this check enforces that.
func ensureTokenDirMode(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("token directory %s is a symlink; cc-clip requires a regular directory", dir)
	}
	// Only tighten if group or world bits are set. os.Chmod on Windows is
	// effectively a no-op on these bits, so the branch is harmless there.
	if info.Mode().Perm()&0077 != 0 {
		log.Printf("cc-clip: tightening %s permissions to 0700 (was %#o)", dir, info.Mode().Perm())
		if err := os.Chmod(dir, 0700); err != nil {
			return fmt.Errorf("chmod %s 0700: %w", dir, err)
		}
	}
	return nil
}

func generateOpaqueToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type tokenDirFn func() (string, error)

func writeOpaqueTokenFile(dirFn tokenDirFn, fileName, tok string) (string, error) {
	dir, err := dirFn()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, fileName)
	if err := writeTokenFileAtomic(path, []byte(strings.TrimSpace(tok)+"\n"), 0600); err != nil {
		return "", err
	}
	return path, nil
}

// writeTokenFileAtomic writes content to path via a tempfile-then-rename so a
// reader never sees a partially-written file, and explicitly chmods to perm
// before the first byte is written so a platform where CreateTemp honors a
// loose umask (0666) never has the secret bytes on disk at a wider mode —
// even for a few microseconds.
func writeTokenFileAtomic(path string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := fileutil.RenameReplace(tmpName, path); err != nil {
		return err
	}
	// Explicitly reapply the mode after rename. On platforms where
	// os.CreateTemp honors a loose umask and the rename inherits the
	// tmp file's metadata, this is belt-and-suspenders; it also survives
	// a hand-placed token file that landed at 0644 before cc-clip ever ran.
	// Surface chmod failures as hard errors — if we cannot guarantee 0600
	// after the rename, we must not report the write as a success: a token
	// that ended up at 0644 is world-readable to every local user, and
	// log-only warnings are silently discarded in many deployments.
	if err := os.Chmod(path, perm); err != nil {
		return fmt.Errorf("chmod %o %s: %w", perm, path, err)
	}
	// fsync the containing directory so the rename is durable across a
	// crash. Without this, the new token may not hit disk and the previous
	// token could resurrect on the next boot, causing a silent mismatch
	// against synced peers. Best-effort: on platforms where a dir cannot
	// be opened for sync (e.g. Windows), log-and-continue.
	if dirFile, dirErr := os.Open(dir); dirErr == nil {
		if syncErr := dirFile.Sync(); syncErr != nil {
			log.Printf("cc-clip: warning: fsync %s failed after token write: %v", dir, syncErr)
		}
		_ = dirFile.Close()
	}
	success = true
	return nil
}

func readOpaqueTokenFile(dirFn tokenDirFn, fileName string) (string, error) {
	dir, err := dirFn()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, fileName)
	permissive := false
	if info, statErr := os.Stat(path); statErr == nil {
		if leakedMode := info.Mode().Perm() &^ 0600; leakedMode != 0 {
			// On first upgrade from an older cc-clip build — or after a
			// hand-placed token file landed at 0644 — the permissions may be
			// wider than 0600. Warn loudly so the leak is visible in daemon
			// logs, and flag for tightening after a successful read so the
			// next reader no longer trips this branch.
			log.Printf("cc-clip: warning: token file %s has permissive mode %o — tightening to 0600", path, info.Mode().Perm())
			permissive = true
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(data))
	if strings.ContainsAny(tok, "\r\n") {
		return "", fmt.Errorf("%w: %s must contain exactly one line", errOpaqueTokenInvalid, fileName)
	}
	if tok == "" {
		return "", fmt.Errorf("%w: %s is empty", errOpaqueTokenInvalid, fileName)
	}
	if len(tok) != 64 {
		return "", fmt.Errorf("%w: %s has invalid length %d", errOpaqueTokenInvalid, fileName, len(tok))
	}
	if _, err := hex.DecodeString(tok); err != nil {
		return "", fmt.Errorf("%w: %s is not valid hex", errOpaqueTokenInvalid, fileName)
	}
	if permissive {
		// Warning alone is not enough — a token that stays at 0644 across
		// restarts stays leaked to every local reader. Re-chmod in place so
		// the next call sees a tightened mode. Errors are logged but not
		// fatal: the secret was already on disk at the wider mode before
		// this function ran, so refusing the read changes nothing.
		if chmodErr := os.Chmod(path, 0600); chmodErr != nil {
			log.Printf("cc-clip: warning: failed to tighten %s to 0600: %v", path, chmodErr)
		}
	}
	return tok, nil
}

func loadOrGenerateOpaqueTokenFile(dirFn tokenDirFn, fileName string) (string, bool, error) {
	tok, err := readOpaqueTokenFile(dirFn, fileName)
	if err == nil {
		return tok, true, nil
	}
	if err != nil && !os.IsNotExist(err) && !errors.Is(err, errOpaqueTokenInvalid) {
		return "", false, err
	}

	tok, err = generateOpaqueToken()
	if err != nil {
		return "", false, err
	}
	if _, err := writeOpaqueTokenFile(dirFn, fileName, tok); err != nil {
		return "", false, err
	}
	return tok, false, nil
}

// localOnlyTokenDir returns the local-only token directory. Unlike TokenDir,
// it never honors CC_CLIP_STATE_DIR / CC_CLIP_TOKEN_DIR — secrets written
// via this resolver must not appear in directories synced to remote peers.
// TokenDirOverride (test hook) is honored as-is since tests already isolate
// it to t.TempDir().
func localOnlyTokenDir() (string, error) {
	if TokenDirOverride != "" {
		return TokenDirOverride, ensureTokenDirMode(TokenDirOverride)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".cache", "cc-clip")
	return dir, ensureTokenDirMode(dir)
}

// WriteTokenFile writes a two-line token file: line 1 = token, line 2 = ISO8601 expiry.
func WriteTokenFile(tok string, expiresAt time.Time) (string, error) {
	dir, err := TokenDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "session.token")
	content := tok + "\n" + expiresAt.Format(time.RFC3339) + "\n"
	if err := writeTokenFileAtomic(path, []byte(content), 0600); err != nil {
		return "", err
	}
	return path, nil
}

// LoadOrGenerateTunnelControlToken reads the local-only tunnel control token
// from disk, or creates a new one if it does not exist yet. The directory is
// resolved via localOnlyTokenDir, which refuses remote-peer env vars so the
// token cannot leak into a synced state directory.
func LoadOrGenerateTunnelControlToken() (string, bool, error) {
	return loadOrGenerateOpaqueTokenFile(localOnlyTokenDir, tunnelControlTokenFileName)
}

// ReadTunnelControlToken reads the local-only tunnel control token from disk.
func ReadTunnelControlToken() (string, error) {
	return readOpaqueTokenFile(localOnlyTokenDir, tunnelControlTokenFileName)
}

// RotateTunnelControlToken generates a fresh local-only tunnel control token
// using the same entropy/format as LoadOrGenerateTunnelControlToken and
// atomically writes it to the local-only token path with perm 0600. It works
// regardless of whether an existing token is present; any prior token is
// replaced. Returns the new token value.
func RotateTunnelControlToken() (string, error) {
	return rotateOpaqueTokenFile(localOnlyTokenDir, tunnelControlTokenFileName)
}

// rotateOpaqueTokenFile is the shared implementation for rotating an
// opaque local-only token file.
func rotateOpaqueTokenFile(dirFn tokenDirFn, fileName string) (string, error) {
	tok, err := generateOpaqueToken()
	if err != nil {
		return "", err
	}
	if _, err := writeOpaqueTokenFile(dirFn, fileName, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// ReadTokenFile reads the token string from the token file.
// It supports both the new two-line format and the old single-line format.
// For backward compatibility, only the token string is returned.
func ReadTokenFile() (string, error) {
	dir, err := TokenDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "session.token")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	if len(lines) == 0 || lines[0] == "" {
		return "", fmt.Errorf("token file is empty")
	}
	return strings.TrimSpace(lines[0]), nil
}

// ReadTokenFileWithExpiry reads both the token string and expiry from the token file.
// If the file uses the old single-line format (no expiry line), an error is returned
// so the caller treats it as expired and generates a new token.
func ReadTokenFileWithExpiry() (string, time.Time, error) {
	dir, err := TokenDir()
	if err != nil {
		return "", time.Time{}, err
	}
	path := filepath.Join(dir, "session.token")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, err
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	if len(lines) < 2 {
		// Old format (single line) — treat as expired so a new token is generated.
		return "", time.Time{}, fmt.Errorf("token file missing expiry (old format): %w", errTokenFileRegeneratable)
	}
	tok := strings.TrimSpace(lines[0])
	if tok == "" {
		return "", time.Time{}, fmt.Errorf("token file has empty token: %w", errTokenFileRegeneratable)
	}
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(lines[1]))
	if err != nil {
		return "", time.Time{}, errors.Join(
			errTokenFileRegeneratable,
			fmt.Errorf("invalid expiry timestamp: %w", err),
		)
	}
	return tok, expiresAt, nil
}
