package token

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGenerateAndValidate(t *testing.T) {
	m := NewManager(1 * time.Hour)

	s, err := m.Generate()
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if len(s.Token) != 64 {
		t.Fatalf("expected 64 char hex token, got %d", len(s.Token))
	}

	if err := m.Validate(s.Token); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
}

func TestValidateWrongToken(t *testing.T) {
	m := NewManager(1 * time.Hour)

	if _, err := m.Generate(); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	err := m.Validate("wrong-token")
	if err != ErrTokenInvalid {
		t.Fatalf("expected ErrTokenInvalid, got %v", err)
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !ConstantTimeEqual("same-token", "same-token") {
		t.Fatal("expected equal strings to match")
	}
	if ConstantTimeEqual("same-token", "other-token") {
		t.Fatal("expected different strings to mismatch")
	}
	if ConstantTimeEqual("short", "much-longer-token") {
		t.Fatal("expected different-length strings to mismatch")
	}
}

func TestValidateExpired(t *testing.T) {
	m := NewManager(1 * time.Millisecond)

	s, err := m.Generate()
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	time.Sleep(5 * time.Millisecond)

	err = m.Validate(s.Token)
	if err != ErrTokenExpired {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
}

func TestValidateNoSession(t *testing.T) {
	m := NewManager(1 * time.Hour)

	err := m.Validate("any-token")
	if err != ErrTokenInvalid {
		t.Fatalf("expected ErrTokenInvalid, got %v", err)
	}
}

func TestCurrent(t *testing.T) {
	m := NewManager(1 * time.Hour)

	if m.Current() != nil {
		t.Fatal("expected nil before Generate")
	}

	s, _ := m.Generate()
	cur := m.Current()
	if cur == nil {
		t.Fatal("expected non-nil after Generate")
	}
	if cur.Token != s.Token {
		t.Fatal("Current token mismatch")
	}
}

func TestWriteAndReadTokenFile(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	m := NewManager(1 * time.Hour)
	s, err := m.Generate()
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	path, err := WriteTokenFile(s.Token, s.ExpiresAt)
	if err != nil {
		t.Fatalf("WriteTokenFile failed: %v", err)
	}
	t.Logf("Token written to: %s", path)

	read, err := ReadTokenFile()
	if err != nil {
		t.Fatalf("ReadTokenFile failed: %v", err)
	}
	if read != s.Token {
		t.Fatalf("token mismatch: wrote %q, read %q", s.Token, read)
	}
}

func TestReadTokenFileWithExpiry(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	expiry := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	_, err := WriteTokenFile("test-token-abc", expiry)
	if err != nil {
		t.Fatalf("WriteTokenFile failed: %v", err)
	}

	tok, expiresAt, err := ReadTokenFileWithExpiry()
	if err != nil {
		t.Fatalf("ReadTokenFileWithExpiry failed: %v", err)
	}
	if tok != "test-token-abc" {
		t.Fatalf("token mismatch: expected %q, got %q", "test-token-abc", tok)
	}
	if !expiresAt.Equal(expiry) {
		t.Fatalf("expiry mismatch: expected %v, got %v", expiry, expiresAt)
	}
}

func TestReadTokenFileWithExpiry_OldFormat(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	// Simulate old format: single line, no expiry
	dir, err := TokenDir()
	if err != nil {
		t.Fatalf("TokenDir failed: %v", err)
	}
	path := filepath.Join(dir, "session.token")
	if err := os.WriteFile(path, []byte("old-format-token\n"), 0600); err != nil {
		t.Fatalf("write old format file failed: %v", err)
	}

	// ReadTokenFile should still work (backward compat)
	tok, err := ReadTokenFile()
	if err != nil {
		t.Fatalf("ReadTokenFile with old format failed: %v", err)
	}
	if tok != "old-format-token" {
		t.Fatalf("expected %q, got %q", "old-format-token", tok)
	}

	// ReadTokenFileWithExpiry should return error for old format
	_, _, err = ReadTokenFileWithExpiry()
	if err == nil {
		t.Fatal("expected error for old format token file, got nil")
	}
}

func TestLoadOrGenerateTunnelControlToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tmpDir := filepath.Join(home, ".cache", "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	first, reused, err := LoadOrGenerateTunnelControlToken()
	if err != nil {
		t.Fatalf("LoadOrGenerateTunnelControlToken: %v", err)
	}
	if reused {
		t.Fatal("expected first tunnel control token load to create a token")
	}
	if len(first) != 64 {
		t.Fatalf("token length = %d, want 64", len(first))
	}

	second, reused, err := LoadOrGenerateTunnelControlToken()
	if err != nil {
		t.Fatalf("LoadOrGenerateTunnelControlToken: %v", err)
	}
	if !reused {
		t.Fatal("expected second tunnel control token load to reuse existing token")
	}
	if second != first {
		t.Fatalf("token mismatch: first=%q second=%q", first, second)
	}

	read, err := ReadTunnelControlToken()
	if err != nil {
		t.Fatalf("ReadTunnelControlToken: %v", err)
	}
	if read != first {
		t.Fatalf("ReadTunnelControlToken = %q, want %q", read, first)
	}
}

// TestReadTunnelControlTokenTightensPermissiveMode pins the rescue path
// where a token file on disk is readable but world/group-accessible. A
// log-only warning would leave every subsequent read observing the same
// leak; the reader tightens to 0600 in place so the next caller sees the
// repaired mode.
func TestReadTunnelControlTokenTightensPermissiveMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows chmod semantics differ; Unix-only regression")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	tmpDir := filepath.Join(home, ".cache", "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	if _, _, err := LoadOrGenerateTunnelControlToken(); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	dir, err := TokenDir()
	if err != nil {
		t.Fatalf("TokenDir: %v", err)
	}
	path := filepath.Join(dir, tunnelControlTokenFileName)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod 0644: %v", err)
	}

	if _, err := ReadTunnelControlToken(); err != nil {
		t.Fatalf("ReadTunnelControlToken: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat after read: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("post-read perm = %o, want 0600", got)
	}
}

func TestLoadOrGenerateTunnelControlTokenRegeneratesInvalidFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tmpDir := filepath.Join(home, ".cache", "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	dir, err := TokenDir()
	if err != nil {
		t.Fatalf("TokenDir: %v", err)
	}
	path := filepath.Join(dir, tunnelControlTokenFileName)
	if err := os.WriteFile(path, []byte("\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tok, reused, err := LoadOrGenerateTunnelControlToken()
	if err != nil {
		t.Fatalf("LoadOrGenerateTunnelControlToken: %v", err)
	}
	if reused {
		t.Fatal("expected invalid tunnel control token file to be regenerated")
	}
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64", len(tok))
	}
}

func TestLoadOrGenerateTunnelControlTokenRejectsSymlinkedTokenDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	realDir := filepath.Join(home, "real-cache")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	linkDir := filepath.Join(home, "linked-cache")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("cannot create symlink on this platform: %v", err)
	}
	TokenDirOverride = linkDir
	defer func() { TokenDirOverride = "" }()

	if _, _, err := LoadOrGenerateTunnelControlToken(); err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("LoadOrGenerateTunnelControlToken error = %v, want symlink rejection", err)
	}
}

func TestLoadOrGenerate_ReusesValidToken(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	ttl := 1 * time.Hour

	// Write a valid token file
	expiry := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	_, err := WriteTokenFile("existing-valid-token", expiry)
	if err != nil {
		t.Fatalf("WriteTokenFile failed: %v", err)
	}

	m := NewManager(ttl)
	session, reused, err := m.LoadOrGenerate(ttl)
	if err != nil {
		t.Fatalf("LoadOrGenerate failed: %v", err)
	}
	if !reused {
		t.Fatal("expected token to be reused, but it was not")
	}
	if session.Token != "existing-valid-token" {
		t.Fatalf("expected reused token %q, got %q", "existing-valid-token", session.Token)
	}
	if !session.ExpiresAt.Equal(expiry) {
		t.Fatalf("expected expiry %v, got %v", expiry, session.ExpiresAt)
	}

	// Validate should accept the loaded token
	if err := m.Validate("existing-valid-token"); err != nil {
		t.Fatalf("Validate failed on reused token: %v", err)
	}
}

func TestLoadOrGenerate_ExpiredToken(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	ttl := 1 * time.Hour

	// Write an expired token file
	expiry := time.Now().Add(-1 * time.Minute)
	_, err := WriteTokenFile("expired-token", expiry)
	if err != nil {
		t.Fatalf("WriteTokenFile failed: %v", err)
	}

	m := NewManager(ttl)
	session, reused, err := m.LoadOrGenerate(ttl)
	if err != nil {
		t.Fatalf("LoadOrGenerate failed: %v", err)
	}
	if reused {
		t.Fatal("expected new token generation, but token was reused")
	}
	if session.Token == "expired-token" {
		t.Fatal("expected a different token, got the expired one")
	}
	if len(session.Token) != 64 {
		t.Fatalf("expected 64 char hex token, got %d chars", len(session.Token))
	}
}

func TestLoadOrGenerate_MissingFile(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	ttl := 1 * time.Hour

	// No token file exists
	m := NewManager(ttl)
	session, reused, err := m.LoadOrGenerate(ttl)
	if err != nil {
		t.Fatalf("LoadOrGenerate failed: %v", err)
	}
	if reused {
		t.Fatal("expected new token generation, but token was reused")
	}
	if len(session.Token) != 64 {
		t.Fatalf("expected 64 char hex token, got %d chars", len(session.Token))
	}
}

func TestLoadOrGenerate_OldFormatTreatedAsExpired(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	ttl := 1 * time.Hour

	// Write old format (single line, no expiry)
	dir, err := TokenDir()
	if err != nil {
		t.Fatalf("TokenDir failed: %v", err)
	}
	path := filepath.Join(dir, "session.token")
	if err := os.WriteFile(path, []byte("old-single-line-token\n"), 0600); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	m := NewManager(ttl)
	session, reused, err := m.LoadOrGenerate(ttl)
	if err != nil {
		t.Fatalf("LoadOrGenerate failed: %v", err)
	}
	if reused {
		t.Fatal("expected new token generation for old format, but token was reused")
	}
	if session.Token == "old-single-line-token" {
		t.Fatal("expected a different token, got the old one")
	}
	if len(session.Token) != 64 {
		t.Fatalf("expected 64 char hex token, got %d chars", len(session.Token))
	}
}

func TestValidateSlidingExpiration(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	ttl := 1 * time.Hour

	m := NewManager(ttl)
	s, err := m.Generate()
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	// Manually set expiry to just under half TTL remaining (should trigger renewal)
	m.mu.Lock()
	m.session.ExpiresAt = time.Now().Add(ttl/2 - 1*time.Minute)
	m.mu.Unlock()

	expiryBefore := m.Current().ExpiresAt

	if err := m.Validate(s.Token); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}

	expiryAfter := m.Current().ExpiresAt
	if !expiryAfter.After(expiryBefore) {
		t.Fatalf("expected expiry to be extended: before=%v, after=%v", expiryBefore, expiryAfter)
	}
	// The new expiry must land within [now+ttl-epsilon, now+ttl+epsilon];
	// a weaker "After(before)" assertion would pass even if the slide
	// extended expiry by seconds instead of the full ttl.
	wantLow := time.Now().Add(ttl - 5*time.Second)
	wantHigh := time.Now().Add(ttl + 5*time.Second)
	if expiryAfter.Before(wantLow) || expiryAfter.After(wantHigh) {
		t.Fatalf("expected expiry to land near now+ttl (=%v); got %v", ttl, expiryAfter.Sub(time.Now()))
	}

	// Verify token file was updated
	_, fileExpiry, err := ReadTokenFileWithExpiry()
	if err != nil {
		t.Fatalf("ReadTokenFileWithExpiry failed: %v", err)
	}
	if !fileExpiry.Equal(expiryAfter.Truncate(time.Second)) {
		t.Fatalf("token file expiry mismatch: expected %v, got %v", expiryAfter.Truncate(time.Second), fileExpiry)
	}
}

func TestValidateNoSlidingWhenFresh(t *testing.T) {
	m := NewManager(1 * time.Hour)
	s, err := m.Generate()
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	expiryBefore := m.Current().ExpiresAt

	if err := m.Validate(s.Token); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}

	expiryAfter := m.Current().ExpiresAt
	if !expiryAfter.Equal(expiryBefore) {
		t.Fatalf("expiry should not change when remaining > ttl/2: before=%v, after=%v", expiryBefore, expiryAfter)
	}
}

// TTL=0 is the "no sliding window" mode: the token is whatever Generate()
// stamped and Validate must never extend it. A latent off-by-one on the
// ttl-zero branch would otherwise treat "0 < time.Until(expiry) < ttl/2 ==
// 0" as a hit and slide the expiry into the past, which would later surface
// as mysterious TokenExpired errors for freshly generated sessions.
func TestValidateNoSlidingWhenTTLIsZero(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	m := NewManager(0)
	// Directly install a session that expires in the distant future so the
	// sliding-window math has every opportunity to misfire.
	s := Session{
		Token:     "zero-ttl-token",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	m.mu.Lock()
	m.session = &s
	m.mu.Unlock()

	expiryBefore := m.Current().ExpiresAt
	if err := m.Validate(s.Token); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	expiryAfter := m.Current().ExpiresAt
	if !expiryAfter.Equal(expiryBefore) {
		t.Fatalf("expiry must not slide when ttl=0: before=%v, after=%v", expiryBefore, expiryAfter)
	}
}

// A token file whose expiry line is corrupted should regenerate cleanly. This
// covers partial writes and old malformed files without pinning startup on a
// manual token reset.
func TestLoadOrGenerate_CorruptExpiryRegenerates(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	dir, err := TokenDir()
	if err != nil {
		t.Fatalf("TokenDir failed: %v", err)
	}
	path := filepath.Join(dir, "session.token")
	if err := os.WriteFile(path, []byte("a-valid-looking-token\nnot-a-timestamp\n"), 0600); err != nil {
		t.Fatalf("write corrupt expiry file: %v", err)
	}

	m := NewManager(1 * time.Hour)
	session, reused, err := m.LoadOrGenerate(1 * time.Hour)
	if err != nil {
		t.Fatalf("LoadOrGenerate with corrupt expiry: %v", err)
	}
	if reused {
		t.Fatal("reused = true, want false for corrupt expiry")
	}
	if session.Token == "a-valid-looking-token" {
		t.Fatal("expected corrupt expiry to trigger token regeneration")
	}
	if !session.ExpiresAt.After(time.Now()) {
		t.Fatalf("ExpiresAt = %v, want future expiry", session.ExpiresAt)
	}
}

func TestRotateToken_ForcesNewGeneration(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	ttl := 1 * time.Hour

	// Write a valid token file
	expiry := time.Now().Add(30 * time.Minute)
	_, err := WriteTokenFile("should-not-be-reused", expiry)
	if err != nil {
		t.Fatalf("WriteTokenFile failed: %v", err)
	}

	// Simulate --rotate-token: use Generate() directly instead of LoadOrGenerate()
	m := NewManager(ttl)
	session, err := m.Generate()
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if session.Token == "should-not-be-reused" {
		t.Fatal("expected a different token when rotating, got the existing one")
	}
	if len(session.Token) != 64 {
		t.Fatalf("expected 64 char hex token, got %d chars", len(session.Token))
	}
}

// F10: RotateTunnelControlToken generates and writes a new token regardless
// of whether an existing token is present.

func TestRotateTunnelControlTokenCreatesWhenMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tmpDir := filepath.Join(home, ".cache", "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	tok, err := RotateTunnelControlToken()
	if err != nil {
		t.Fatalf("RotateTunnelControlToken: %v", err)
	}
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64", len(tok))
	}

	onDisk, err := ReadTunnelControlToken()
	if err != nil {
		t.Fatalf("ReadTunnelControlToken: %v", err)
	}
	if onDisk != tok {
		t.Fatalf("on-disk token = %q, want %q", onDisk, tok)
	}

	path := filepath.Join(tmpDir, tunnelControlTokenFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token file perm = %o, want 0600", info.Mode().Perm())
	}
}

func TestRotateTunnelControlTokenReplacesExistingToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tmpDir := filepath.Join(home, ".cache", "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	first, _, err := LoadOrGenerateTunnelControlToken()
	if err != nil {
		t.Fatalf("LoadOrGenerateTunnelControlToken: %v", err)
	}

	rotated, err := RotateTunnelControlToken()
	if err != nil {
		t.Fatalf("RotateTunnelControlToken: %v", err)
	}
	if rotated == first {
		t.Fatal("expected rotated token to differ from previous token")
	}
	if len(rotated) != 64 {
		t.Fatalf("rotated token length = %d, want 64", len(rotated))
	}

	onDisk, err := ReadTunnelControlToken()
	if err != nil {
		t.Fatalf("ReadTunnelControlToken: %v", err)
	}
	if onDisk != rotated {
		t.Fatalf("on-disk token = %q, want rotated token %q", onDisk, rotated)
	}

	// Subsequent rotations keep producing distinct tokens.
	rotated2, err := RotateTunnelControlToken()
	if err != nil {
		t.Fatalf("RotateTunnelControlToken (second): %v", err)
	}
	if rotated2 == rotated || rotated2 == first {
		t.Fatalf("expected a fresh token, got %q (prev=%q first=%q)", rotated2, rotated, first)
	}
}

func TestLoadOrGenerateTunnelControlTokenRegeneratesMultilineFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tmpDir := filepath.Join(home, ".cache", "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	dir, err := TokenDir()
	if err != nil {
		t.Fatalf("TokenDir: %v", err)
	}
	path := filepath.Join(dir, tunnelControlTokenFileName)
	if err := os.WriteFile(path, []byte("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff\nextra\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tok, reused, err := LoadOrGenerateTunnelControlToken()
	if err != nil {
		t.Fatalf("LoadOrGenerateTunnelControlToken: %v", err)
	}
	if reused {
		t.Fatal("expected multiline tunnel control token file to be regenerated")
	}
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64", len(tok))
	}
	if tok == "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff" {
		t.Fatal("expected malformed multiline token to be replaced")
	}
}

func TestReadTunnelControlTokenRejectsInvalidLength(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tmpDir := filepath.Join(home, ".cache", "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	dir, err := TokenDir()
	if err != nil {
		t.Fatalf("TokenDir: %v", err)
	}
	path := filepath.Join(dir, tunnelControlTokenFileName)
	if err := os.WriteFile(path, []byte("short-token\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err = ReadTunnelControlToken()
	if !IsOpaqueTokenInvalid(err) {
		t.Fatalf("ReadTunnelControlToken err = %v, want invalid opaque token", err)
	}
}

// F12: writes must tighten pre-existing loose permissions on the token file.
// os.WriteFile leaves the mode of an existing file untouched, so rotating onto
// a file left at 0644 by a prior bug would silently keep the secret
// world-readable.

func TestWriteTokenFileTightensLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningfully enforced on Windows; os.Chmod clamps to 0666/0444")
	}
	tmpDir := filepath.Join(t.TempDir(), "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(tmpDir, "session.token")
	if err := os.WriteFile(path, []byte("old\n2026-01-01T00:00:00Z\n"), 0o644); err != nil {
		t.Fatalf("seed loose perms: %v", err)
	}

	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	if _, err := WriteTokenFile("new-token", expiry); err != nil {
		t.Fatalf("WriteTokenFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("session.token perm = %o after rewrite, want 0600", info.Mode().Perm())
	}
}

func TestRotateTunnelControlTokenTightensLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningfully enforced on Windows; os.Chmod clamps to 0666/0444")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	tmpDir := filepath.Join(home, ".cache", "cc-clip")
	TokenDirOverride = tmpDir
	defer func() { TokenDirOverride = "" }()

	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(tmpDir, tunnelControlTokenFileName)
	if err := os.WriteFile(path, []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("seed loose perms: %v", err)
	}

	if _, err := RotateTunnelControlToken(); err != nil {
		t.Fatalf("RotateTunnelControlToken: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("tunnel-control.token perm = %o after rotate, want 0600", info.Mode().Perm())
	}
}

func TestLocalOnlyTokenDirAllowsSubdirectoriesOfHomeCacheRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	nested := filepath.Join(home, ".cache", "cc-clip", "nested")
	TokenDirOverride = nested
	defer func() { TokenDirOverride = "" }()

	tok, _, err := LoadOrGenerateTunnelControlToken()
	if err != nil {
		t.Fatalf("LoadOrGenerateTunnelControlToken: %v", err)
	}
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64", len(tok))
	}
}

// TestWriteTokenFileSurfaceWriteErrors verifies that an unreachable token
// directory (parent read-only so MkdirAll fails, mimicking EROFS / tightened
// ACLs) surfaces an error rather than silently succeeding with no persisted
// token. If this path ever started swallowing write errors, callers expecting
// the token file to exist on disk would be left holding a session only
// present in memory.
func TestWriteTokenFileSurfaceWriteErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write-protection is not meaningfully enforced on Windows test envs")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	parent := t.TempDir()
	// Nest the target dir inside a read-only parent so TokenDir()'s
	// MkdirAll cannot create it.
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("Chmod parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	target := filepath.Join(parent, "cc-clip")
	TokenDirOverride = target
	defer func() { TokenDirOverride = "" }()

	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	_, err := WriteTokenFile("fresh", expiry)
	if err == nil {
		t.Fatal("WriteTokenFile = nil, want error when token dir cannot be created")
	}
}
